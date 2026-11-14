package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSanitizeArchiveEntryPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "normal nested path",
			in:   "Game/bin/game.exe",
			want: filepath.Join("Game", "bin", "game.exe"),
		},
		{
			name: "windows illegal characters",
			in:   `Game:1/sub?dir*/file"name<1>|2.txt`,
			want: filepath.Join("Game_1", "sub_dir_", "file_name_1__2.txt"),
		},
		{
			name: "trailing spaces and dots",
			in:   "Folder . /SubDir.../readme.txt.. ",
			want: filepath.Join("Folder", "SubDir", "readme.txt"),
		},
		{
			name: "backslashes normalized",
			in:   `Game\Data:01\config?.ini `,
			want: filepath.Join("Game", "Data_01", "config_.ini"),
		},
		{
			name: "dot segments collapsed",
			in:   "./Game/./data/file.bin",
			want: filepath.Join("Game", "data", "file.bin"),
		},
		{
			name: "empty or root dot",
			in:   ".",
			want: ".",
		},
		{
			name: "path traversal rejected",
			in:   "Game/../../etc/passwd",
			want: "",
		},
		{
			name: "windows reserved names prefixed",
			in:   "aux.c/con.txt/NUL/COM1/LPT9.log",
			want: filepath.Join("_aux.c", "_con.txt", "_NUL", "_COM1", "_LPT9.log"),
		},
		{
			name: "ascii control characters sanitized",
			in:   "Game/\x00hidden/file\x1f.bin",
			want: filepath.Join("Game", "_hidden", "file_.bin"),
		},
		{
			name: "windows drive letter neutralized",
			in:   `C:\Games\setup.exe`,
			want: filepath.Join("C_", "Games", "setup.exe"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeArchiveEntryPath(tc.in)
			if got != tc.want {
				t.Fatalf("sanitizeArchiveEntryPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseURLList(t *testing.T) {
	raw := "\r\n  \"https://cdn.example.com/game.part1.rar\" \r\n\n  'https://cdn.example.com/game.part2.rar'  \n   \nhttps://cdn.example.com/game.part3.rar\n"
	want := []string{
		"https://cdn.example.com/game.part1.rar",
		"https://cdn.example.com/game.part2.rar",
		"https://cdn.example.com/game.part3.rar",
	}

	got := parseURLList(raw)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseURLList() = %#v, want %#v", got, want)
	}

	if empty := parseURLList("   \r\n\t "); len(empty) != 0 {
		t.Fatalf("expected empty slice for whitespace input, got %#v", empty)
	}
}

func TestCompileExcludePatterns(t *testing.T) {
	got := compileExcludePatterns(" *_French*, , *Bonus_OST* , *.TMP ")
	want := []string{"*_french*", "*bonus_ost*", "*.tmp"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compileExcludePatterns() = %#v, want %#v", got, want)
	}

	if empty := compileExcludePatterns(" , , "); len(empty) != 0 {
		t.Fatalf("expected empty slice for blank patterns, got %#v", empty)
	}
}

func TestDeriveFallbackName(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{
			url:  "https://example.com/downloads/Cyber.Game.v1.2.zip",
			want: "Cyber_Game_v1_2",
		},
		{
			url:  "https://example.com/downloads/Adventure.part1.rar",
			want: "Adventure",
		},
		{
			url:  "https://example.com/downloads/Adventure.part01.rar",
			want: "Adventure",
		},
		{
			url:  "https://example.com/downloads/BuildArchive.tar.gz",
			want: "BuildArchive",
		},
		{
			url:  "",
			want: "Extracted_Archive",
		},
	}

	for _, tc := range tests {
		got := deriveFallbackName(tc.url)
		if got != tc.want {
			t.Fatalf("deriveFallbackName(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestComputeSteamAppID(t *testing.T) {
	exe := "/home/deck/Games/Hades/Hades.exe"
	name := "Hades"
	id := computeSteamAppID(exe, name)

	if id&0x80000000 == 0 {
		t.Fatalf("expected high bit set on Steam AppID, got %#x", id)
	}

	want := crc32.ChecksumIEEE([]byte(exe+name)) | 0x80000000
	if id != want {
		t.Fatalf("computeSteamAppID() = %d, want %d", id, want)
	}

	if other := computeSteamAppID(exe, "Hades II"); other == id {
		t.Fatalf("expected distinct AppIDs for different app names")
	}
}

func TestFlattenSingleRootDir(t *testing.T) {
	t.Run("hoists single nested directory", func(t *testing.T) {
		root := t.TempDir()
		nested := filepath.Join(root, "WrapperFolder")
		sub := filepath.Join(nested, "data")
		if err := os.MkdirAll(sub, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(nested, "game.exe"), []byte("exe"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "pak0.bin"), []byte("pak"), 0644); err != nil {
			t.Fatal(err)
		}

		if err := flattenSingleRootDir(root); err != nil {
			t.Fatalf("flattenSingleRootDir failed: %v", err)
		}

		if _, err := os.Stat(filepath.Join(root, "game.exe")); err != nil {
			t.Fatalf("expected hoisted game.exe: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, "data", "pak0.bin")); err != nil {
			t.Fatalf("expected hoisted data/pak0.bin: %v", err)
		}
		if _, err := os.Stat(nested); !os.IsNotExist(err) {
			t.Fatalf("expected wrapper directory to be removed")
		}
	})

	t.Run("handles identical inner directory name", func(t *testing.T) {
		root := t.TempDir()
		innerSameName := filepath.Join(root, "Game", "Game")
		if err := os.MkdirAll(innerSameName, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(innerSameName, "launch.exe"), []byte("ok"), 0644); err != nil {
			t.Fatal(err)
		}

		if err := flattenSingleRootDir(root); err != nil {
			t.Fatalf("flattenSingleRootDir failed on same-name inner dir: %v", err)
		}

		if _, err := os.Stat(filepath.Join(root, "Game", "launch.exe")); err != nil {
			t.Fatalf("expected hoisted Game/launch.exe: %v", err)
		}
	})

	t.Run("leaves multiple top-level entries untouched", func(t *testing.T) {
		root := t.TempDir()
		dirA := filepath.Join(root, "A")
		dirB := filepath.Join(root, "B")
		if err := os.MkdirAll(dirA, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dirB, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dirA, "a.txt"), []byte("a"), 0644); err != nil {
			t.Fatal(err)
		}

		if err := flattenSingleRootDir(root); err != nil {
			t.Fatalf("flattenSingleRootDir failed: %v", err)
		}

		if _, err := os.Stat(filepath.Join(dirA, "a.txt")); err != nil {
			t.Fatalf("expected dirA/a.txt to remain untouched: %v", err)
		}
	})
}

func TestReadTailLines(t *testing.T) {
	tempDir := t.TempDir()
	logFile := filepath.Join(tempDir, "test.log")

	// Empty file or non-existent file
	lines, err := readTailLines(logFile, 10)
	if err != nil || len(lines) != 0 {
		t.Fatalf("expected 0 lines from missing file, got %d, err: %v", len(lines), err)
	}

	// Write 10 lines
	var content strings.Builder
	for i := 1; i <= 10; i++ {
		content.WriteString(fmt.Sprintf("line %d\n", i))
	}
	if err := os.WriteFile(logFile, []byte(content.String()), 0644); err != nil {
		t.Fatal(err)
	}

	// Read trailing 3 lines
	tail3, err := readTailLines(logFile, 3)
	if err != nil {
		t.Fatalf("readTailLines failed: %v", err)
	}
	want3 := []string{"line 8", "line 9", "line 10"}
	if !reflect.DeepEqual(tail3, want3) {
		t.Fatalf("readTailLines(3) = %#v, want %#v", tail3, want3)
	}

	// Read with limit larger than total line count
	tailAll, err := readTailLines(logFile, 50)
	if err != nil {
		t.Fatalf("readTailLines failed: %v", err)
	}
	if len(tailAll) != 10 {
		t.Fatalf("expected 10 lines, got %d", len(tailAll))
	}
}

func TestRotateLogIfExceeds(t *testing.T) {
	tempDir := t.TempDir()
	logFile := filepath.Join(tempDir, "directcrate.log")
	oldFile := logFile + ".old"

	// Create 100-byte file
	data := make([]byte, 100)
	if err := os.WriteFile(logFile, data, 0644); err != nil {
		t.Fatal(err)
	}

	// Should not rotate if threshold is 200 bytes
	rotateLogIfExceeds(logFile, 200)
	if _, err := os.Stat(logFile); err != nil {
		t.Fatalf("log file should still exist: %v", err)
	}
	if _, err := os.Stat(oldFile); err == nil {
		t.Fatalf("old log file should not exist yet")
	}

	// Should rotate if threshold is 100 bytes
	rotateLogIfExceeds(logFile, 100)
	if _, err := os.Stat(oldFile); err != nil {
		t.Fatalf("expected rotated file %s: %v", oldFile, err)
	}
	if _, err := os.Stat(logFile); err == nil {
		t.Fatalf("expected original log file to be moved to .old")
	}
}

func TestFormatCrashReport(t *testing.T) {
	dummyStack := []byte("goroutine 1 [running]:\nmain.executeTask(...)\n\t/main.go:100")
	report := formatCrashReport("test:context", "simulated panic", dummyStack)

	if !strings.Contains(report, "DIRECTCRATE CRASH REPORT") {
		t.Fatalf("missing crash report header")
	}
	if !strings.Contains(report, "test:context") {
		t.Fatalf("missing context name in crash report")
	}
	if !strings.Contains(report, "simulated panic") {
		t.Fatalf("missing panic value in crash report")
	}
	if !strings.Contains(report, "goroutine 1 [running]") {
		t.Fatalf("missing stack trace in crash report")
	}
}

func TestSaveAndLoadSettings(t *testing.T) {
	tempDir := t.TempDir()
	setCustomConfigDir(tempDir)
	defer setCustomConfigDir("")

	// Initially defaults should load
	s := loadSettings()
	if s.Workers != 4 {
		t.Fatalf("expected default workers 4, got %d", s.Workers)
	}
	if s.BufferMB != 4 {
		t.Fatalf("expected default buffer_mb 4, got %d", s.BufferMB)
	}

	// Save custom settings
	s.DefaultDest = filepath.Join(tempDir, "downloads")
	s.Workers = 8
	s.BufferMB = 16
	s.SpeedLimitMBs = 25
	s.ConflictMode = "skip"
	s.DesktopNotify = false

	if err := saveSettings(s); err != nil {
		t.Fatalf("failed to save settings: %v", err)
	}

	loaded := loadSettings()
	if loaded.DefaultDest != s.DefaultDest {
		t.Fatalf("expected DefaultDest %s, got %s", s.DefaultDest, loaded.DefaultDest)
	}
	if loaded.Workers != 8 {
		t.Fatalf("expected Workers 8, got %d", loaded.Workers)
	}
	if loaded.BufferMB != 16 {
		t.Fatalf("expected BufferMB 16, got %d", loaded.BufferMB)
	}
	if loaded.SpeedLimitMBs != 25 {
		t.Fatalf("expected SpeedLimitMBs 25, got %d", loaded.SpeedLimitMBs)
	}
	if loaded.ConflictMode != "skip" {
		t.Fatalf("expected ConflictMode skip, got %s", loaded.ConflictMode)
	}
	if loaded.DesktopNotify != false {
		t.Fatalf("expected DesktopNotify false, got %v", loaded.DesktopNotify)
	}
}

func TestResolveValidBrowsePath(t *testing.T) {
	tempDir := t.TempDir()
	subDir := filepath.Join(tempDir, "existing_folder")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("failed to create test directory: %v", err)
	}

	// 1. Valid directory returns itself
	if got := resolveValidBrowsePath(subDir); got != subDir {
		t.Fatalf("resolveValidBrowsePath(%q) = %q, want %q", subDir, got, subDir)
	}

	// 2. Non-existent nested directory climbs up to existing parent
	nonExistent := filepath.Join(subDir, "missing", "deeper")
	if got := resolveValidBrowsePath(nonExistent); got != subDir {
		t.Fatalf("resolveValidBrowsePath(%q) = %q, want %q", nonExistent, got, subDir)
	}

	// 3. Empty string resolves to an existing host directory
	if got := resolveValidBrowsePath(""); got == "" {
		t.Fatalf("resolveValidBrowsePath(\"\") returned empty string")
	} else if fi, err := os.Stat(got); err != nil || !fi.IsDir() {
		t.Fatalf("resolveValidBrowsePath(\"\") = %q which is not an existing directory", got)
	}
}

func TestHandleBrowse(t *testing.T) {
	tempDir := t.TempDir()
	folderA := filepath.Join(tempDir, "AlphaFolder")
	folderB := filepath.Join(tempDir, "BetaFolder")
	if err := os.MkdirAll(folderA, 0755); err != nil {
		t.Fatalf("failed to create AlphaFolder: %v", err)
	}
	if err := os.MkdirAll(folderB, 0755); err != nil {
		t.Fatalf("failed to create BetaFolder: %v", err)
	}

	// GET /api/browse?path=tempDir
	req := httptest.NewRequest(http.MethodGet, "/api/browse?path="+tempDir, nil)
	rr := httptest.NewRecorder()
	handleBrowse(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/browse returned code %d, want 200", rr.Code)
	}

	var resp struct {
		Current   string         `json:"current"`
		Parent    string         `json:"parent"`
		Items     []DirItem      `json:"items"`
		Shortcuts []ShortcutItem `json:"shortcuts"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode GET /api/browse response: %v", err)
	}

	if resp.Current != tempDir {
		t.Fatalf("expected Current %q, got %q", tempDir, resp.Current)
	}
	if len(resp.Shortcuts) == 0 {
		t.Fatalf("expected at least 1 shortcut, got 0")
	}

	foundAlpha := false
	for _, it := range resp.Items {
		if it.Name == "AlphaFolder" && it.IsDir {
			foundAlpha = true
		}
	}
	if !foundAlpha {
		t.Fatalf("expected AlphaFolder in items, got %#v", resp.Items)
	}

	// POST /api/browse to create New Folder
	createBody := strings.NewReader(fmt.Sprintf(`{"parent":%q,"name":"GammaFolder"}`, tempDir))
	postReq := httptest.NewRequest(http.MethodPost, "/api/browse", createBody)
	postRR := httptest.NewRecorder()
	handleBrowse(postRR, postReq)

	if postRR.Code != http.StatusOK {
		t.Fatalf("POST /api/browse returned code %d, want 200", postRR.Code)
	}

	gammaPath := filepath.Join(tempDir, "GammaFolder")
	if fi, err := os.Stat(gammaPath); err != nil || !fi.IsDir() {
		t.Fatalf("expected GammaFolder to exist on disk at %s", gammaPath)
	}

	// POST with invalid name returns 400
	badBody := strings.NewReader(fmt.Sprintf(`{"parent":%q,"name":"../Escape"}`, tempDir))
	badReq := httptest.NewRequest(http.MethodPost, "/api/browse", badBody)
	badRR := httptest.NewRecorder()
	handleBrowse(badRR, badReq)

	if badRR.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/browse with invalid name returned code %d, want 400", badRR.Code)
	}
}

func TestGetActiveProgressSummary(t *testing.T) {
	tempDir := t.TempDir()
	setCustomConfigDir(tempDir)
	defer setCustomConfigDir("")

	// 1. Idle state
	manager.mu.Lock()
	manager.state.Status = "idle"
	manager.state.CurrentFile = ""
	manager.state.Percent = 0
	manager.state.SpeedMBs = 0
	manager.mu.Unlock()

	sum := getActiveProgressSummary()
	if sum.Active || sum.Paused {
		t.Fatalf("expected inactive and not paused when idle, got %#v", sum)
	}

	// 2. Running state
	manager.mu.Lock()
	manager.state.Status = "running"
	manager.state.CurrentFile = "archive.part1.rar"
	manager.state.Percent = 45.5
	manager.state.SpeedMBs = 18.2
	manager.mu.Unlock()

	sumRunning := getActiveProgressSummary()
	if !sumRunning.Active || sumRunning.Paused {
		t.Fatalf("expected active and not paused when running, got %#v", sumRunning)
	}
	if sumRunning.Percent != 45.5 {
		t.Fatalf("expected percent 45.5, got %f", sumRunning.Percent)
	}
	if sumRunning.Filename != "archive.part1.rar" {
		t.Fatalf("expected filename archive.part1.rar, got %s", sumRunning.Filename)
	}
	if sumRunning.Speed != "18.2 MB/s" {
		t.Fatalf("expected speed 18.2 MB/s, got %s", sumRunning.Speed)
	}

	// 3. Needs password state (active but paused)
	manager.mu.Lock()
	manager.state.Status = "needs_password"
	manager.state.Message = "Password required"
	manager.mu.Unlock()

	sumPaused := getActiveProgressSummary()
	if !sumPaused.Active || !sumPaused.Paused {
		t.Fatalf("expected active and paused when needs_password, got %#v", sumPaused)
	}

	// 4. JSON serialization
	jsonStr := getActiveProgressSummaryJSON()
	var parsed ProgressSummary
	if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
		t.Fatalf("failed to unmarshal progress json: %v", err)
	}
	if parsed.Status != "needs_password" || !parsed.Paused {
		t.Fatalf("unexpected parsed json: %#v", parsed)
	}
	if !parsed.LiveProgressNotifications {
		t.Fatalf("expected LiveProgressNotifications true by default, got false")
	}

	// 5. Test disabling live progress notifications in settings
	cfg := loadSettings()
	cfg.LiveProgressNotifications = false
	if err := saveSettings(cfg); err != nil {
		t.Fatalf("failed to save settings: %v", err)
	}
	sumDisabled := getActiveProgressSummary()
	if sumDisabled.LiveProgressNotifications {
		t.Fatalf("expected LiveProgressNotifications false after save, got true")
	}

	// Clean up state
	manager.mu.Lock()
	manager.state.Status = "idle"
	manager.mu.Unlock()
}

func TestProbeDirectCrateInstance(t *testing.T) {
	// 1. Mock valid DirectCrate server
	validServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/status" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"app":"DirectCrate","status":"idle","message":"Ready"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer validServer.Close()

	if !probeDirectCrateInstance(validServer.URL) {
		t.Fatalf("expected probeDirectCrateInstance to return true for valid DirectCrate server")
	}

	// 2. Mock foreign server running on same port (missing or wrong app identifier)
	foreignServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/status" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"app":"OtherService","status":"running"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer foreignServer.Close()

	if probeDirectCrateInstance(foreignServer.URL) {
		t.Fatalf("expected probeDirectCrateInstance to return false for foreign server")
	}

	// 3. Mock server returning 404
	notFoundServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer notFoundServer.Close()

	if probeDirectCrateInstance(notFoundServer.URL) {
		t.Fatalf("expected probeDirectCrateInstance to return false for 404 server")
	}

	// 4. Mock unreachable server
	if probeDirectCrateInstance("http://127.0.0.1:59999") {
		t.Fatalf("expected probeDirectCrateInstance to return false for unreachable port")
	}
}

func TestCorruptJSONRecovery(t *testing.T) {
	tempDir := t.TempDir()
	setCustomConfigDir(tempDir)
	defer setCustomConfigDir("")

	settingsPath := filepath.Join(tempDir, "settings.json")
	corruptContent := []byte(`{"theme": "dark", "bufferMB": NOT_VALID_JSON...`)

	// 1. Corrupt settings.json recovery
	if err := os.WriteFile(settingsPath, corruptContent, 0644); err != nil {
		t.Fatalf("failed to write corrupt settings file: %v", err)
	}

	loaded := loadSettings()
	if loaded.Theme != "system" || loaded.BufferMB != 4 {
		t.Fatalf("expected default settings upon corrupt file, got %+v", loaded)
	}

	bakPath := settingsPath + ".corrupt.bak"
	bakData, err := os.ReadFile(bakPath)
	if err != nil {
		t.Fatalf("expected corrupt backup file %s to exist: %v", bakPath, err)
	}
	if string(bakData) != string(corruptContent) {
		t.Fatalf("backup file content mismatch: got %q, want %q", string(bakData), string(corruptContent))
	}

	// 2. Corrupt history.json recovery
	historyPath := filepath.Join(tempDir, "history.json")
	if err := os.WriteFile(historyPath, corruptContent, 0644); err != nil {
		t.Fatalf("failed to write corrupt history file: %v", err)
	}

	// Trigger appendHistory which reads history.json
	appendHistory(HistoryEntry{
		Timestamp:       time.Now().Format(time.RFC3339),
		FolderName:      "test_archive",
		FinalDest:       "/tmp/test",
		ExtractedFiles:  5,
		IntegrityStatus: "OK",
	})

	histBakPath := historyPath + ".corrupt.bak"
	histBakData, err := os.ReadFile(histBakPath)
	if err != nil {
		t.Fatalf("expected corrupt history backup file %s to exist: %v", histBakPath, err)
	}
	if string(histBakData) != string(corruptContent) {
		t.Fatalf("history backup content mismatch: got %q, want %q", string(histBakData), string(corruptContent))
	}
}

func TestIsDiskSpaceOrPermissionError(t *testing.T) {
	if isDiskSpaceOrPermissionError(nil) {
		t.Fatalf("expected false for nil error")
	}
	if !isDiskSpaceOrPermissionError(syscall.ENOSPC) {
		t.Fatalf("expected true for syscall.ENOSPC")
	}
	if !isDiskSpaceOrPermissionError(syscall.EACCES) {
		t.Fatalf("expected true for syscall.EACCES")
	}
	if !isDiskSpaceOrPermissionError(os.ErrPermission) {
		t.Fatalf("expected true for os.ErrPermission")
	}
	if !isDiskSpaceOrPermissionError(errors.New("write error: no space left on device")) {
		t.Fatalf("expected true for substring 'no space left on device'")
	}
	if !isDiskSpaceOrPermissionError(errors.New("Disk full while unpacking archive")) {
		t.Fatalf("expected true for substring 'Disk full'")
	}
	if !isDiskSpaceOrPermissionError(errors.New("mkdir: permission denied")) {
		t.Fatalf("expected true for substring 'permission denied'")
	}
	if isDiskSpaceOrPermissionError(errors.New("connection reset by peer")) {
		t.Fatalf("expected false for connection reset error")
	}
}

func TestWaitForDiskSpace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	targetDir := t.TempDir()
	err := manager.WaitForDiskSpace(ctx, targetDir, syscall.ENOSPC)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	manager.mu.Lock()
	status := manager.state.Status
	manager.state.Status = "idle"
	manager.mu.Unlock()

	if status != "needs_disk" {
		t.Fatalf("expected status 'needs_disk', got %q", status)
	}
}

func TestHandleFocus(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/focus", strings.NewReader("{}"))
	rr := httptest.NewRecorder()

	handleFocus(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}

	var res map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatalf("failed to decode json response: %v", err)
	}
	if res["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", res)
	}
}

func TestGlobalManagerLifecycleAndQueueCheck(t *testing.T) {
	// Verify IsBusyOrQueued
	manager.mu.Lock()
	manager.state.Status = "idle"
	manager.queue = nil
	manager.mu.Unlock()

	if manager.IsBusyOrQueued() {
		t.Fatalf("expected IsBusyOrQueued false when idle with empty queue")
	}

	manager.mu.Lock()
	manager.state.Status = "running"
	manager.mu.Unlock()

	if !manager.IsBusyOrQueued() {
		t.Fatalf("expected IsBusyOrQueued true when status is running")
	}

	manager.mu.Lock()
	manager.state.Status = "idle"
	manager.queue = []QueuedTask{{ID: "task-1", Label: "Archive 1"}}
	manager.mu.Unlock()

	if !manager.IsBusyOrQueued() {
		t.Fatalf("expected IsBusyOrQueued true when queue is non-empty")
	}

	// Clean up state
	manager.mu.Lock()
	manager.state.Status = "idle"
	manager.queue = nil
	manager.mu.Unlock()
}

func TestLinuxDesktopIntegration(t *testing.T) {
	tempHome := t.TempDir()
	origHome := os.Getenv("HOME")
	_ = os.Setenv("HOME", tempHome)
	defer os.Setenv("HOME", origHome)

	ensureLinuxDesktopIntegration()

	// Verify SVG icon
	iconPath := filepath.Join(tempHome, ".local", "share", "icons", "hicolor", "scalable", "apps", "directcrate.svg")
	svgData, err := os.ReadFile(iconPath)
	if err != nil {
		t.Fatalf("expected directcrate.svg to be written: %v", err)
	}
	if !strings.Contains(string(svgData), "#09090B") || !strings.Contains(string(svgData), "#FAFAFA") || !strings.Contains(string(svgData), "#10B981") {
		t.Fatalf("unexpected directcrate.svg content: %s", string(svgData))
	}

	// Verify .desktop file
	desktopPath := filepath.Join(tempHome, ".local", "share", "applications", "directcrate.desktop")
	desktopData, err := os.ReadFile(desktopPath)
	if err != nil {
		t.Fatalf("expected directcrate.desktop to be written: %v", err)
	}
	s := string(desktopData)
	if !strings.Contains(s, "Icon=directcrate") || !strings.Contains(s, "StartupWMClass=DirectCrate") || !strings.Contains(s, "Exec=") {
		t.Fatalf("directcrate.desktop missing required keys: %s", s)
	}
}

func TestPropertyStoreDataBlockAndLnkInjection(t *testing.T) {
	block := buildPropertyStoreDataBlock("DirectCrate.App")
	if len(block) != 85 {
		t.Fatalf("expected PropertyStoreDataBlock length 85, got %d", len(block))
	}
	if !bytes.Contains(block, []byte("1SPS")) {
		t.Fatalf("expected 1SPS signature in property store")
	}

	tempDir := t.TempDir()
	mockLnk := filepath.Join(tempDir, "mock.lnk")
	// Write dummy lnk with 4-byte terminal 0x00000000
	dummyContent := append([]byte("DUMMY_LNK_HEADER_DATA_1234567890"), 0, 0, 0, 0)
	if err := os.WriteFile(mockLnk, dummyContent, 0644); err != nil {
		t.Fatal(err)
	}

	injectAppUserModelIDToLnk(mockLnk, "DirectCrate.App")

	injected, err := os.ReadFile(mockLnk)
	if err != nil {
		t.Fatal(err)
	}
	utf16AppID := []byte("D\x00i\x00r\x00e\x00c\x00t\x00C\x00r\x00a\x00t\x00e\x00.\x00A\x00p\x00p\x00")
	if !bytes.Contains(injected, utf16AppID) {
		t.Fatalf("expected injected lnk to contain UTF-16LE 'DirectCrate.App'")
	}
	if len(injected) != len(dummyContent)-4+len(block)+4 {
		t.Fatalf("unexpected injected length: got %d, want %d", len(injected), len(dummyContent)-4+len(block)+4)
	}
}

func TestFaviconAndIconEndpoints(t *testing.T) {
	router := setupRouter()

	// 1. /favicon.ico
	reqFav := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	rrFav := httptest.NewRecorder()
	router.ServeHTTP(rrFav, reqFav)
	if rrFav.Code != http.StatusOK {
		t.Fatalf("GET /favicon.ico returned %d, want 200", rrFav.Code)
	}
	if ct := rrFav.Header().Get("Content-Type"); ct != "image/x-icon" {
		t.Fatalf("GET /favicon.ico Content-Type = %q, want image/x-icon", ct)
	}
	if len(rrFav.Body.Bytes()) == 0 {
		t.Fatalf("GET /favicon.ico returned empty body")
	}

	// 2. /icon.png
	reqIcon := httptest.NewRequest(http.MethodGet, "/icon.png", nil)
	rrIcon := httptest.NewRecorder()
	router.ServeHTTP(rrIcon, reqIcon)
	if rrIcon.Code != http.StatusOK {
		t.Fatalf("GET /icon.png returned %d, want 200", rrIcon.Code)
	}
	if ct := rrIcon.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("GET /icon.png Content-Type = %q, want image/png", ct)
	}
	if len(rrIcon.Body.Bytes()) == 0 {
		t.Fatalf("GET /icon.png returned empty body")
	}

	// 3. 404 for unknown path
	req404 := httptest.NewRequest(http.MethodGet, "/some-unknown-path", nil)
	rr404 := httptest.NewRecorder()
	router.ServeHTTP(rr404, req404)
	if rr404.Code != http.StatusNotFound {
		t.Fatalf("GET /some-unknown-path returned %d, want 404", rr404.Code)
	}
}

func TestRollingSpeedCalculator(t *testing.T) {
	calc := newRollingSpeedCalculator()
	t0 := time.Now()

	// Initial sample
	speed0 := calc.addSample(t0, 0)
	if speed0 != 0 {
		t.Fatalf("expected 0 on first sample, got %f", speed0)
	}

	// Sample 1s later: 10 MB downloaded
	t1 := t0.Add(1 * time.Second)
	speed1 := calc.addSample(t1, 10*1024*1024)
	if speed1 < 9.9 || speed1 > 10.1 {
		t.Fatalf("expected ~10 MB/s, got %f", speed1)
	}

	// Sample 2s later: 20 MB total (10 MB more)
	t2 := t0.Add(2 * time.Second)
	speed2 := calc.addSample(t2, 20*1024*1024)
	if speed2 < 9.9 || speed2 > 10.1 {
		t.Fatalf("expected ~10 MB/s, got %f", speed2)
	}

	// Sample 4s later: t0 is pruned (>3s ago), from t1 (10MB) to t4 (40MB) = 30MB over 3s = 10 MB/s
	t4 := t0.Add(4 * time.Second)
	speed4 := calc.addSample(t4, 40*1024*1024)
	if speed4 < 9.9 || speed4 > 10.1 {
		t.Fatalf("expected ~10 MB/s, got %f", speed4)
	}
}

func TestProbeRemoteURLFallback(t *testing.T) {
	// Mock server that returns 405 Method Not Allowed on HEAD, but 206 Partial Content on GET with Range
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodGet {
			if r.Header.Get("Range") == "bytes=0-0" {
				w.Header().Set("Content-Range", "bytes 0-0/10485760")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write([]byte("R"))
				return
			}
			w.Header().Set("Content-Length", "10485760")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("Rar!...."))
		}
	}))
	defer ts.Close()

	client := ts.Client()
	resp, resolved, contentLen, err := probeRemoteURL(context.Background(), client, ts.URL, nil)
	if err != nil {
		t.Fatalf("probeRemoteURL failed: %v", err)
	}
	defer resp.Body.Close()

	if resolved == "" {
		t.Fatalf("expected non-empty resolved URL")
	}
	if contentLen != 10485760 {
		t.Fatalf("expected content length 10485760, got %d", contentLen)
	}
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 206 or 200, got %d", resp.StatusCode)
	}
}

func TestNormalizeTargetPath(t *testing.T) {
	shortPath := "/tmp/short/file.txt"
	if p := normalizeTargetPath(shortPath); p != shortPath {
		t.Fatalf("expected %s, got %s", shortPath, p)
	}
}

func TestThermalsEndpointAndTelemetry(t *testing.T) {
	router := setupRouter()

	// 1. POST /api/thermals
	body := bytes.NewBufferString(`{"temp_c": 42.5, "thermal_status": "warm"}`)
	reqPost := httptest.NewRequest(http.MethodPost, "/api/thermals", body)
	reqPost.Header.Set("Content-Type", "application/json")
	rrPost := httptest.NewRecorder()
	router.ServeHTTP(rrPost, reqPost)
	if rrPost.Code != http.StatusOK {
		t.Fatalf("POST /api/thermals returned %d, want 200", rrPost.Code)
	}

	// 2. GET /api/status should now contain updated telemetry fields
	reqGet := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rrGet := httptest.NewRecorder()
	router.ServeHTTP(rrGet, reqGet)
	if rrGet.Code != http.StatusOK {
		t.Fatalf("GET /api/status returned %d, want 200", rrGet.Code)
	}

	var st ExtractionState
	if err := json.Unmarshal(rrGet.Body.Bytes(), &st); err != nil {
		t.Fatalf("failed to unmarshal status JSON: %v", err)
	}
	if st.MemMB <= 0 {
		t.Fatalf("expected positive MemMB in status, got %f", st.MemMB)
	}
	if st.TempC != 42.5 {
		t.Fatalf("expected TempC 42.5, got %f", st.TempC)
	}
	if st.ThermalStatus != "warm" {
		t.Fatalf("expected ThermalStatus 'warm', got %s", st.ThermalStatus)
	}
}

func TestPlainLanguageErrorTranslation(t *testing.T) {
	// 1. Webpage HTML server
	htmlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!DOCTYPE html><html><body><h1>Landing Page</h1></body></html>"))
	}))
	defer htmlServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	tmpDir := t.TempDir()
	_, err := inspectRemoteArchive(ctx, []string{htmlServer.URL}, tmpDir, nil)
	if err == nil {
		t.Fatalf("expected error inspecting HTML webpage, got nil")
	}
	expectedHTML := "This link opened a webpage instead of a direct archive file. Copy the direct download link and try again."
	if !strings.Contains(err.Error(), expectedHTML) {
		t.Fatalf("expected error containing %q, got %q", expectedHTML, err.Error())
	}

	// 2. HTTP 403 Forbidden server
	forbiddenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Forbidden", http.StatusForbidden)
	}))
	defer forbiddenServer.Close()

	_, err403 := inspectRemoteArchive(ctx, []string{forbiddenServer.URL}, tmpDir, nil)
	if err403 == nil {
		t.Fatalf("expected error inspecting 403 URL, got nil")
	}
	expected403 := "Download link expired or blocked by host. Refresh the link and click Retry to resume."
	if !strings.Contains(err403.Error(), expected403) {
		t.Fatalf("expected error containing %q, got %q", expected403, err403.Error())
	}

	// 3. Storage full error message
	gm := &GlobalManager{clients: make(map[chan string]bool)}
	pauseCtx, pauseCancel := context.WithCancel(context.Background())
	pauseCancel() // cancel immediately so WaitForDiskSpace exits
	_ = gm.WaitForDiskSpace(pauseCtx, tmpDir, errors.New("no space left on device"))

	gm.mu.RLock()
	diskMsg := gm.state.Message
	gm.mu.RUnlock()
	expectedDisk := "Not enough free storage space. Free up space on your device and click Retry."
	if diskMsg != expectedDisk {
		t.Fatalf("expected message %q, got %q", expectedDisk, diskMsg)
	}
}

func TestHandleSettingsDelete(t *testing.T) {
	tmpDir := t.TempDir()
	setCustomConfigDir(tmpDir)
	defer setCustomConfigDir("")

	// Save custom setting
	customCfg := AppSettings{
		BufferMB:      32,
		Workers:       8,
		SpeedLimitMBs: 50,
	}
	if err := saveSettings(customCfg); err != nil {
		t.Fatalf("failed saving settings: %v", err)
	}

	router := setupRouter()
	reqDel := httptest.NewRequest(http.MethodDelete, "/api/settings", nil)
	rrDel := httptest.NewRecorder()
	router.ServeHTTP(rrDel, reqDel)
	if rrDel.Code != http.StatusOK {
		t.Fatalf("DELETE /api/settings returned %d, want 200", rrDel.Code)
	}

	// Verify settings file was deleted
	cfgPath := getSettingsFilePath()
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatalf("expected settings file %s to be removed, but stat was: %v", cfgPath, err)
	}

	// Reload settings should yield default settings
	loaded := loadSettings()
	if loaded.Workers != 4 || loaded.BufferMB != defaultBufferMB {
		t.Fatalf("expected default settings, got Workers=%d, BufferMB=%d", loaded.Workers, loaded.BufferMB)
	}
}

func TestHandleOpenFolderEndpoint(t *testing.T) {
	router := setupRouter()

	// 1. Invalid payload
	reqBad := httptest.NewRequest(http.MethodPost, "/api/open-folder", strings.NewReader("bad json"))
	rrBad := httptest.NewRecorder()
	router.ServeHTTP(rrBad, reqBad)
	if rrBad.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/open-folder with bad json returned %d, want 400", rrBad.Code)
	}

	// 2. Valid folder path (using temp directory)
	tmpDir := t.TempDir()
	body := fmt.Sprintf(`{"path": %q}`, tmpDir)
	reqValid := httptest.NewRequest(http.MethodPost, "/api/open-folder", strings.NewReader(body))
	rrValid := httptest.NewRecorder()
	router.ServeHTTP(rrValid, reqValid)
	if rrValid.Code != http.StatusOK {
		t.Fatalf("POST /api/open-folder with valid path returned %d, want 200", rrValid.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(rrValid.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["ok"] != true {
		t.Fatalf("expected ok: true, got %v", resp)
	}
}

func TestIsFlatpakSandbox(t *testing.T) {
	// Under normal test execution outside flatpak, should return false unless FLATPAK_ID is set
	origID := os.Getenv("FLATPAK_ID")
	defer func() { _ = os.Setenv("FLATPAK_ID", origID) }()

	_ = os.Setenv("FLATPAK_ID", "com.directcrate.App")
	if !isFlatpakSandbox() {
		t.Fatalf("expected isFlatpakSandbox to return true when FLATPAK_ID is set")
	}

	_ = os.Unsetenv("FLATPAK_ID")
	// If /.flatpak-info doesn't exist, should return false
	if _, err := os.Stat("/.flatpak-info"); err != nil {
		if isFlatpakSandbox() {
			t.Fatalf("expected isFlatpakSandbox to return false outside sandbox")
		}
	}
}

// Helpers for archive mocking
func createTestZipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("failed creating zip entry %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("failed writing zip entry %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("failed closing zip writer: %v", err)
	}
	return buf.Bytes()
}

func createTestTarBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		data := []byte(content)
		hdr := &tar.Header{
			Name: name,
			Mode: 0644,
			Size: int64(len(data)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("failed writing tar header %s: %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("failed writing tar content %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("failed closing tar writer: %v", err)
	}
	return buf.Bytes()
}

func createMockArchiveServer(t *testing.T, archiveData []byte) *httptest.Server {
	t.Helper()
	fixedModTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "test_archive.zip", fixedModTime, bytes.NewReader(archiveData))
	}))
}

func TestAPIInspect(t *testing.T) {
	router := setupRouter()
	testFiles := map[string]string{
		"game/game.exe":         "EXECUTABLE_BINARY_DATA",
		"game/data/config.json": `{"version": 1}`,
		"game/readme.txt":       "Play instructions",
	}
	zipData := createTestZipBytes(t, testFiles)
	srv := createMockArchiveServer(t, zipData)
	defer srv.Close()

	// 1. Valid archive inspection
	bodyValid := fmt.Sprintf(`{"url": %q}`, srv.URL)
	reqValid := httptest.NewRequest(http.MethodPost, "/api/inspect", strings.NewReader(bodyValid))
	rrValid := httptest.NewRecorder()
	router.ServeHTTP(rrValid, reqValid)
	if rrValid.Code != http.StatusOK {
		t.Fatalf("POST /api/inspect valid archive returned %d: %s", rrValid.Code, rrValid.Body.String())
	}
	var inspectResp struct {
		Format           string `json:"format"`
		FileCount        int    `json:"file_count"`
		UncompressedMB   float64 `json:"uncompressed_mb"`
		Encrypted        bool   `json:"encrypted"`
		Files            []struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		} `json:"files"`
	}
	if err := json.NewDecoder(rrValid.Body).Decode(&inspectResp); err != nil {
		t.Fatalf("failed decoding inspect response: %v", err)
	}
	if strings.ToLower(inspectResp.Format) != "zip" {
		t.Fatalf("expected format 'zip', got %q", inspectResp.Format)
	}
	if inspectResp.FileCount != 3 {
		t.Fatalf("expected 3 files, got %d", inspectResp.FileCount)
	}
	if inspectResp.Encrypted {
		t.Fatalf("expected encrypted=false")
	}

	// 2. Custom headers forwarding
	var headerReceivedAuth, headerReceivedCookie, headerReceivedReferer string
	headerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headerReceivedAuth = r.Header.Get("Authorization")
		headerReceivedCookie = r.Header.Get("Cookie")
		headerReceivedReferer = r.Header.Get("Referer")
		http.ServeContent(w, r, "test_archive.zip", time.Now(), bytes.NewReader(zipData))
	}))
	defer headerSrv.Close()

	bodyHeaders := fmt.Sprintf(`{"url": %q, "headers": {"Authorization": "Bearer token123", "Cookie": "sess=xyz", "Referer": "https://ref.example.com"}}`, headerSrv.URL)
	reqHeaders := httptest.NewRequest(http.MethodPost, "/api/inspect", strings.NewReader(bodyHeaders))
	rrHeaders := httptest.NewRecorder()
	router.ServeHTTP(rrHeaders, reqHeaders)
	if rrHeaders.Code != http.StatusOK {
		t.Fatalf("POST /api/inspect with headers returned %d", rrHeaders.Code)
	}
	if headerReceivedAuth != "Bearer token123" {
		t.Fatalf("expected Authorization header 'Bearer token123', got %q", headerReceivedAuth)
	}
	if headerReceivedCookie != "sess=xyz" {
		t.Fatalf("expected Cookie header 'sess=xyz', got %q", headerReceivedCookie)
	}
	if headerReceivedReferer != "https://ref.example.com" {
		t.Fatalf("expected Referer header 'https://ref.example.com', got %q", headerReceivedReferer)
	}

	// 3. Multi-part detection
	bodyMulti := fmt.Sprintf(`{"urls": [%q, %q]}`, srv.URL+"/archive.part1.rar", srv.URL+"/archive.part2.rar")
	reqMulti := httptest.NewRequest(http.MethodPost, "/api/inspect", strings.NewReader(bodyMulti))
	rrMulti := httptest.NewRecorder()
	router.ServeHTTP(rrMulti, reqMulti)
	// Even if probing mock URLs fails format parsing, handler handles multi-part cleanly without 500 panic
	if rrMulti.Code == http.StatusInternalServerError {
		t.Fatalf("POST /api/inspect multi-part returned 500 panic")
	}

	// 4. HTML landing page rejection
	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body>Direct link page</body></html>"))
	}))
	defer htmlSrv.Close()

	bodyHTML := fmt.Sprintf(`{"url": %q}`, htmlSrv.URL)
	reqHTML := httptest.NewRequest(http.MethodPost, "/api/inspect", strings.NewReader(bodyHTML))
	rrHTML := httptest.NewRecorder()
	router.ServeHTTP(rrHTML, reqHTML)
	if rrHTML.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for HTML landing page, got %d", rrHTML.Code)
	}
	if !strings.Contains(rrHTML.Body.String(), "webpage instead of a direct archive") {
		t.Fatalf("expected plain-language error message, got %q", rrHTML.Body.String())
	}

	// 5. 403 Forbidden link error
	forbiddenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Forbidden", http.StatusForbidden)
	}))
	defer forbiddenSrv.Close()

	body403 := fmt.Sprintf(`{"url": %q}`, forbiddenSrv.URL)
	req403 := httptest.NewRequest(http.MethodPost, "/api/inspect", strings.NewReader(body403))
	rr403 := httptest.NewRecorder()
	router.ServeHTTP(rr403, req403)
	if rr403.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for 403 Forbidden URL, got %d", rr403.Code)
	}
	if !strings.Contains(rr403.Body.String(), "expired or blocked") {
		t.Fatalf("expected expired or blocked message, got %q", rr403.Body.String())
	}

	// 6. Bad JSON and empty payload
	reqEmpty := httptest.NewRequest(http.MethodPost, "/api/inspect", strings.NewReader(`{}`))
	rrEmpty := httptest.NewRecorder()
	router.ServeHTTP(rrEmpty, reqEmpty)
	if rrEmpty.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty payload, got %d", rrEmpty.Code)
	}
}

func TestAPISpeedLimit(t *testing.T) {
	router := setupRouter()

	// 1. Set 25 MB/s
	req25 := httptest.NewRequest(http.MethodPost, "/api/speed-limit", strings.NewReader(`{"speed_limit_mbs": 25}`))
	rr25 := httptest.NewRecorder()
	router.ServeHTTP(rr25, req25)
	if rr25.Code != http.StatusOK {
		t.Fatalf("POST /api/speed-limit 25 returned %d", rr25.Code)
	}
	if globalSpeedLimitBytesPerSec.Load() != 25*1024*1024 {
		t.Fatalf("expected 25MB/s limit in atomic, got %d", globalSpeedLimitBytesPerSec.Load())
	}

	// 2. Set 0 (unlimited)
	req0 := httptest.NewRequest(http.MethodPost, "/api/speed-limit", strings.NewReader(`{"speed_limit_mbs": 0}`))
	rr0 := httptest.NewRecorder()
	router.ServeHTTP(rr0, req0)
	if rr0.Code != http.StatusOK {
		t.Fatalf("POST /api/speed-limit 0 returned %d", rr0.Code)
	}
	if globalSpeedLimitBytesPerSec.Load() != 0 {
		t.Fatalf("expected 0 (unlimited) limit, got %d", globalSpeedLimitBytesPerSec.Load())
	}

	// 3. Bad JSON
	reqBad := httptest.NewRequest(http.MethodPost, "/api/speed-limit", strings.NewReader(`invalid`))
	rrBad := httptest.NewRecorder()
	router.ServeHTTP(rrBad, reqBad)
	if rrBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad json, got %d", rrBad.Code)
	}
}

func TestAPIQueue(t *testing.T) {
	router := setupRouter()

	// Drain any leftover queue items
	for {
		if _, ok := manager.PopNextQueue(); !ok {
			break
		}
	}

	// 1. GET /api/queue returns empty slice
	reqGet := httptest.NewRequest(http.MethodGet, "/api/queue", nil)
	rrGet := httptest.NewRecorder()
	router.ServeHTTP(rrGet, reqGet)
	if rrGet.Code != http.StatusOK {
		t.Fatalf("GET /api/queue returned %d", rrGet.Code)
	}
	var qItems []QueuedTask
	if err := json.NewDecoder(rrGet.Body).Decode(&qItems); err != nil {
		t.Fatalf("failed decoding queue: %v", err)
	}
	if len(qItems) != 0 {
		t.Fatalf("expected empty queue, got %d items", len(qItems))
	}

	// 2. POST /api/queue add first item
	payload1 := `{"url": "https://example.com/game1.zip", "custom_folder": "GameOne"}`
	reqPost1 := httptest.NewRequest(http.MethodPost, "/api/queue", strings.NewReader(payload1))
	rrPost1 := httptest.NewRecorder()
	router.ServeHTTP(rrPost1, reqPost1)
	if rrPost1.Code != http.StatusCreated {
		t.Fatalf("POST /api/queue item 1 returned %d", rrPost1.Code)
	}
	var createdItem1 QueuedTask
	if err := json.NewDecoder(rrPost1.Body).Decode(&createdItem1); err != nil {
		t.Fatalf("failed decoding created item 1: %v", err)
	}
	if createdItem1.ID == "" || createdItem1.Label != "GameOne" {
		t.Fatalf("unexpected created item: %#v", createdItem1)
	}
	item1ID := createdItem1.ID

	// 3. POST /api/queue add second item
	payload2 := `{"url": "https://example.com/game2.zip", "custom_folder": "GameTwo"}`
	reqPost2 := httptest.NewRequest(http.MethodPost, "/api/queue", strings.NewReader(payload2))
	rrPost2 := httptest.NewRecorder()
	router.ServeHTTP(rrPost2, reqPost2)
	if rrPost2.Code != http.StatusCreated {
		t.Fatalf("POST /api/queue item 2 returned %d", rrPost2.Code)
	}

	// 4. DELETE /api/queue/<item1ID>
	reqDel := httptest.NewRequest(http.MethodDelete, "/api/queue/"+item1ID, nil)
	rrDel := httptest.NewRecorder()
	router.ServeHTTP(rrDel, reqDel)
	if rrDel.Code != http.StatusOK {
		t.Fatalf("DELETE /api/queue item 1 returned %d", rrDel.Code)
	}
	var qAfterDel []QueuedTask
	_ = json.NewDecoder(rrDel.Body).Decode(&qAfterDel)
	if len(qAfterDel) != 1 || qAfterDel[0].Label != "GameTwo" {
		t.Fatalf("expected remaining queue item GameTwo, got %#v", qAfterDel)
	}

	// 5. DELETE nonexistent item returns 404
	reqDelNon := httptest.NewRequest(http.MethodDelete, "/api/queue/nonexistent-id", nil)
	rrDelNon := httptest.NewRecorder()
	router.ServeHTTP(rrDelNon, reqDelNon)
	if rrDelNon.Code != http.StatusNotFound {
		t.Fatalf("DELETE /api/queue nonexistent returned %d, want 404", rrDelNon.Code)
	}

	// 6. Pop item via manager
	popped, ok := manager.PopNextQueue()
	if !ok || popped.Label != "GameTwo" {
		t.Fatalf("expected popped GameTwo, got %#v (ok=%v)", popped, ok)
	}
	if _, ok := manager.PopNextQueue(); ok {
		t.Fatalf("expected queue to be empty after pop")
	}
}

func TestAPIBrowse(t *testing.T) {
	router := setupRouter()

	// 1. GET /api/browse (empty path resolves default/home)
	reqDefault := httptest.NewRequest(http.MethodGet, "/api/browse", nil)
	rrDefault := httptest.NewRecorder()
	router.ServeHTTP(rrDefault, reqDefault)
	if rrDefault.Code != http.StatusOK {
		t.Fatalf("GET /api/browse default returned %d", rrDefault.Code)
	}
	var browseData struct {
		Current   string `json:"current"`
		Shortcuts []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"shortcuts"`
	}
	if err := json.NewDecoder(rrDefault.Body).Decode(&browseData); err != nil {
		t.Fatalf("failed decoding browse response: %v", err)
	}
	if browseData.Current == "" {
		t.Fatalf("expected non-empty current path")
	}

	// 2. GET /api/browse with non-existent path falls back safely
	reqInvalid := httptest.NewRequest(http.MethodGet, "/api/browse?path=/nonexistent/invalid/dir", nil)
	rrInvalid := httptest.NewRecorder()
	router.ServeHTTP(rrInvalid, reqInvalid)
	if rrInvalid.Code != http.StatusOK {
		t.Fatalf("GET /api/browse invalid path returned %d", rrInvalid.Code)
	}

	// 3. POST /api/browse to create a new folder
	tmpDir := t.TempDir()
	createPayload := fmt.Sprintf(`{"parent": %q, "name": "DirectCrateSubFolder"}`, tmpDir)
	reqCreate := httptest.NewRequest(http.MethodPost, "/api/browse", strings.NewReader(createPayload))
	rrCreate := httptest.NewRecorder()
	router.ServeHTTP(rrCreate, reqCreate)
	if rrCreate.Code != http.StatusOK {
		t.Fatalf("POST /api/browse create folder returned %d: %s", rrCreate.Code, rrCreate.Body.String())
	}
	expectedFolder := filepath.Join(tmpDir, "DirectCrateSubFolder")
	if fi, err := os.Stat(expectedFolder); err != nil || !fi.IsDir() {
		t.Fatalf("expected directory %s to exist, stat: %v", expectedFolder, err)
	}

	// 4. POST /api/browse with empty parent
	reqBadCreate := httptest.NewRequest(http.MethodPost, "/api/browse", strings.NewReader(`{"parent": "", "name": ""}`))
	rrBadCreate := httptest.NewRecorder()
	router.ServeHTTP(rrBadCreate, reqBadCreate)
	if rrBadCreate.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty create folder, got %d", rrBadCreate.Code)
	}
}

func TestAPIHistory(t *testing.T) {
	tmpDir := t.TempDir()
	setCustomConfigDir(tmpDir)
	defer setCustomConfigDir("")

	router := setupRouter()

	// 1. Initial history is empty
	reqEmpty := httptest.NewRequest(http.MethodGet, "/api/history", nil)
	rrEmpty := httptest.NewRecorder()
	router.ServeHTTP(rrEmpty, reqEmpty)
	if rrEmpty.Code != http.StatusOK {
		t.Fatalf("GET /api/history empty returned %d", rrEmpty.Code)
	}

	// 2. Write mock history file
	histFile := filepath.Join(tmpDir, "history.json")
	mockItems := []HistoryEntry{
		{
			FolderName:      "GameArchive1",
			FinalDest:       "/downloads/GameArchive1",
			ExtractedFiles:  10,
			UncompressedGB:  2.5,
			DurationSec:     30,
			AverageSpeedMBs: 85.3,
			Timestamp:       "2026-11-14 20:00:00",
		},
		{
			FolderName:      "GameArchive2",
			FinalDest:       "/downloads/GameArchive2",
			ExtractedFiles:  25,
			UncompressedGB:  5.0,
			DurationSec:     60,
			AverageSpeedMBs: 83.3,
			Timestamp:       "2026-11-14 20:30:00",
		},
	}
	data, _ := json.Marshal(mockItems)
	_ = os.WriteFile(histFile, data, 0644)

	// 3. GET /api/history returns the 2 items
	reqGet := httptest.NewRequest(http.MethodGet, "/api/history", nil)
	rrGet := httptest.NewRecorder()
	router.ServeHTTP(rrGet, reqGet)
	if rrGet.Code != http.StatusOK {
		t.Fatalf("GET /api/history populated returned %d", rrGet.Code)
	}
	var loaded []HistoryEntry
	_ = json.NewDecoder(rrGet.Body).Decode(&loaded)
	if len(loaded) != 2 {
		t.Fatalf("expected 2 history items, got %d", len(loaded))
	}

	// 4. DELETE /api/history clears history
	reqDel := httptest.NewRequest(http.MethodDelete, "/api/history", nil)
	rrDel := httptest.NewRecorder()
	router.ServeHTTP(rrDel, reqDel)
	if rrDel.Code != http.StatusOK {
		t.Fatalf("DELETE /api/history returned %d", rrDel.Code)
	}

	// 5. Verify history is empty
	reqGetAfter := httptest.NewRequest(http.MethodGet, "/api/history", nil)
	rrGetAfter := httptest.NewRecorder()
	router.ServeHTTP(rrGetAfter, reqGetAfter)
	var afterDel []HistoryEntry
	_ = json.NewDecoder(rrGetAfter.Body).Decode(&afterDel)
	if len(afterDel) != 0 {
		t.Fatalf("expected 0 history items after delete, got %d", len(afterDel))
	}
}

func TestAPISettings(t *testing.T) {
	tmpDir := t.TempDir()
	setCustomConfigDir(tmpDir)
	defer setCustomConfigDir("")

	router := setupRouter()

	// 1. GET /api/settings returns defaults
	reqGet := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	rrGet := httptest.NewRecorder()
	router.ServeHTTP(rrGet, reqGet)
	if rrGet.Code != http.StatusOK {
		t.Fatalf("GET /api/settings returned %d", rrGet.Code)
	}

	// 2. POST /api/settings saves custom settings
	customPayload := `{"workers": 8, "buffer_mb": 16, "speed_limit_mbs": 40, "theme": "dark"}`
	reqPost := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(customPayload))
	rrPost := httptest.NewRecorder()
	router.ServeHTTP(rrPost, reqPost)
	if rrPost.Code != http.StatusOK {
		t.Fatalf("POST /api/settings returned %d", rrPost.Code)
	}

	loaded := loadSettings()
	if loaded.Workers != 8 || loaded.BufferMB != 16 || loaded.SpeedLimitMBs != 40 || loaded.Theme != "dark" {
		t.Fatalf("unexpected loaded settings: %#v", loaded)
	}

	// 3. DELETE /api/settings resets
	reqDel := httptest.NewRequest(http.MethodDelete, "/api/settings", nil)
	rrDel := httptest.NewRecorder()
	router.ServeHTTP(rrDel, reqDel)
	if rrDel.Code != http.StatusOK {
		t.Fatalf("DELETE /api/settings returned %d", rrDel.Code)
	}
	reset := loadSettings()
	if reset.Workers != 4 || reset.BufferMB != defaultBufferMB {
		t.Fatalf("expected reset settings, got Workers=%d", reset.Workers)
	}
}

func TestAPIDefaultsAndCheckFolder(t *testing.T) {
	router := setupRouter()

	// 1. GET /api/defaults
	reqDef := httptest.NewRequest(http.MethodGet, "/api/defaults", nil)
	rrDef := httptest.NewRecorder()
	router.ServeHTTP(rrDef, reqDef)
	if rrDef.Code != http.StatusOK {
		t.Fatalf("GET /api/defaults returned %d", rrDef.Code)
	}
	var defResp struct {
		DefaultDest string   `json:"default_dest"`
		OS          string   `json:"os"`
		Arch        string   `json:"arch"`
		LanURLs     []string `json:"lan_urls"`
	}
	if err := json.NewDecoder(rrDef.Body).Decode(&defResp); err != nil {
		t.Fatalf("failed decoding defaults: %v", err)
	}
	if defResp.OS == "" || defResp.Arch == "" {
		t.Fatalf("expected OS and Arch in defaults response")
	}

	// 2. POST /api/check-folder non-existent
	tmpDir := t.TempDir()
	payloadNonEx := fmt.Sprintf(`{"dest": %q, "custom_folder": "FreshFolder"}`, tmpDir)
	reqNonEx := httptest.NewRequest(http.MethodPost, "/api/check-folder", strings.NewReader(payloadNonEx))
	rrNonEx := httptest.NewRecorder()
	router.ServeHTTP(rrNonEx, reqNonEx)
	if rrNonEx.Code != http.StatusOK {
		t.Fatalf("POST /api/check-folder returned %d", rrNonEx.Code)
	}
	var chkNonEx struct {
		Exists bool `json:"exists"`
	}
	_ = json.NewDecoder(rrNonEx.Body).Decode(&chkNonEx)
	if chkNonEx.Exists {
		t.Fatalf("expected exists=false for fresh folder")
	}

	// 3. POST /api/check-folder existing directory with files
	existingDir := filepath.Join(tmpDir, "ExistingFolder")
	_ = os.MkdirAll(existingDir, 0755)
	_ = os.WriteFile(filepath.Join(existingDir, "file1.txt"), []byte("data"), 0644)

	payloadEx := fmt.Sprintf(`{"dest": %q, "custom_folder": "ExistingFolder"}`, tmpDir)
	reqEx := httptest.NewRequest(http.MethodPost, "/api/check-folder", strings.NewReader(payloadEx))
	rrEx := httptest.NewRecorder()
	router.ServeHTTP(rrEx, reqEx)
	var chkEx struct {
		Exists    bool `json:"exists"`
		FileCount int  `json:"file_count"`
	}
	_ = json.NewDecoder(rrEx.Body).Decode(&chkEx)
	if !chkEx.Exists || chkEx.FileCount != 1 {
		t.Fatalf("expected exists=true with file_count=1, got exists=%v count=%d", chkEx.Exists, chkEx.FileCount)
	}
}

func TestAPIUnlockAndResumeURL(t *testing.T) {
	router := setupRouter()

	// 1. POST /api/unlock bad json
	reqBad := httptest.NewRequest(http.MethodPost, "/api/unlock", strings.NewReader("bad"))
	rrBad := httptest.NewRecorder()
	router.ServeHTTP(rrBad, reqBad)
	if rrBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad unlock payload, got %d", rrBad.Code)
	}

	// 2. POST /api/unlock valid payload
	reqUnlock := httptest.NewRequest(http.MethodPost, "/api/unlock", strings.NewReader(`{"password": "secretPassword"}`))
	rrUnlock := httptest.NewRecorder()
	router.ServeHTTP(rrUnlock, reqUnlock)
	if rrUnlock.Code != http.StatusOK {
		t.Fatalf("expected 200 for unlock, got %d", rrUnlock.Code)
	}

	// 3. POST /api/resume-url bad json
	reqResumeBad := httptest.NewRequest(http.MethodPost, "/api/resume-url", strings.NewReader("bad"))
	rrResumeBad := httptest.NewRecorder()
	router.ServeHTTP(rrResumeBad, reqResumeBad)
	if rrResumeBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad resume payload, got %d", rrResumeBad.Code)
	}

	// 4. POST /api/resume-url when not waiting returns 409 Conflict
	reqResumeNoWait := httptest.NewRequest(http.MethodPost, "/api/resume-url", strings.NewReader(`{"url": "https://example.com/refreshed.zip"}`))
	rrResumeNoWait := httptest.NewRecorder()
	router.ServeHTTP(rrResumeNoWait, reqResumeNoWait)
	if rrResumeNoWait.Code != http.StatusConflict {
		t.Fatalf("expected 409 for resume-url when not waiting, got %d", rrResumeNoWait.Code)
	}

	// 5. POST /api/resume-url when stream is waiting returns 200
	manager.urlMu.Lock()
	manager.urlWaitCh = make(chan struct{})
	manager.urlMu.Unlock()

	reqResume := httptest.NewRequest(http.MethodPost, "/api/resume-url", strings.NewReader(`{"url": "https://example.com/refreshed.zip"}`))
	rrResume := httptest.NewRecorder()
	router.ServeHTTP(rrResume, reqResume)
	if rrResume.Code != http.StatusOK {
		t.Fatalf("expected 200 for resume-url, got %d", rrResume.Code)
	}
}

func TestAPILogs(t *testing.T) {
	tmpDir := t.TempDir()
	setCustomConfigDir(tmpDir)
	defer setCustomConfigDir("")

	logFile := filepath.Join(tmpDir, "directcrate.log")
	_ = os.WriteFile(logFile, []byte("2026/11/14 [TEST] Sample diagnostic log line\n"), 0644)

	router := setupRouter()

	// 1. GET /api/logs returns content
	reqGet := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	rrGet := httptest.NewRecorder()
	router.ServeHTTP(rrGet, reqGet)
	if rrGet.Code != http.StatusOK {
		t.Fatalf("GET /api/logs returned %d", rrGet.Code)
	}
	var logResp struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	_ = json.NewDecoder(rrGet.Body).Decode(&logResp)
	if !strings.Contains(logResp.Content, "Sample diagnostic log line") {
		t.Fatalf("expected log content, got %q", logResp.Content)
	}

	// 2. DELETE /api/logs clears file
	reqDel := httptest.NewRequest(http.MethodDelete, "/api/logs", nil)
	rrDel := httptest.NewRecorder()
	router.ServeHTTP(rrDel, reqDel)
	if rrDel.Code != http.StatusOK {
		t.Fatalf("DELETE /api/logs returned %d", rrDel.Code)
	}

	// Verify old logs are cleared
	content, _ := os.ReadFile(logFile)
	if strings.Contains(string(content), "Sample diagnostic log line") {
		t.Fatalf("expected old logs to be cleared, got %q", string(content))
	}
}

func TestExtractZIPAndTarStream(t *testing.T) {
	// 1. Test ZIP extraction
	zipFiles := map[string]string{
		"game/game.exe":         "BINARY_EXE_DATA_FOR_TEST",
		"game/data/config.json": `{"installed": true}`,
		"game/doc/readme.txt":   "Documentation contents",
	}
	zipData := createTestZipBytes(t, zipFiles)
	zipSrv := createMockArchiveServer(t, zipData)
	defer zipSrv.Close()

	destZIP := t.TempDir()
	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second}
	countZip, skippedZip, totalZip, _, errZip := extractZIP(ctx, client, zipSrv.URL, int64(len(zipData)), destZIP, false, "", "overwrite", "game", 4, nil, nil, nil)
	if errZip != nil {
		t.Fatalf("extractZIP failed: %v", errZip)
	}
	if countZip != 3 || skippedZip != 0 || totalZip <= 0 {
		t.Fatalf("unexpected zip extraction counts: count=%d, skipped=%d, total=%d", countZip, skippedZip, totalZip)
	}
	if data, err := os.ReadFile(filepath.Join(destZIP, "game", "game.exe")); err != nil || string(data) != "BINARY_EXE_DATA_FOR_TEST" {
		t.Fatalf("expected extracted game.exe, got err=%v content=%s", err, string(data))
	}
	if data, err := os.ReadFile(filepath.Join(destZIP, "game", "data", "config.json")); err != nil || string(data) != `{"installed": true}` {
		t.Fatalf("expected extracted config.json, got err=%v", err)
	}

	// 2. Test TAR extraction
	tarFiles := map[string]string{
		"app/main.py":   "print('Hello from tar')",
		"app/setup.cfg": "[metadata]\nname=test",
	}
	tarData := createTestTarBytes(t, tarFiles)
	tarSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tarData)))
		_, _ = w.Write(tarData)
	}))
	defer tarSrv.Close()

	destTar := t.TempDir()
	countTar, skippedTar, totalTar, _, errTar := extractTarStream(ctx, bytes.NewReader(tarData), ArchiveTar, destTar, false, "", "overwrite", "app", 4, nil)
	if errTar != nil {
		t.Fatalf("extractTarStream failed: %v", errTar)
	}
	if countTar != 2 || skippedTar != 0 || totalTar <= 0 {
		t.Fatalf("unexpected tar extraction counts: count=%d, skipped=%d, total=%d", countTar, skippedTar, totalTar)
	}
	if data, err := os.ReadFile(filepath.Join(destTar, "app", "main.py")); err != nil || string(data) != "print('Hello from tar')" {
		t.Fatalf("expected extracted main.py, got err=%v content=%s", err, string(data))
	}
}

func TestDetectFormat(t *testing.T) {
	// ZIP
	if fmtType := detectFormat([]byte("PK\x03\x04\x14\x00"), "test.zip"); fmtType != ArchiveZIP {
		t.Fatalf("expected ArchiveZIP, got %v", fmtType)
	}
	// 7Z
	if fmtType := detectFormat([]byte("7z\xbc\xaf\x27\x1c\x00"), "test.7z"); fmtType != Archive7Z {
		t.Fatalf("expected Archive7Z, got %v", fmtType)
	}
	// RAR4
	if fmtType := detectFormat([]byte("Rar!\x1a\x07\x00\x00"), "test.rar"); fmtType != ArchiveRAR5 {
		t.Fatalf("expected ArchiveRAR5, got %v", fmtType)
	}
	// RAR5
	if fmtType := detectFormat([]byte("Rar!\x1a\x07\x01\x00"), "test.rar"); fmtType != ArchiveRAR5 {
		t.Fatalf("expected ArchiveRAR5, got %v", fmtType)
	}
	// TAR
	tarHeader := make([]byte, 512)
	copy(tarHeader[257:], []byte("ustar"))
	if fmtType := detectFormat(tarHeader, "test.tar"); fmtType != ArchiveTar {
		t.Fatalf("expected ArchiveTar, got %v", fmtType)
	}
	// Unknown
	if fmtType := detectFormat([]byte("unknown_data_bytes"), "test.bin"); fmtType != ArchiveUnknown {
		t.Fatalf("expected ArchiveUnknown for unknown bytes, got %v", fmtType)
	}
}

func TestPrimaryExecutablesDetection(t *testing.T) {
	tmpDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(tmpDir, "bin"), 0755)
	_ = os.MkdirAll(filepath.Join(tmpDir, "redist"), 0755)

	// Create valid executables
	_ = os.WriteFile(filepath.Join(tmpDir, "launcher.exe"), []byte("small"), 0755)
	_ = os.WriteFile(filepath.Join(tmpDir, "bin", "game.exe"), []byte(strings.Repeat("large", 1000)), 0755)
	// Create ignored executables
	_ = os.WriteFile(filepath.Join(tmpDir, "redist", "vcredist_x64.exe"), []byte("redist"), 0755)
	_ = os.WriteFile(filepath.Join(tmpDir, "unins000.exe"), []byte("unins"), 0755)

	exes := detectPrimaryExecutables(tmpDir)
	if len(exes) < 2 {
		t.Fatalf("expected at least 2 executables, got %d: %#v", len(exes), exes)
	}
	for _, p := range exes {
		lower := strings.ToLower(p)
		if strings.Contains(lower, "vcredist") || strings.Contains(lower, "unins") {
			t.Fatalf("found unignored helper executable: %s", p)
		}
	}
}

func TestNetworkAndListenHelpers(t *testing.T) {
	// 1. cleanGameSearchQuery
	raw := "Cyberpunk_2077.v2.1-FitGirl.Repack_Deluxe.Edition"
	clean := cleanGameSearchQuery(raw)
	if !strings.Contains(clean, "Cyberpunk 2077") || strings.Contains(clean, "FitGirl") {
		t.Fatalf("cleanGameSearchQuery(%q) = %q, expected cleaned title", raw, clean)
	}

	// 2. formatDisplayURL
	urlStr := formatDisplayURL("0.0.0.0", 9090)
	if urlStr != "http://localhost:9090" {
		t.Fatalf("formatDisplayURL(0.0.0.0, 9090) = %q, want http://localhost:9090", urlStr)
	}
	urlLocal := formatDisplayURL("127.0.0.1", 9090)
	if urlLocal != "http://127.0.0.1:9090" {
		t.Fatalf("formatDisplayURL(127.0.0.1, 9090) = %q, want http://127.0.0.1:9090", urlLocal)
	}
	urlLAN := formatDisplayURL("192.168.1.50", 9090)
	if urlLAN != "http://192.168.1.50:9090" {
		t.Fatalf("formatDisplayURL(192.168.1.50, 9090) = %q", urlLAN)
	}

	// 3. parseListenAddr
	h1, p1, err1 := parseListenAddr(":9095")
	if err1 != nil || h1 != "" || p1 != 9095 {
		t.Fatalf("parseListenAddr(:9095) = (%q, %d, %v), want (\"\", 9095)", h1, p1, err1)
	}
	h2, p2, err2 := parseListenAddr("127.0.0.1:9098")
	if err2 != nil || h2 != "127.0.0.1" || p2 != 9098 {
		t.Fatalf("parseListenAddr(127.0.0.1:9098) = (%q, %d, %v)", h2, p2, err2)
	}

	// 4. getLocalIPv4s and getLANURLs
	ips := getLocalIPv4s()
	urls := getLANURLs(9090)
	if len(ips) != len(urls) {
		t.Fatalf("expected matching ips and urls count: %d vs %d", len(ips), len(urls))
	}

	// 5. syncDiskBuffers
	syncDiskBuffers()
}

func TestEndToEndExtraction(t *testing.T) {
	zipFiles := map[string]string{
		"game/game.exe":         "GAME_EXEC_BINARY_PAYLOAD",
		"game/data/sounds.dat":  "SOUND_DATA_BYTES",
		"game/readme.txt":       "Finished extraction test",
	}
	zipData := createTestZipBytes(t, zipFiles)
	srv := createMockArchiveServer(t, zipData)
	defer srv.Close()

	destDir := t.TempDir()
	opts := TaskOptions{
		URL:            srv.URL,
		Dest:           destDir,
		CustomFolder:   "TestE2EGame",
		ConflictPolicy: "overwrite",
		AutoSubfolder:  true,
	}

	router := setupRouter()
	payloadBytes, _ := json.Marshal(opts)
	reqStart := httptest.NewRequest(http.MethodPost, "/start", bytes.NewReader(payloadBytes))
	rrStart := httptest.NewRecorder()
	router.ServeHTTP(rrStart, reqStart)
	if rrStart.Code != http.StatusOK {
		t.Fatalf("POST /start returned %d: %s", rrStart.Code, rrStart.Body.String())
	}

	// Poll status until complete or timeout
	deadline := time.Now().Add(10 * time.Second)
	completed := false
	for time.Now().Before(deadline) {
		manager.mu.RLock()
		st := manager.state
		manager.mu.RUnlock()
		if st.Status == "completed" {
			completed = true
			break
		}
		if st.Status == "error" {
			t.Fatalf("extraction failed with status error: %s", st.Message)
		}
		time.Sleep(50 * time.Millisecond)
	}

	manager.mu.RLock()
	finalState := manager.state
	manager.mu.RUnlock()

	if !completed {
		t.Fatalf("timed out waiting for extraction to complete, status=%s msg=%s", finalState.Status, finalState.Message)
	}

	// Verify extracted files on disk
	targetFolder := filepath.Join(destDir, "TestE2EGame")
	if data, err := os.ReadFile(filepath.Join(targetFolder, "game", "game.exe")); err != nil || string(data) != "GAME_EXEC_BINARY_PAYLOAD" {
		t.Fatalf("expected extracted game.exe, err=%v data=%s", err, string(data))
	}

	if finalState.ExtractedFiles != 3 {
		t.Fatalf("expected 3 extracted files, got %d", finalState.ExtractedFiles)
	}

	// Reset manager state to idle so subsequent tests start with a clean state
	manager.mu.Lock()
	manager.state = ExtractionState{Status: "idle", Message: "Ready"}
	manager.queue = nil
	manager.mu.Unlock()
}

func TestTriggerGracefulExit(t *testing.T) {
	// Drain any leftover signal before test
	select {
	case <-shutdownSignalCh:
	default:
	}

	triggerGracefulExit()

	select {
	case <-shutdownSignalCh:
		// expected signal delivery
	default:
		t.Fatalf("expected shutdown signal in shutdownSignalCh")
	}
}

func TestCompareSemVer(t *testing.T) {
	tests := []struct {
		v1       string
		v2       string
		expected int
	}{
		{"v1.1.0", "v1.0.0", 1},
		{"1.0.1", "1.0.0", 1},
		{"v2.0.0", "1.9.9", 1},
		{"v1.0.0", "v1.0.0", 0},
		{"1.0.0", "1.0.0", 0},
		{"v0.9.9", "v1.0.0", -1},
		{"v1.0.0-beta", "v1.0.0", 0},
		{"1.0", "1.0.0", 0},
		{"1.0.0.1", "1.0.0", 1},
	}

	for _, tt := range tests {
		got := compareSemVer(tt.v1, tt.v2)
		if got != tt.expected {
			t.Errorf("compareSemVer(%q, %q) = %d, expected %d", tt.v1, tt.v2, got, tt.expected)
		}
	}
}

func TestCheckUpdateAPI(t *testing.T) {
	origURL := githubReleaseURL
	defer func() { githubReleaseURL = origURL }()

	// 1. Mock server with newer release available
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"tag_name": "v1.1.0",
			"html_url": "https://github.com/musakpg/directcrate/releases/tag/v1.1.0",
			"body":     "What's new in v1.1.0: performance boosts.",
		})
	}))
	defer mockServer.Close()

	githubReleaseURL = mockServer.URL

	router := setupRouter()
	req := httptest.NewRequest(http.MethodGet, "/api/check-update", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", rr.Code)
	}

	var resp UpdateCheckResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed decoding UpdateCheckResponse: %v", err)
	}

	if !resp.UpdateAvailable {
		t.Fatalf("expected update_available=true for v1.1.0 > %s", appVersion)
	}
	if resp.LatestVersion != "v1.1.0" {
		t.Fatalf("expected latest_version=v1.1.0, got %s", resp.LatestVersion)
	}
	if resp.ReleaseURL != "https://github.com/musakpg/directcrate/releases/tag/v1.1.0" {
		t.Fatalf("unexpected release_url: %s", resp.ReleaseURL)
	}

	// 2. Mock server returning same or older version
	olderServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"tag_name": "v1.0.0",
			"html_url": "https://github.com/musakpg/directcrate/releases/tag/v1.0.0",
			"body":     "Initial release.",
		})
	}))
	defer olderServer.Close()

	githubReleaseURL = olderServer.URL
	reqOlder := httptest.NewRequest(http.MethodGet, "/api/check-update", nil)
	rrOlder := httptest.NewRecorder()
	router.ServeHTTP(rrOlder, reqOlder)

	var respOlder UpdateCheckResponse
	if err := json.NewDecoder(rrOlder.Body).Decode(&respOlder); err != nil {
		t.Fatalf("failed decoding older UpdateCheckResponse: %v", err)
	}
	if respOlder.UpdateAvailable {
		t.Fatalf("expected update_available=false when versions match")
	}

	// 3. Mock server returning error
	errServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer errServer.Close()

	githubReleaseURL = errServer.URL
	reqErr := httptest.NewRequest(http.MethodGet, "/api/check-update", nil)
	rrErr := httptest.NewRecorder()
	router.ServeHTTP(rrErr, reqErr)

	var respErr UpdateCheckResponse
	if err := json.NewDecoder(rrErr.Body).Decode(&respErr); err != nil {
		t.Fatalf("failed decoding err UpdateCheckResponse: %v", err)
	}
	if respErr.UpdateAvailable {
		t.Fatalf("expected update_available=false on server error")
	}
	if respErr.Error == "" {
		t.Fatalf("expected error message populated on server error")
	}
}





