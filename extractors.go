package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/bodgit/sevenzip"
	"github.com/klauspost/compress/zstd"
	"github.com/nwaples/rardecode/v2"
	"github.com/snabb/httpreaderat"
)

type ArchiveType int

const (
	ArchiveUnknown ArchiveType = iota
	ArchiveRAR5
	ArchiveZIP
	Archive7Z
	ArchiveTarGz
	ArchiveTarZst
	ArchiveTar
)

type ArchiveFileEntry struct {
	Path string `json:"path"`
	Size uint64 `json:"size"`
}

type InspectResult struct {
	Format          string             `json:"format"`
	PartCount       int                `json:"part_count"`
	ArchiveSizeMB   float64            `json:"archive_size_mb"`
	ArchiveSizeGB   float64            `json:"archive_size_gb"`
	ExactUnpackedGB float64            `json:"exact_unpacked_gb"`
	EstUnpackedGB   float64            `json:"est_unpacked_gb"`
	IsExact         bool               `json:"is_exact"`
	FileCount       int                `json:"file_count"`
	Files           []ArchiveFileEntry `json:"files,omitempty"`
	FreeDiskGB      float64            `json:"free_disk_gb"`
	SafeToExtract   bool               `json:"safe_to_extract"`
	Note            string             `json:"note"`
}

var windowsReservedNames = map[string]bool{
	"CON":  true,
	"PRN":  true,
	"AUX":  true,
	"NUL":  true,
	"COM1": true,
	"COM2": true,
	"COM3": true,
	"COM4": true,
	"COM5": true,
	"COM6": true,
	"COM7": true,
	"COM8": true,
	"COM9": true,
	"LPT1": true,
	"LPT2": true,
	"LPT3": true,
	"LPT4": true,
	"LPT5": true,
	"LPT6": true,
	"LPT7": true,
	"LPT8": true,
	"LPT9": true,
}

var illegalFSCharsReplacer = strings.NewReplacer(
	":", "_",
	"?", "_",
	"*", "_",
	"\"", "_",
	"<", "_",
	">", "_",
	"|", "_",
)

// sanitizeSegment normalizes a single path segment for NTFS, exFAT, POSIX, and Windows compatibility.
func sanitizeSegment(seg string) string {
	var b strings.Builder
	b.Grow(len(seg))
	for i := 0; i < len(seg); i++ {
		ch := seg[i]
		if ch < 0x20 || ch == 0x7F {
			b.WriteByte('_')
		} else {
			switch ch {
			case ':', '?', '*', '"', '<', '>', '|':
				b.WriteByte('_')
			default:
				b.WriteByte(ch)
			}
		}
	}
	clean := strings.TrimRight(b.String(), " .")
	if clean == "" {
		return "_"
	}
	base := strings.ToUpper(clean)
	if dot := strings.IndexByte(base, '.'); dot != -1 {
		base = base[:dot]
	}
	if windowsReservedNames[base] {
		clean = "_" + clean
	}
	return clean
}

// sanitizeArchiveEntryPath converts an archive member path into a safe, cross-filesystem relative path.
func sanitizeArchiveEntryPath(relPath string) string {
	normalized := strings.ReplaceAll(relPath, "\\", "/")
	parts := strings.Split(normalized, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			return ""
		}
		out = append(out, sanitizeSegment(part))
	}
	if len(out) == 0 {
		return "."
	}
	return filepath.Join(out...)
}

// normalizeTargetPath handles Windows extended paths (>240 chars) with \\?\ prefix.
func normalizeTargetPath(p string) string {
	if runtime.GOOS == "windows" {
		clean := filepath.Clean(p)
		if len(clean) > 240 && !strings.HasPrefix(clean, `\\?\`) && filepath.IsAbs(clean) {
			return `\\?\` + clean
		}
		return clean
	}
	return p
}

func sanitizeFolderName(name string) string {
	clean := strings.TrimSpace(name)
	clean = strings.ReplaceAll(clean, "/", "_")
	clean = strings.ReplaceAll(clean, "\\", "_")
	clean = strings.ReplaceAll(clean, "..", "_")
	clean = illegalFSCharsReplacer.Replace(clean)
	clean = strings.TrimRight(clean, " .")
	if clean == "" || clean == "." {
		return "Extracted_Archive"
	}
	return clean
}

func deriveFallbackName(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "Extracted_Archive"
	}
	base := filepath.Base(u.Path)
	ext := filepath.Ext(base)
	clean := strings.TrimSuffix(base, ext)
	clean = strings.TrimSuffix(clean, ".part1")
	clean = strings.TrimSuffix(clean, ".part01")
	clean = strings.TrimSuffix(clean, ".tar")
	clean = strings.ReplaceAll(clean, ".", "_")
	return sanitizeFolderName(clean)
}

func parseURLList(raw string) []string {
	var urls []string
	for _, line := range strings.Split(raw, "\n") {
		clean := strings.TrimSpace(line)
		clean = strings.Trim(clean, "\r\n\"'")
		if clean != "" {
			urls = append(urls, clean)
		}
	}
	return urls
}

func compileExcludePatterns(raw string) []string {
	var patterns []string
	for _, part := range strings.Split(raw, ",") {
		p := strings.ToLower(strings.TrimSpace(part))
		if p != "" {
			patterns = append(patterns, p)
		}
	}
	return patterns
}

func buildSelectionSet(selected []string) map[string]bool {
	if len(selected) == 0 {
		return nil
	}
	set := make(map[string]bool, len(selected))
	for _, s := range selected {
		set[filepath.ToSlash(filepath.Clean(s))] = true
	}
	return set
}

func shouldSkipEntry(entryPath string, patterns []string, selectionSet map[string]bool, isDir bool) bool {
	norm := filepath.ToSlash(filepath.Clean(entryPath))

	// Check explicit selective file tree allow-list (for ZIP/7Z)
	if selectionSet != nil && !isDir {
		if !selectionSet[norm] {
			return true
		}
	}

	if len(patterns) == 0 {
		return false
	}
	lowerNorm := strings.ToLower(norm)
	base := strings.ToLower(filepath.Base(lowerNorm))
	for _, pat := range patterns {
		if matched, _ := filepath.Match(pat, base); matched {
			return true
		}
		if matched, _ := filepath.Match(pat, lowerNorm); matched {
			return true
		}
		cleanSub := strings.Trim(pat, "*")
		if cleanSub != "" && strings.Contains(lowerNorm, cleanSub) {
			return true
		}
	}
	return false
}

func resolveConflictDirectory(baseDir, folderName, policy string) (string, error) {
	target := filepath.Join(baseDir, sanitizeFolderName(folderName))
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) == 0 {
		return target, nil
	}

	switch policy {
	case "overwrite":
		if err := os.RemoveAll(target); err != nil {
			return target, fmt.Errorf("failed to clean existing directory: %w", err)
		}
		return target, nil
	case "rename":
		for i := 2; i < 100; i++ {
			candidate := fmt.Sprintf("%s_%d", target, i)
			if _, err := os.Stat(candidate); os.IsNotExist(err) {
				return candidate, nil
			}
		}
		return target, nil
	default: // "merge"
		return target, nil
	}
}

func detectFormat(header []byte, sampleURL string) ArchiveType {
	if len(header) >= 7 && bytes.Equal(header[:7], []byte{0x52, 0x61, 0x72, 0x21, 0x1A, 0x07, 0x01}) {
		return ArchiveRAR5
	}
	if len(header) >= 6 && bytes.Equal(header[:6], []byte{0x52, 0x61, 0x72, 0x21, 0x1A, 0x07}) {
		return ArchiveRAR5
	}
	if len(header) >= 4 && bytes.Equal(header[:4], []byte{0x50, 0x4B, 0x03, 0x04}) {
		return ArchiveZIP
	}
	if len(header) >= 6 && bytes.Equal(header[:6], []byte{0x37, 0x7A, 0xBC, 0xAF, 0x27, 0x1C}) {
		return Archive7Z
	}
	if len(header) >= 4 && bytes.Equal(header[:4], []byte{0x28, 0xB5, 0x2F, 0xFD}) {
		return ArchiveTarZst
	}
	if len(header) >= 2 && header[0] == 0x1F && header[1] == 0x8B {
		return ArchiveTarGz
	}
	lower := strings.ToLower(sampleURL)
	if strings.HasSuffix(lower, ".tar") {
		return ArchiveTar
	}
	return ArchiveUnknown
}

func inspectRemoteArchive(ctx context.Context, urls []string, outputDir string, headers map[string]string) (*InspectResult, error) {
	freeBytes, err := getFreeDiskSpace(outputDir)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect storage: %w", err)
	}
	freeDiskGB := float64(freeBytes) / (1024 * 1024 * 1024)

	client := createOptimizedHTTPClient(30 * time.Second)
	var totalArchiveBytes int64
	var detected ArchiveType
	var firstResolvedURL string
	var firstContentLen int64

	for idx, u := range urls {
		resp, resolved, contentLen, err := probeRemoteURL(ctx, client, u, headers)
		if err != nil {
			return nil, fmt.Errorf("part %d unreachable: %w", idx+1, err)
		}

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone {
				return nil, fmt.Errorf("Download link expired or blocked by host. Refresh the link and click Retry to resume.")
			}
			return nil, fmt.Errorf("part %d returned HTTP %s", idx+1, resp.Status)
		}

		if contentLen > 0 {
			totalArchiveBytes += contentLen
		}

		if idx == 0 {
			firstContentLen = contentLen
			firstResolvedURL = resolved
			magicBuffer := make([]byte, 16)
			var n int
			var probeRespHeader = resp.Header
			if resp.Request != nil && resp.Request.Method == "HEAD" {
				_ = resp.Body.Close()
				getReq, err := http.NewRequestWithContext(ctx, "GET", firstResolvedURL, nil)
				if err == nil {
					applyCustomHeaders(getReq, headers)
					getReq.Header.Set("Range", "bytes=0-15")
					if getResp, err := client.Do(getReq); err == nil {
						probeRespHeader = getResp.Header
						n, _ = io.ReadFull(getResp.Body, magicBuffer)
						_ = getResp.Body.Close()
					}
				}
			} else {
				n, _ = io.ReadFull(resp.Body, magicBuffer)
				_ = resp.Body.Close()
			}

			contentType := probeRespHeader.Get("Content-Type")
			isHTML := strings.Contains(strings.ToLower(contentType), "text/html") ||
				bytes.HasPrefix(bytes.TrimSpace(magicBuffer[:n]), []byte("<!DOCTYPE")) ||
				bytes.HasPrefix(bytes.TrimSpace(magicBuffer[:n]), []byte("<html")) ||
				bytes.HasPrefix(bytes.TrimSpace(magicBuffer[:n]), []byte("<HTML"))
			if isHTML {
				return nil, fmt.Errorf("This link opened a webpage instead of a direct archive file. Copy the direct download link and try again.")
			}

			detected = detectFormat(magicBuffer[:n], u)
			if detected == ArchiveUnknown {
				return nil, fmt.Errorf("Unsupported archive format. Supported formats: RAR5, ZIP, 7Z, TAR.GZ, TAR.ZST.")
			}
		} else {
			_ = resp.Body.Close()
		}
	}

	archiveMB := float64(totalArchiveBytes) / (1024 * 1024)
	archiveGB := archiveMB / 1024.0

	result := &InspectResult{
		PartCount:     len(urls),
		ArchiveSizeMB: archiveMB,
		ArchiveSizeGB: archiveGB,
		FreeDiskGB:    freeDiskGB,
	}

	if detected == ArchiveZIP && firstContentLen > 0 && len(urls) == 1 {
		result.Format = "ZIP"
		rangeReq, err := http.NewRequestWithContext(ctx, "GET", firstResolvedURL, nil)
		if err == nil {
			applyCustomHeaders(rangeReq, headers)
			if r, err := httpreaderat.New(client, rangeReq, nil); err == nil {
				if zr, err := zip.NewReader(r, firstContentLen); err == nil {
					var exactBytes uint64
					var entries []ArchiveFileEntry
					for _, f := range zr.File {
						exactBytes += f.UncompressedSize64
						if !f.FileInfo().IsDir() {
							result.FileCount++
							entries = append(entries, ArchiveFileEntry{
								Path: f.Name,
								Size: f.UncompressedSize64,
							})
						}
					}
					exactGB := float64(exactBytes) / (1024 * 1024 * 1024)
					result.ExactUnpackedGB = exactGB
					result.EstUnpackedGB = exactGB
					result.IsExact = true
					result.Files = entries
					result.SafeToExtract = (exactGB + safetyBufferGB) <= freeDiskGB
					result.Note = fmt.Sprintf("Central Directory verified (%d files). Uncheck unwanted files below before unpacking.", result.FileCount)
					return result, nil
				}
			}
		}
	}

	if detected == Archive7Z && firstContentLen > 0 && len(urls) == 1 {
		result.Format = "7Z"
		rangeReq, err := http.NewRequestWithContext(ctx, "GET", firstResolvedURL, nil)
		if err == nil {
			applyCustomHeaders(rangeReq, headers)
			if r, err := httpreaderat.New(client, rangeReq, nil); err == nil {
				if szr, err := sevenzip.NewReader(r, firstContentLen); err == nil {
					var exactBytes uint64
					var entries []ArchiveFileEntry
					for _, f := range szr.File {
						exactBytes += f.UncompressedSize
						if !f.FileInfo().IsDir() {
							result.FileCount++
							entries = append(entries, ArchiveFileEntry{
								Path: f.Name,
								Size: f.UncompressedSize,
							})
						}
					}
					exactGB := float64(exactBytes) / (1024 * 1024 * 1024)
					result.ExactUnpackedGB = exactGB
					result.EstUnpackedGB = exactGB
					result.IsExact = true
					result.Files = entries
					result.SafeToExtract = (exactGB + safetyBufferGB) <= freeDiskGB
					result.Note = fmt.Sprintf("7z Header verified (%d files). Uncheck unwanted files below before unpacking.", result.FileCount)
					return result, nil
				}
			}
		}
	}

	switch detected {
	case ArchiveRAR5:
		result.Format = "RAR5"
		if len(urls) > 1 {
			result.Format = fmt.Sprintf("RAR5 (%d Parts)", len(urls))
		}
		result.EstUnpackedGB = archiveGB * 1.6
		result.SafeToExtract = (archiveGB + safetyBufferGB) <= freeDiskGB
		result.Note = "Sequential RAR stream: dynamic free-space check runs before every file write."
	case ArchiveZIP:
		result.Format = "ZIP"
		result.EstUnpackedGB = archiveGB * 1.4
		result.SafeToExtract = (result.EstUnpackedGB + safetyBufferGB) <= freeDiskGB
		result.Note = "ZIP archive detected."
	case Archive7Z:
		result.Format = "7Z"
		result.EstUnpackedGB = archiveGB * 1.8
		result.SafeToExtract = (result.EstUnpackedGB + safetyBufferGB) <= freeDiskGB
		result.Note = "7-Zip archive detected."
	case ArchiveTarZst:
		result.Format = "TAR.ZST"
		result.EstUnpackedGB = archiveGB * 2.2
		result.SafeToExtract = (archiveGB + safetyBufferGB) <= freeDiskGB
		result.Note = "Zstandard compressed tar stream."
	case ArchiveTarGz:
		result.Format = "TAR.GZ"
		result.EstUnpackedGB = archiveGB * 2.0
		result.SafeToExtract = (archiveGB + safetyBufferGB) <= freeDiskGB
		result.Note = "Gzip compressed tar stream."
	default:
		result.Format = "Unknown"
		result.SafeToExtract = false
		result.Note = "Unsupported format. Supported: RAR5, ZIP, 7Z, TAR.GZ, TAR.ZST."
	}

	return result, nil
}

func verifyChecksums(dir string) string {
	var manifestPath string
	var isSHA256 bool

	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		lower := strings.ToLower(d.Name())
		if strings.HasSuffix(lower, ".sha256") || lower == "sha256sums.txt" {
			manifestPath = path
			isSHA256 = true
			return filepath.SkipAll
		}
		if strings.HasSuffix(lower, ".md5") || lower == "md5sums.txt" || lower == "checksums.md5" {
			manifestPath = path
			isSHA256 = false
			return filepath.SkipAll
		}
		return nil
	})

	if manifestPath == "" {
		return "No checksum manifest found (Skipped)"
	}

	f, err := os.Open(manifestPath)
	if err != nil {
		return "Checksum manifest unreadable"
	}
	defer f.Close()

	baseDir := filepath.Dir(manifestPath)
	scanner := bufio.NewScanner(f)
	var verifiedCount, failedCount int

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		expectedHash := strings.ToLower(fields[0])
		relPath := strings.TrimPrefix(fields[1], "*")
		relPath = strings.ReplaceAll(relPath, "\\", "/")
		fullPath := filepath.Join(baseDir, filepath.Clean(relPath))

		targetFile, err := os.Open(fullPath)
		if err != nil {
			failedCount++
			continue
		}

		var h hash.Hash
		if isSHA256 {
			h = sha256.New()
		} else {
			h = md5.New()
		}

		_, err = io.Copy(h, targetFile)
		_ = targetFile.Close()
		if err != nil {
			failedCount++
			continue
		}

		actualHash := hex.EncodeToString(h.Sum(nil))
		if actualHash == expectedHash {
			verifiedCount++
		} else {
			failedCount++
		}
	}

	if failedCount > 0 {
		log.Printf("[CHECKSUM:MISMATCH] Manifest %s: %d file(s) mismatched, %d passed", filepath.Base(manifestPath), failedCount, verifiedCount)
		return fmt.Sprintf("Failed: %d file(s) mismatched (%d passed)", failedCount, verifiedCount)
	}
	if verifiedCount > 0 {
		log.Printf("[CHECKSUM:OK] Manifest %s: %d file(s) verified successfully", filepath.Base(manifestPath), verifiedCount)
		return fmt.Sprintf("Verified: %d manifest file(s) passed", verifiedCount)
	}
	return "Manifest contained zero valid entries"
}

func isEncryptionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "password") || strings.Contains(msg, "encrypt") || strings.Contains(msg, "bad header")
}

var errPasswordRequired = errors.New("archive requires a password")

func extractRAR(ctx context.Context, r io.Reader, baseOutputDir string, autoSubfolder bool, customFolder string, conflictPolicy string, fallbackName string, password string, bufferSizeMB int, excludePatterns []string) (count int, skipped int, totalUnpacked int64, actualOutputDir string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("RAR decompression panic trapped: %v", rec)
		}
	}()
	actualOutputDir = baseOutputDir
	var opts []rardecode.Option
	if password != "" {
		opts = append(opts, rardecode.Password(password))
	}

	rr, err := rardecode.NewReader(r, opts...)
	if err != nil {
		if isEncryptionError(err) {
			return 0, 0, 0, baseOutputDir, errPasswordRequired
		}
		return 0, 0, 0, baseOutputDir, fmt.Errorf("failed to initialize RAR engine: %w", err)
	}

	buf := acquireBuffer(bufferSizeMB)
	defer releaseBuffer(bufferSizeMB, buf)

	actualOutputDir = baseOutputDir
	var determinedSubfolder = false

	if customFolder != "" {
		resolved, err := resolveConflictDirectory(baseOutputDir, customFolder, conflictPolicy)
		if err != nil {
			return 0, 0, 0, baseOutputDir, err
		}
		actualOutputDir = resolved
		manager.SetCreatedRoot(actualOutputDir)
		determinedSubfolder = true
	} else if !autoSubfolder {
		determinedSubfolder = true
	}

	for {
		if ctx.Err() != nil {
			return count, skipped, totalUnpacked, actualOutputDir, ctx.Err()
		}

		header, err := rr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				return count, skipped, totalUnpacked, actualOutputDir, ctx.Err()
			}
			if isEncryptionError(err) {
				return count, skipped, totalUnpacked, actualOutputDir, errPasswordRequired
			}
			return count, skipped, totalUnpacked, actualOutputDir, fmt.Errorf("archive read error: %w", err)
		}

		if header.Mode()&os.ModeSymlink != 0 {
			continue
		}

		cleanEntry := sanitizeArchiveEntryPath(header.Name)
		if cleanEntry == "" || cleanEntry == "." {
			continue
		}

		if !determinedSubfolder {
			parts := strings.Split(cleanEntry, string(filepath.Separator))
			if len(parts) <= 1 && !header.IsDir {
				resolved, err := resolveConflictDirectory(baseOutputDir, fallbackName, conflictPolicy)
				if err != nil {
					return 0, 0, 0, baseOutputDir, err
				}
				actualOutputDir = resolved
				manager.SetCreatedRoot(actualOutputDir)
			} else if len(parts) > 0 {
				manager.SetCreatedRoot(filepath.Join(baseOutputDir, parts[0]))
			}
			determinedSubfolder = true
		}

		if shouldSkipEntry(header.Name, excludePatterns, nil, header.IsDir) {
			if !header.IsDir {
				skipped++
				manager.Update(func(s *ExtractionState) {
					s.SkippedFiles = skipped
				})
			}
			continue
		}

		targetPath := filepath.Join(actualOutputDir, cleanEntry)
		cleanActual := filepath.Clean(actualOutputDir)
		if targetPath != cleanActual && !strings.HasPrefix(filepath.Clean(targetPath), cleanActual+string(filepath.Separator)) {
			continue
		}

		if header.IsDir {
			_ = os.MkdirAll(targetPath, 0755)
			continue
		}

		freeBytes, diskErr := getFreeDiskSpace(actualOutputDir)
		if diskErr == nil {
			minFloorBytes := uint64(midStreamMinMB) * 1024 * 1024
			neededBytes := uint64(header.UnPackedSize) + minFloorBytes
			if freeBytes < neededBytes {
				return count, skipped, totalUnpacked, actualOutputDir, fmt.Errorf("mid-stream storage halt: '%s' requires %.2f MB, only %.2f MB free", header.Name, float64(header.UnPackedSize)/(1024*1024), float64(freeBytes)/(1024*1024))
			}
			manager.Update(func(s *ExtractionState) {
				s.FreeDiskGB = float64(freeBytes) / (1024 * 1024 * 1024)
			})
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return count, skipped, totalUnpacked, actualOutputDir, err
		}

		_ = os.Chmod(targetPath, 0644)
		normPath := normalizeTargetPath(targetPath)
		outFile, err := os.OpenFile(normPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			return count, skipped, totalUnpacked, actualOutputDir, err
		}

		manager.SetActiveFile(outFile, targetPath)
		manager.Update(func(s *ExtractionState) {
			s.CurrentFile = header.Name
			s.FinalDest = actualOutputDir
		})

		bufWriter := bufio.NewWriterSize(outFile, 256*1024)
		written, err := io.CopyBuffer(bufWriter, rr, buf)
		flushErr := bufWriter.Flush()
		_ = outFile.Close()
		manager.ClearActiveFile()
		if err == nil && flushErr != nil {
			err = flushErr
		}

		if err != nil {
			_ = os.Remove(targetPath)
			if ctx.Err() != nil {
				return count, skipped, totalUnpacked, actualOutputDir, ctx.Err()
			}
			if isEncryptionError(err) {
				return count, skipped, totalUnpacked, actualOutputDir, errPasswordRequired
			}
			if isDiskSpaceOrPermissionError(err) {
				if waitErr := manager.WaitForDiskSpace(ctx, actualOutputDir, err); waitErr != nil {
					return count, skipped, totalUnpacked, actualOutputDir, waitErr
				}
			}
			return count, skipped, totalUnpacked, actualOutputDir, fmt.Errorf("write error for %s: %w", header.Name, err)
		}

		totalUnpacked += written
		count++
		manager.Update(func(s *ExtractionState) {
			s.ExtractedFiles = count
			s.UncompressedMB = float64(totalUnpacked) / (1024 * 1024)
			s.UncompressedGB = float64(totalUnpacked) / (1024 * 1024 * 1024)
		})
	}
	return count, skipped, totalUnpacked, actualOutputDir, nil
}

func extractTarStream(ctx context.Context, r io.Reader, archiveType ArchiveType, baseOutputDir string, autoSubfolder bool, customFolder string, conflictPolicy string, fallbackName string, bufferSizeMB int, excludePatterns []string) (count int, skipped int, totalUnpacked int64, actualOutputDir string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("TAR decompression panic trapped: %v", rec)
		}
	}()
	actualOutputDir = baseOutputDir
	var tarInput io.Reader = r

	if archiveType == ArchiveTarGz {
		gzr, err := gzip.NewReader(r)
		if err != nil {
			return 0, 0, 0, baseOutputDir, fmt.Errorf("failed to initialize gzip stream: %w", err)
		}
		defer gzr.Close()
		tarInput = gzr
	} else if archiveType == ArchiveTarZst {
		zr, err := zstd.NewReader(r)
		if err != nil {
			return 0, 0, 0, baseOutputDir, fmt.Errorf("failed to initialize zstd stream: %w", err)
		}
		defer zr.Close()
		tarInput = zr
	}

	tr := tar.NewReader(tarInput)
	buf := acquireBuffer(bufferSizeMB)
	defer releaseBuffer(bufferSizeMB, buf)

	actualOutputDir = baseOutputDir
	var determinedSubfolder = false

	if customFolder != "" {
		resolved, err := resolveConflictDirectory(baseOutputDir, customFolder, conflictPolicy)
		if err != nil {
			return 0, 0, 0, baseOutputDir, err
		}
		actualOutputDir = resolved
		manager.SetCreatedRoot(actualOutputDir)
		determinedSubfolder = true
	} else if !autoSubfolder {
		determinedSubfolder = true
	}

	for {
		if ctx.Err() != nil {
			return count, skipped, totalUnpacked, actualOutputDir, ctx.Err()
		}

		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return count, skipped, totalUnpacked, actualOutputDir, fmt.Errorf("tar stream read error: %w", err)
		}

		if header.Typeflag == tar.TypeSymlink || header.Typeflag == tar.TypeLink {
			continue
		}

		cleanEntry := sanitizeArchiveEntryPath(header.Name)
		if cleanEntry == "" || cleanEntry == "." {
			continue
		}

		if !determinedSubfolder {
			parts := strings.Split(cleanEntry, string(filepath.Separator))
			if len(parts) <= 1 && header.Typeflag != tar.TypeDir {
				resolved, err := resolveConflictDirectory(baseOutputDir, fallbackName, conflictPolicy)
				if err != nil {
					return 0, 0, 0, baseOutputDir, err
				}
				actualOutputDir = resolved
				manager.SetCreatedRoot(actualOutputDir)
			} else if len(parts) > 0 {
				manager.SetCreatedRoot(filepath.Join(baseOutputDir, parts[0]))
			}
			determinedSubfolder = true
		}

		if shouldSkipEntry(header.Name, excludePatterns, nil, header.Typeflag == tar.TypeDir) {
			if header.Typeflag != tar.TypeDir {
				skipped++
				manager.Update(func(s *ExtractionState) {
					s.SkippedFiles = skipped
				})
			}
			continue
		}

		targetPath := filepath.Join(actualOutputDir, cleanEntry)
		cleanActual := filepath.Clean(actualOutputDir)
		if targetPath != cleanActual && !strings.HasPrefix(filepath.Clean(targetPath), cleanActual+string(filepath.Separator)) {
			continue
		}

		if header.Typeflag == tar.TypeDir {
			_ = os.MkdirAll(targetPath, 0755)
			continue
		}

		freeBytes, diskErr := getFreeDiskSpace(actualOutputDir)
		if diskErr == nil {
			minFloorBytes := uint64(midStreamMinMB) * 1024 * 1024
			neededBytes := uint64(header.Size) + minFloorBytes
			if freeBytes < neededBytes {
				return count, skipped, totalUnpacked, actualOutputDir, fmt.Errorf("mid-stream storage halt: '%s' requires %.2f MB, only %.2f MB free", header.Name, float64(header.Size)/(1024*1024), float64(freeBytes)/(1024*1024))
			}
			manager.Update(func(s *ExtractionState) {
				s.FreeDiskGB = float64(freeBytes) / (1024 * 1024 * 1024)
			})
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return count, skipped, totalUnpacked, actualOutputDir, err
		}

		_ = os.Chmod(targetPath, 0644)
		normPath := normalizeTargetPath(targetPath)
		outFile, err := os.OpenFile(normPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			return count, skipped, totalUnpacked, actualOutputDir, err
		}

		manager.SetActiveFile(outFile, targetPath)
		manager.Update(func(s *ExtractionState) {
			s.CurrentFile = header.Name
			s.FinalDest = actualOutputDir
		})

		bufWriter := bufio.NewWriterSize(outFile, 256*1024)
		written, err := io.CopyBuffer(bufWriter, tr, buf)
		flushErr := bufWriter.Flush()
		_ = outFile.Close()
		manager.ClearActiveFile()
		if err == nil && flushErr != nil {
			err = flushErr
		}
		manager.ClearActiveFile()

		if err != nil {
			_ = os.Remove(targetPath)
			if ctx.Err() != nil {
				return count, skipped, totalUnpacked, actualOutputDir, ctx.Err()
			}
			if isDiskSpaceOrPermissionError(err) {
				if waitErr := manager.WaitForDiskSpace(ctx, actualOutputDir, err); waitErr != nil {
					return count, skipped, totalUnpacked, actualOutputDir, waitErr
				}
			}
			return count, skipped, totalUnpacked, actualOutputDir, err
		}

		totalUnpacked += written
		count++
		manager.Update(func(s *ExtractionState) {
			s.ExtractedFiles = count
			s.UncompressedMB = float64(totalUnpacked) / (1024 * 1024)
			s.UncompressedGB = float64(totalUnpacked) / (1024 * 1024 * 1024)
		})
	}
	return count, skipped, totalUnpacked, actualOutputDir, nil
}

func extractZIP(ctx context.Context, client *http.Client, resolvedURL string, contentLength int64, baseOutputDir string, autoSubfolder bool, customFolder string, conflictPolicy string, fallbackName string, bufferSizeMB int, excludePatterns []string, selectionSet map[string]bool, headers map[string]string) (count int, skipped int, totalUnpacked int64, actualOutputDir string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("ZIP decompression panic trapped: %v", rec)
		}
	}()
	actualOutputDir = baseOutputDir
	req, err := http.NewRequestWithContext(ctx, "GET", resolvedURL, nil)
	if err != nil {
		return 0, 0, 0, baseOutputDir, err
	}
	applyCustomHeaders(req, headers)

	r, err := httpreaderat.New(client, req, nil)
	if err != nil {
		return 0, 0, 0, baseOutputDir, fmt.Errorf("failed to init range reader: %w", err)
	}

	zipReader, err := zip.NewReader(r, contentLength)
	if err != nil {
		return 0, 0, 0, baseOutputDir, fmt.Errorf("failed to parse central directory: %w", err)
	}

	var totalRequiredBytes uint64
	for _, f := range zipReader.File {
		if !shouldSkipEntry(f.Name, excludePatterns, selectionSet, f.FileInfo().IsDir()) {
			totalRequiredBytes += f.UncompressedSize64
		}
	}
	freeBytes, diskErr := getFreeDiskSpace(baseOutputDir)
	if diskErr == nil {
		if totalRequiredBytes+(uint64(safetyBufferGB)*1024*1024*1024) > freeBytes {
			return 0, 0, 0, baseOutputDir, fmt.Errorf("insufficient disk space: selected ZIP files require %.2f GB (+1GB safety buffer), only %.2f GB available", float64(totalRequiredBytes)/(1024*1024*1024), float64(freeBytes)/(1024*1024*1024))
		}
	}

	buf := acquireBuffer(bufferSizeMB)
	defer releaseBuffer(bufferSizeMB, buf)

	readAheadSize := bufferSizeMB * 1024 * 1024
	if readAheadSize > 4*1024*1024 {
		readAheadSize = 4 * 1024 * 1024
	}

	actualOutputDir = baseOutputDir
	startTime := time.Now()
	speedCalc := newRollingSpeedCalculator()
	lastLogged := startTime

	if customFolder != "" {
		resolved, err := resolveConflictDirectory(baseOutputDir, customFolder, conflictPolicy)
		if err != nil {
			return 0, 0, 0, baseOutputDir, err
		}
		actualOutputDir = resolved
		manager.SetCreatedRoot(actualOutputDir)
	} else if autoSubfolder {
		hasScatteredRoot := false
		topDirs := make(map[string]bool)
		for _, f := range zipReader.File {
			sanitized := sanitizeArchiveEntryPath(f.Name)
			if sanitized == "" || sanitized == "." {
				continue
			}
			parts := strings.Split(sanitized, string(filepath.Separator))
			if len(parts) <= 1 && !f.FileInfo().IsDir() {
				hasScatteredRoot = true
				break
			}
			if len(parts) > 0 {
				topDirs[parts[0]] = true
			}
		}
		if hasScatteredRoot {
			resolved, err := resolveConflictDirectory(baseOutputDir, fallbackName, conflictPolicy)
			if err != nil {
				return 0, 0, 0, baseOutputDir, err
			}
			actualOutputDir = resolved
			manager.SetCreatedRoot(actualOutputDir)
		} else if len(topDirs) == 1 {
			for d := range topDirs {
				manager.SetCreatedRoot(filepath.Join(baseOutputDir, d))
			}
		}
	}

	for _, file := range zipReader.File {
		if ctx.Err() != nil {
			return count, skipped, totalUnpacked, actualOutputDir, ctx.Err()
		}

		if file.Mode()&os.ModeSymlink != 0 {
			continue
		}

		cleanEntry := sanitizeArchiveEntryPath(file.Name)
		if cleanEntry == "" || cleanEntry == "." {
			continue
		}
		if shouldSkipEntry(file.Name, excludePatterns, selectionSet, file.FileInfo().IsDir()) {
			if !file.FileInfo().IsDir() {
				skipped++
				manager.Update(func(s *ExtractionState) {
					s.SkippedFiles = skipped
				})
			}
			continue
		}

		targetPath := filepath.Join(actualOutputDir, cleanEntry)
		cleanActual := filepath.Clean(actualOutputDir)
		if targetPath != cleanActual && !strings.HasPrefix(filepath.Clean(targetPath), cleanActual+string(filepath.Separator)) {
			continue
		}

		if file.FileInfo().IsDir() {
			_ = os.MkdirAll(targetPath, 0755)
			continue
		}

		freeBytes, diskErr := getFreeDiskSpace(actualOutputDir)
		if diskErr == nil {
			minFloorBytes := uint64(midStreamMinMB) * 1024 * 1024
			neededBytes := file.UncompressedSize64 + minFloorBytes
			if freeBytes < neededBytes {
				return count, skipped, totalUnpacked, actualOutputDir, fmt.Errorf("mid-stream storage halt: '%s' requires %.2f MB, only %.2f MB free", file.Name, float64(file.UncompressedSize64)/(1024*1024), float64(freeBytes)/(1024*1024))
			}
			manager.Update(func(s *ExtractionState) {
				s.FreeDiskGB = float64(freeBytes) / (1024 * 1024 * 1024)
			})
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return count, skipped, totalUnpacked, actualOutputDir, err
		}

		rc, err := file.Open()
		if err != nil {
			return count, skipped, totalUnpacked, actualOutputDir, err
		}

		bufferedRC := bufio.NewReaderSize(rc, readAheadSize)

		_ = os.Chmod(targetPath, 0644)
		normPath := normalizeTargetPath(targetPath)
		outFile, err := os.OpenFile(normPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			_ = rc.Close()
			return count, skipped, totalUnpacked, actualOutputDir, err
		}

		manager.SetActiveFile(outFile, targetPath)
		manager.Update(func(s *ExtractionState) {
			s.CurrentFile = file.Name
			s.FinalDest = actualOutputDir
		})

		bufWriter := bufio.NewWriterSize(outFile, 256*1024)
		written, err := io.CopyBuffer(bufWriter, bufferedRC, buf)
		flushErr := bufWriter.Flush()
		_ = rc.Close()
		_ = outFile.Close()
		manager.ClearActiveFile()
		if err == nil && flushErr != nil {
			err = flushErr
		}

		if err != nil {
			_ = os.Remove(targetPath)
			if ctx.Err() != nil {
				return count, skipped, totalUnpacked, actualOutputDir, ctx.Err()
			}
			if isDiskSpaceOrPermissionError(err) {
				if waitErr := manager.WaitForDiskSpace(ctx, actualOutputDir, err); waitErr != nil {
					return count, skipped, totalUnpacked, actualOutputDir, waitErr
				}
			}
			return count, skipped, totalUnpacked, actualOutputDir, err
		}

		totalUnpacked += written
		count++

		now := time.Now()
		rollingSpeed := speedCalc.addSample(now, totalUnpacked)

		if now.Sub(lastLogged) >= 200*time.Millisecond {
			var pct float64
			etaStr := "--:--"
			if totalRequiredBytes > 0 {
				pct = (float64(totalUnpacked) / float64(totalRequiredBytes)) * 100
				rem := int64(totalRequiredBytes) - totalUnpacked
				speedBytesSec := rollingSpeed * 1024 * 1024
				if speedBytesSec > 0 && rem > 0 {
					etaSec := int(float64(rem) / speedBytesSec)
					etaStr = fmt.Sprintf("%02d:%02d", etaSec/60, etaSec%60)
				}
			}
			memMB := sampleProcessMemMB()
			tempC, thermStatus := sampleDeviceTemperature()
			manager.Update(func(s *ExtractionState) {
				s.ExtractedFiles = count
				s.DownloadedMB = float64(totalUnpacked) / (1024 * 1024)
				s.TotalMB = float64(totalRequiredBytes) / (1024 * 1024)
				s.HasContentLen = totalRequiredBytes > 0
				s.Percent = pct
				s.SpeedMBs = rollingSpeed
				s.ETA = etaStr
				s.UncompressedMB = float64(totalUnpacked) / (1024 * 1024)
				s.UncompressedGB = float64(totalUnpacked) / (1024 * 1024 * 1024)
				s.MemMB = memMB
				if runtime.GOOS == "linux" && tempC > 0 {
					s.TempC = tempC
					s.ThermalStatus = thermStatus
				}
			})
			lastLogged = now
		}
	}
	return count, skipped, totalUnpacked, actualOutputDir, nil
}

func extract7Z(ctx context.Context, client *http.Client, resolvedURL string, contentLength int64, baseOutputDir string, autoSubfolder bool, customFolder string, conflictPolicy string, fallbackName string, password string, bufferSizeMB int, excludePatterns []string, selectionSet map[string]bool, headers map[string]string) (count int, skipped int, totalUnpacked int64, actualOutputDir string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("7Z decompression panic trapped: %v", rec)
		}
	}()
	actualOutputDir = baseOutputDir
	req, err := http.NewRequestWithContext(ctx, "GET", resolvedURL, nil)
	if err != nil {
		return 0, 0, 0, baseOutputDir, err
	}
	applyCustomHeaders(req, headers)

	r, err := httpreaderat.New(client, req, nil)
	if err != nil {
		return 0, 0, 0, baseOutputDir, fmt.Errorf("failed to init 7z range reader: %w", err)
	}

	var szReader *sevenzip.Reader
	if password != "" {
		szReader, err = sevenzip.NewReaderWithPassword(r, contentLength, password)
	} else {
		szReader, err = sevenzip.NewReader(r, contentLength)
	}
	if err != nil {
		if isEncryptionError(err) {
			return 0, 0, 0, baseOutputDir, errPasswordRequired
		}
		return 0, 0, 0, baseOutputDir, fmt.Errorf("failed to parse 7z archive: %w", err)
	}

	var totalRequiredBytes uint64
	for _, f := range szReader.File {
		if !shouldSkipEntry(f.Name, excludePatterns, selectionSet, f.FileInfo().IsDir()) {
			totalRequiredBytes += f.UncompressedSize
		}
	}

	buf := acquireBuffer(bufferSizeMB)
	defer releaseBuffer(bufferSizeMB, buf)

	actualOutputDir = baseOutputDir
	startTime := time.Now()
	speedCalc := newRollingSpeedCalculator()
	lastLogged := startTime

	if customFolder != "" {
		resolved, err := resolveConflictDirectory(baseOutputDir, customFolder, conflictPolicy)
		if err != nil {
			return 0, 0, 0, baseOutputDir, err
		}
		actualOutputDir = resolved
		manager.SetCreatedRoot(actualOutputDir)
	} else if autoSubfolder {
		hasScatteredRoot := false
		topDirs := make(map[string]bool)
		for _, f := range szReader.File {
			sanitized := sanitizeArchiveEntryPath(f.Name)
			if sanitized == "" || sanitized == "." {
				continue
			}
			parts := strings.Split(sanitized, string(filepath.Separator))
			if len(parts) <= 1 && !f.FileInfo().IsDir() {
				hasScatteredRoot = true
				break
			}
			if len(parts) > 0 {
				topDirs[parts[0]] = true
			}
		}
		if hasScatteredRoot {
			resolved, err := resolveConflictDirectory(baseOutputDir, fallbackName, conflictPolicy)
			if err != nil {
				return 0, 0, 0, baseOutputDir, err
			}
			actualOutputDir = resolved
			manager.SetCreatedRoot(actualOutputDir)
		} else if len(topDirs) == 1 {
			for d := range topDirs {
				manager.SetCreatedRoot(filepath.Join(baseOutputDir, d))
			}
		}
	}

	for _, file := range szReader.File {
		if ctx.Err() != nil {
			return count, skipped, totalUnpacked, actualOutputDir, ctx.Err()
		}

		if file.Mode()&os.ModeSymlink != 0 {
			continue
		}

		cleanEntry := sanitizeArchiveEntryPath(file.Name)
		if cleanEntry == "" || cleanEntry == "." {
			continue
		}
		if shouldSkipEntry(file.Name, excludePatterns, selectionSet, file.FileInfo().IsDir()) {
			if !file.FileInfo().IsDir() {
				skipped++
				manager.Update(func(s *ExtractionState) {
					s.SkippedFiles = skipped
				})
			}
			continue
		}

		targetPath := filepath.Join(actualOutputDir, cleanEntry)
		cleanActual := filepath.Clean(actualOutputDir)
		if targetPath != cleanActual && !strings.HasPrefix(filepath.Clean(targetPath), cleanActual+string(filepath.Separator)) {
			continue
		}

		if file.FileInfo().IsDir() {
			_ = os.MkdirAll(targetPath, 0755)
			continue
		}

		freeBytes, diskErr := getFreeDiskSpace(actualOutputDir)
		if diskErr == nil {
			minFloorBytes := uint64(midStreamMinMB) * 1024 * 1024
			neededBytes := uint64(file.UncompressedSize) + minFloorBytes
			if freeBytes < neededBytes {
				return count, skipped, totalUnpacked, actualOutputDir, fmt.Errorf("mid-stream storage halt: '%s' requires %.2f MB, only %.2f MB free", file.Name, float64(file.UncompressedSize)/(1024*1024), float64(freeBytes)/(1024*1024))
			}
			manager.Update(func(s *ExtractionState) {
				s.FreeDiskGB = float64(freeBytes) / (1024 * 1024 * 1024)
			})
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return count, skipped, totalUnpacked, actualOutputDir, err
		}

		rc, err := file.Open()
		if err != nil {
			if isEncryptionError(err) {
				return count, skipped, totalUnpacked, actualOutputDir, errPasswordRequired
			}
			return count, skipped, totalUnpacked, actualOutputDir, err
		}

		_ = os.Chmod(targetPath, 0644)
		normPath := normalizeTargetPath(targetPath)
		outFile, err := os.OpenFile(normPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			_ = rc.Close()
			return count, skipped, totalUnpacked, actualOutputDir, err
		}

		manager.SetActiveFile(outFile, targetPath)
		manager.Update(func(s *ExtractionState) {
			s.CurrentFile = file.Name
			s.FinalDest = actualOutputDir
		})

		bufWriter := bufio.NewWriterSize(outFile, 256*1024)
		written, err := io.CopyBuffer(bufWriter, rc, buf)
		flushErr := bufWriter.Flush()
		_ = rc.Close()
		_ = outFile.Close()
		manager.ClearActiveFile()
		if err == nil && flushErr != nil {
			err = flushErr
		}

		if err != nil {
			_ = os.Remove(targetPath)
			if ctx.Err() != nil {
				return count, skipped, totalUnpacked, actualOutputDir, ctx.Err()
			}
			if isEncryptionError(err) {
				return count, skipped, totalUnpacked, actualOutputDir, errPasswordRequired
			}
			if isDiskSpaceOrPermissionError(err) {
				if waitErr := manager.WaitForDiskSpace(ctx, actualOutputDir, err); waitErr != nil {
					return count, skipped, totalUnpacked, actualOutputDir, waitErr
				}
			}
			return count, skipped, totalUnpacked, actualOutputDir, err
		}

		totalUnpacked += written
		count++

		now := time.Now()
		rollingSpeed := speedCalc.addSample(now, totalUnpacked)

		if now.Sub(lastLogged) >= 200*time.Millisecond {
			var pct float64
			etaStr := "--:--"
			if totalRequiredBytes > 0 {
				pct = (float64(totalUnpacked) / float64(totalRequiredBytes)) * 100
				rem := int64(totalRequiredBytes) - totalUnpacked
				speedBytesSec := rollingSpeed * 1024 * 1024
				if speedBytesSec > 0 && rem > 0 {
					etaSec := int(float64(rem) / speedBytesSec)
					etaStr = fmt.Sprintf("%02d:%02d", etaSec/60, etaSec%60)
				}
			}
			memMB := sampleProcessMemMB()
			tempC, thermStatus := sampleDeviceTemperature()
			manager.Update(func(s *ExtractionState) {
				s.ExtractedFiles = count
				s.DownloadedMB = float64(totalUnpacked) / (1024 * 1024)
				s.TotalMB = float64(totalRequiredBytes) / (1024 * 1024)
				s.HasContentLen = totalRequiredBytes > 0
				s.Percent = pct
				s.SpeedMBs = rollingSpeed
				s.ETA = etaStr
				s.UncompressedMB = float64(totalUnpacked) / (1024 * 1024)
				s.UncompressedGB = float64(totalUnpacked) / (1024 * 1024 * 1024)
				s.MemMB = memMB
				if runtime.GOOS == "linux" && tempC > 0 {
					s.TempC = tempC
					s.ThermalStatus = thermStatus
				}
			})
			lastLogged = now
		}
	}
	return count, skipped, totalUnpacked, actualOutputDir, nil
}

// flattenSingleRootDir hoists inner contents up one level if targetDir contains only a single subdirectory.
func flattenSingleRootDir(targetDir string) error {
	entries, err := os.ReadDir(targetDir)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return nil
	}

	wrapperPath := filepath.Join(targetDir, entries[0].Name())
	innerEntries, err := os.ReadDir(wrapperPath)
	if err != nil {
		return err
	}

	// Rename the wrapper first so inner items with the same name cannot collide.
	tempWrapper := filepath.Join(targetDir, fmt.Sprintf(".directcrate_flatten_%d", time.Now().UnixNano()))
	if err := os.Rename(wrapperPath, tempWrapper); err != nil {
		return err
	}

	for _, item := range innerEntries {
		src := filepath.Join(tempWrapper, item.Name())
		dst := filepath.Join(targetDir, item.Name())
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}

	return os.Remove(tempWrapper)
}

