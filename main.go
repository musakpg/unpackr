package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	appVersion       = "v1.0.0"
	defaultUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	defaultBufferMB  = 4
	maxRetries       = 10
	safetyBufferGB   = 1.0             // 1GB pre-flight safety margin
	midStreamMinMB   = 512             // 512MB minimum free disk floor during mid-stream extraction
	prefetchChunkSz  = 2 * 1024 * 1024 // 2MB per parallel range chunk
)

type HistoryEntry struct {
	Timestamp       string   `json:"timestamp"`
	FolderName      string   `json:"folder_name"`
	FinalDest       string   `json:"final_dest"`
	DetectedExes    []string `json:"detected_exes"`
	UncompressedGB  float64  `json:"uncompressed_gb"`
	ExtractedFiles  int      `json:"extracted_files"`
	SkippedFiles    int      `json:"skipped_files"`
	AverageSpeedMBs float64  `json:"average_speed_mbs"`
	DurationSec     float64  `json:"duration_sec"`
	IntegrityStatus string   `json:"integrity_status"`
}

type QueuedTask struct {
	ID           string      `json:"id"`
	Label        string      `json:"label"`
	TargetFolder string      `json:"target_folder"`
	Options      TaskOptions `json:"options"`
	AddedAt      string      `json:"added_at"`
}

type ExtractionState struct {
	App             string       `json:"app"`
	Status          string       `json:"status"` // idle, running, needs_password, needs_url, needs_disk, completed, error, aborted
	Message         string       `json:"message"`
	CurrentFile     string       `json:"current_file"`
	CurrentPart     int          `json:"current_part"`
	TotalParts      int          `json:"total_parts"`
	DownloadedMB    float64      `json:"downloaded_mb"`
	TotalMB         float64      `json:"total_mb"`
	HasContentLen   bool         `json:"has_content_len"`
	Percent         float64      `json:"percent"`
	SpeedMBs        float64      `json:"speed_mbs"`
	PeakSpeedMBs    float64      `json:"peak_speed_mbs"`
	ETA             string       `json:"eta"`
	FreeDiskGB      float64      `json:"free_disk_gb"`
	ExtractedFiles  int          `json:"extracted_files"`
	SkippedFiles    int          `json:"skipped_files"`
	UncompressedMB  float64      `json:"uncompressed_mb"`
	UncompressedGB  float64      `json:"uncompressed_gb"`
	TimeTakenSec    float64      `json:"time_taken_sec"`
	AverageSpeedMBs float64      `json:"average_speed_mbs"`
	FinalDest       string       `json:"final_dest"`
	IntegrityStatus string       `json:"integrity_status"`
	DetectedExes    []string     `json:"detected_exes"`
	SpeedLimitMBs             int          `json:"speed_limit_mbs"`
	Queue                     []QueuedTask `json:"queue"`
	IsAndroid                 bool         `json:"is_android"`
	LiveProgressNotifications bool         `json:"live_progress_notifications"`
	MemMB                     float64      `json:"mem_mb"`
	TempC                     float64      `json:"temp_c"`
	ThermalStatus             string       `json:"thermal_status"`
	ActiveStreams             int          `json:"active_streams"`
	RetryCount                int          `json:"retry_count"`
}

type TaskOptions struct {
	URL               string            `json:"url"`
	Dest              string            `json:"dest"`
	CustomFolder      string            `json:"custom_folder"`
	ConflictPolicy    string            `json:"conflict_policy"` // merge, overwrite, rename
	SelectedFiles     []string          `json:"selected_files"`  // non-empty when selective filter is active
	AutoSubfolder     bool              `json:"auto_subfolder"`
	FlattenSingleRoot bool              `json:"flatten_single_root"`
	PlaySound         bool              `json:"play_sound"`
	PreventIdle       bool              `json:"prevent_idle"`
	PowerAction       string            `json:"power_action"` // none, sleep, shutdown
	BufferSizeMB      int               `json:"buffer_size_mb"`
	ParallelConns     int               `json:"parallel_conns"`
	SpeedLimitMBs     int               `json:"speed_limit_mbs"`
	ExcludePatterns   string            `json:"exclude_patterns"`
	Password          string            `json:"password"`
	Headers           map[string]string `json:"headers"`
	AddToSteam        bool              `json:"add_to_steam"`
}

type AppSettings struct {
	DefaultDest       string `json:"default_dest"`
	Workers           int    `json:"workers"`
	BufferMB          int    `json:"buffer_mb"`
	ConflictMode      string `json:"conflict_mode"`
	ExcludePatterns   string `json:"exclude_patterns"`
	FlattenSingleRoot bool   `json:"flatten_single_root"`
	SpeedLimitMBs     int    `json:"speed_limit_mbs"`
	PowerAction       string `json:"power_action"`
	PlaySound         bool   `json:"play_sound"`
	DesktopNotify     bool   `json:"desktop_notify"`
	AutoSubfolder     bool   `json:"auto_subfolder"`
	PreventIdle       bool   `json:"prevent_idle"`
	Theme                     string `json:"theme"`
	AddSteam                  bool   `json:"add_steam"`
	LiveProgressNotifications bool   `json:"live_progress_notifications"`
}

type GlobalManager struct {
	mu           sync.RWMutex
	state        ExtractionState
	cancelFunc   context.CancelFunc
	activeFile   *os.File
	activePath   string
	activeBodies []io.Closer
	createdRoot  string
	lastTaskOpts TaskOptions
	clients      map[chan string]bool
	clientsMu    sync.Mutex
	hadUiClient  bool
	idleTimer    *time.Timer

	queue       []QueuedTask
	nextQueueID uint64

	urlMu              sync.Mutex
	urlWaitCh          chan struct{}
	urlWaitExpired     string
	urlNewValue        string
	currentResolvedURL string
}

var manager = &GlobalManager{
	clients: make(map[chan string]bool),
	state: ExtractionState{
		App:                       "DirectCrate",
		Status:                    "idle",
		Message:                   "Ready for remote archive stream.",
		Queue:                     []QueuedTask{},
		IsAndroid:                 runtime.GOOS == "android",
		LiveProgressNotifications: true,
		ActiveStreams:             4,
		RetryCount:                0,
	},
}

func (gm *GlobalManager) broadcastPayload(payload []byte) {
	gm.clientsMu.Lock()
	for ch := range gm.clients {
		select {
		case ch <- string(payload):
		default:
		}
	}
	gm.clientsMu.Unlock()
}

type speedSample struct {
	timestamp time.Time
	bytes     int64
}

type rollingSpeedCalculator struct {
	mu      sync.Mutex
	samples []speedSample
}

func newRollingSpeedCalculator() *rollingSpeedCalculator {
	return &rollingSpeedCalculator{
		samples: make([]speedSample, 0, 32),
	}
}

func (rsc *rollingSpeedCalculator) addSample(now time.Time, totalBytes int64) float64 {
	rsc.mu.Lock()
	defer rsc.mu.Unlock()

	rsc.samples = append(rsc.samples, speedSample{timestamp: now, bytes: totalBytes})

	// Prune samples older than 3 seconds
	cutoff := now.Add(-3 * time.Second)
	idx := 0
	for idx < len(rsc.samples) && rsc.samples[idx].timestamp.Before(cutoff) {
		idx++
	}
	if idx > 0 && idx < len(rsc.samples) {
		rsc.samples = rsc.samples[idx:]
	}

	if len(rsc.samples) < 2 {
		return 0.0
	}

	first := rsc.samples[0]
	last := rsc.samples[len(rsc.samples)-1]
	elapsed := last.timestamp.Sub(first.timestamp).Seconds()
	if elapsed <= 0.05 {
		return 0.0
	}
	deltaBytes := last.bytes - first.bytes
	if deltaBytes < 0 {
		deltaBytes = 0
	}
	return (float64(deltaBytes) / (1024 * 1024)) / elapsed
}

func sampleProcessMemMB() float64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.Sys) / (1024 * 1024)
}

func sampleDeviceTemperature() (float64, string) {
	if runtime.GOOS == "linux" {
		matches, err := filepath.Glob("/sys/class/thermal/thermal_zone*/temp")
		if err == nil && len(matches) > 0 {
			var maxTemp float64
			for _, p := range matches {
				data, err := os.ReadFile(p)
				if err != nil {
					continue
				}
				val, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
				if err != nil {
					continue
				}
				tempC := val / 1000.0
				if tempC >= 15.0 && tempC <= 115.0 && tempC > maxTemp {
					maxTemp = tempC
				}
			}
			if maxTemp > 0 {
				status := "normal"
				if maxTemp >= 80.0 {
					status = "hot"
				} else if maxTemp >= 65.0 {
					status = "warm"
				}
				return maxTemp, status
			}
		}
	}
	return 0.0, ""
}

var telemetrySamplerOnce sync.Once

func startTelemetrySampler() {
	telemetrySamplerOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(1 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				mem := sampleProcessMemMB()
				temp, status := sampleDeviceTemperature()

				manager.mu.RLock()
				clientCount := len(manager.clients)
				curStatus := manager.state.Status
				manager.mu.RUnlock()

				if clientCount > 0 || curStatus == "running" {
					manager.Update(func(s *ExtractionState) {
						s.MemMB = mem
						if runtime.GOOS == "linux" && temp > 0 {
							s.TempC = temp
							s.ThermalStatus = status
						}
					})
				}
			}
		}()
	})
}

// IsBusyOrQueued returns true if an archive stream is actively decompressing,
// waiting for an expired URL hot-swap, waiting for a password, or has pending tasks in queue.
func (gm *GlobalManager) IsBusyOrQueued() bool {
	gm.mu.RLock()
	defer gm.mu.RUnlock()

	isRunning := gm.state.Status == "running" || gm.state.Status == "needs_password" || gm.state.Status == "needs_url" || gm.state.Status == "needs_disk"
	hasQueue := len(gm.queue) > 0
	return isRunning || hasQueue
}

// cancelIdleTimer cancels any pending auto-exit timer under lock.
func (gm *GlobalManager) cancelIdleTimer() {
	gm.clientsMu.Lock()
	defer gm.clientsMu.Unlock()
	if gm.idleTimer != nil {
		gm.idleTimer.Stop()
		gm.idleTimer = nil
		log.Printf("[LIFECYCLE] Activity detected; cancelled idle auto-exit timer")
	}
}

// checkAndScheduleIdleShutdown checks whether conditions are met to initiate the 10-second
// auto-exit grace period on Desktop (Linux / Windows). Android lifecycle is managed natively.
func (gm *GlobalManager) checkAndScheduleIdleShutdown() {
	if runtime.GOOS == "android" {
		return
	}

	gm.clientsMu.Lock()
	defer gm.clientsMu.Unlock()

	if !gm.hadUiClient || len(gm.clients) > 0 {
		return
	}

	if gm.IsBusyOrQueued() {
		return
	}

	if gm.idleTimer != nil {
		gm.idleTimer.Stop()
	}

	log.Printf("[LIFECYCLE] All UI clients disconnected and no tasks active; scheduling auto-exit in 10s...")
	gm.idleTimer = time.AfterFunc(10*time.Second, func() {
		gm.clientsMu.Lock()
		defer gm.clientsMu.Unlock()

		if len(gm.clients) > 0 {
			log.Printf("[LIFECYCLE] UI client reconnected within grace period; auto-exit cancelled")
			return
		}
		if gm.IsBusyOrQueued() {
			log.Printf("[LIFECYCLE] Task became active within grace period; auto-exit cancelled")
			return
		}

		log.Printf("[LIFECYCLE] 10s idle timeout elapsed with zero connected clients; initiating graceful shutdown")
		triggerGracefulExit()
	})
}

func (gm *GlobalManager) syncQueueIntoStateLocked() {
	qCopy := make([]QueuedTask, len(gm.queue))
	copy(qCopy, gm.queue)
	gm.state.Queue = qCopy
}

func (gm *GlobalManager) Update(fn func(s *ExtractionState)) {
	gm.mu.Lock()
	fn(&gm.state)
	gm.syncQueueIntoStateLocked()
	payload, _ := json.Marshal(gm.state)
	gm.mu.Unlock()

	gm.broadcastPayload(payload)
}

func (gm *GlobalManager) EnqueueTask(opts TaskOptions) QueuedTask {
	gm.mu.Lock()
	gm.nextQueueID++
	id := fmt.Sprintf("task_%d", gm.nextQueueID)

	urls := parseURLList(opts.URL)
	label := deriveFallbackName(opts.URL)
	if opts.CustomFolder != "" {
		label = opts.CustomFolder
	}
	if len(urls) > 1 {
		label = fmt.Sprintf("%s (%d parts)", label, len(urls))
	}

	item := QueuedTask{
		ID:           id,
		Label:        label,
		TargetFolder: opts.CustomFolder,
		Options:      opts,
		AddedAt:      time.Now().Format("15:04:05"),
	}
	gm.queue = append(gm.queue, item)
	gm.syncQueueIntoStateLocked()
	payload, _ := json.Marshal(gm.state)
	gm.mu.Unlock()

	gm.broadcastPayload(payload)
	return item
}

func (gm *GlobalManager) RemoveQueuedTask(id string) bool {
	gm.mu.Lock()
	idx := -1
	for i, t := range gm.queue {
		if t.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		gm.mu.Unlock()
		return false
	}
	gm.queue = append(gm.queue[:idx], gm.queue[idx+1:]...)
	gm.syncQueueIntoStateLocked()
	payload, _ := json.Marshal(gm.state)
	gm.mu.Unlock()

	gm.broadcastPayload(payload)
	return true
}

func (gm *GlobalManager) ListQueue() []QueuedTask {
	gm.mu.RLock()
	defer gm.mu.RUnlock()
	qCopy := make([]QueuedTask, len(gm.queue))
	copy(qCopy, gm.queue)
	return qCopy
}

func (gm *GlobalManager) PopNextQueue() (QueuedTask, bool) {
	gm.mu.Lock()
	if len(gm.queue) == 0 {
		gm.mu.Unlock()
		return QueuedTask{}, false
	}
	next := gm.queue[0]
	gm.queue = gm.queue[1:]
	gm.syncQueueIntoStateLocked()
	payload, _ := json.Marshal(gm.state)
	gm.mu.Unlock()

	gm.broadcastPayload(payload)
	return next, true
}

func (gm *GlobalManager) ResetURLWait() {
	gm.urlMu.Lock()
	defer gm.urlMu.Unlock()
	if gm.urlWaitCh != nil {
		select {
		case <-gm.urlWaitCh:
		default:
			close(gm.urlWaitCh)
		}
	}
	gm.urlWaitCh = nil
	gm.urlWaitExpired = ""
	gm.urlNewValue = ""
}

func (gm *GlobalManager) WaitForRefreshedURL(ctx context.Context, expiredURL string, statusCode int) (string, error) {
	gm.urlMu.Lock()
	if gm.urlWaitCh != nil && gm.urlWaitExpired == expiredURL {
		waitCh := gm.urlWaitCh
		gm.urlMu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-waitCh:
			gm.urlMu.Lock()
			newURL := gm.urlNewValue
			gm.urlMu.Unlock()
			return newURL, nil
		}
	}

	gm.urlWaitCh = make(chan struct{})
	gm.urlWaitExpired = expiredURL
	gm.urlNewValue = ""
	waitCh := gm.urlWaitCh
	gm.urlMu.Unlock()

	log.Printf("[URL:EXPIRED] HTTP %d received for %s. Pausing stream for refreshed link.", statusCode, expiredURL)

	gm.Update(func(s *ExtractionState) {
		s.Status = "needs_url"
		s.Message = "Download link expired or blocked by host. Refresh the link and click Retry to resume."
	})

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-waitCh:
		gm.urlMu.Lock()
		newURL := gm.urlNewValue
		gm.urlMu.Unlock()
		if newURL == "" {
			return "", fmt.Errorf("URL refresh cancelled")
		}
		log.Printf("[URL:RESUME] New URL provided: %s. Resuming stream from current offset.", newURL)
		gm.Update(func(s *ExtractionState) {
			s.Status = "running"
			s.Message = "Refreshed URL accepted. Resuming extraction from exact offset..."
		})
		return newURL, nil
	}
}

func (gm *GlobalManager) ProvideRefreshedURL(newURL string) bool {
	gm.urlMu.Lock()
	defer gm.urlMu.Unlock()
	if gm.urlWaitCh == nil {
		return false
	}
	select {
	case <-gm.urlWaitCh:
		return false
	default:
		gm.urlNewValue = newURL
		close(gm.urlWaitCh)
		return true
	}
}

func isDiskSpaceOrPermissionError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EACCES) || errors.Is(err, os.ErrPermission) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no space left on device") ||
		strings.Contains(s, "disk full") ||
		strings.Contains(s, "not enough space") ||
		strings.Contains(s, "permission denied") ||
		strings.Contains(s, "access is denied")
}

// WaitForDiskSpace pauses the extraction stream when destination disk space is exhausted (ENOSPC)
// or permission is denied (EACCES), polling until space is freed by the user or the context is cancelled.
func (gm *GlobalManager) WaitForDiskSpace(ctx context.Context, targetDir string, causeErr error) error {
	log.Printf("[DISK:PAUSE] Low disk space or write error in %s: %v. Pausing stream.", targetDir, causeErr)

	gm.Update(func(s *ExtractionState) {
		s.Status = "needs_disk"
		s.Message = "Not enough free storage space. Free up space on your device and click Retry."
	})

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			freeBytes, err := getFreeDiskSpace(targetDir)
			if err == nil {
				gm.Update(func(s *ExtractionState) {
					s.FreeDiskGB = float64(freeBytes) / (1024 * 1024 * 1024)
				})
				if freeBytes >= midStreamMinMB*1024*1024 {
					log.Printf("[DISK:RESUME] Free space detected (%.2f GB). Resuming extraction.", float64(freeBytes)/(1024*1024*1024))
					gm.Update(func(s *ExtractionState) {
						s.Status = "running"
						s.Message = "Storage space available. Resuming extraction..."
					})
					return nil
				}
			}
		}
	}
}

func (gm *GlobalManager) SetActiveFile(f *os.File, path string) {
	gm.mu.Lock()
	gm.activeFile = f
	gm.activePath = path
	gm.mu.Unlock()
}

func (gm *GlobalManager) ClearActiveFile() {
	gm.mu.Lock()
	gm.activeFile = nil
	gm.activePath = ""
	gm.mu.Unlock()
}

func (gm *GlobalManager) RegisterActiveBody(body io.Closer) {
	if body == nil {
		return
	}
	gm.mu.Lock()
	gm.activeBodies = append(gm.activeBodies, body)
	gm.mu.Unlock()
}

func (gm *GlobalManager) ClearActiveBodies() {
	gm.mu.Lock()
	gm.activeBodies = nil
	gm.mu.Unlock()
}

func (gm *GlobalManager) SetCreatedRoot(dir string) {
	gm.mu.Lock()
	gm.createdRoot = dir
	gm.mu.Unlock()
}

func (gm *GlobalManager) InstantAbort() {
	gm.mu.Lock()
	if gm.cancelFunc != nil {
		gm.cancelFunc()
	}
	for _, body := range gm.activeBodies {
		if body != nil {
			_ = body.Close()
		}
	}
	gm.activeBodies = nil
	if gm.activeFile != nil {
		_ = gm.activeFile.Close()
	}
	gm.mu.Unlock()
}

func (gm *GlobalManager) AbortCleanup(baseOutputDir string) {
	gm.mu.Lock()
	fileToRemove := gm.activePath
	rootToRemove := gm.createdRoot
	gm.activeFile = nil
	gm.activePath = ""
	gm.createdRoot = ""
	gm.mu.Unlock()

	if fileToRemove != "" {
		_ = os.Remove(fileToRemove)
	}
	if rootToRemove != "" && filepath.Clean(rootToRemove) != filepath.Clean(baseOutputDir) {
		removeEmptyDirsBottomUp(rootToRemove)
		if entries, err := os.ReadDir(rootToRemove); err == nil && len(entries) == 0 {
			_ = os.Remove(rootToRemove)
		}
	}
}

func removeEmptyDirsBottomUp(root string) {
	var dirs []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() && path != root {
			dirs = append(dirs, path)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i])
	}
}

var (
	logMu      sync.Mutex
	appLogFile *os.File

	customConfigDirMu sync.RWMutex
	customConfigDir   string
	customDefaultDest string
	activeServerPort  int

	settingsMu      sync.RWMutex
	currentSettings AppSettings
)

func setCustomConfigDir(dir string) {
	customConfigDirMu.Lock()
	customConfigDir = dir
	customConfigDirMu.Unlock()
}

func setCustomDefaultDest(dest string) {
	customConfigDirMu.Lock()
	customDefaultDest = dest
	customConfigDirMu.Unlock()
}

func getAppConfigDir() string {
	customConfigDirMu.RLock()
	if customConfigDir != "" {
		dir := customConfigDir
		customConfigDirMu.RUnlock()
		_ = os.MkdirAll(dir, 0755)
		return dir
	}
	customConfigDirMu.RUnlock()

	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	cfgDir := filepath.Join(home, ".config", "directcrate")
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("APPDATA"); appData != "" {
			cfgDir = filepath.Join(appData, "directcrate")
		}
	}
	_ = os.MkdirAll(cfgDir, 0755)
	return cfgDir
}

func getSettingsFilePath() string {
	return filepath.Join(getAppConfigDir(), "settings.json")
}

func defaultAppSettings() AppSettings {
	return AppSettings{
		DefaultDest:       getDefaultDownloadsDir(),
		Workers:           4,
		BufferMB:          defaultBufferMB,
		ConflictMode:      "ask",
		ExcludePatterns:   "",
		FlattenSingleRoot: true,
		SpeedLimitMBs:     0,
		PowerAction:       "none",
		PlaySound:         true,
		DesktopNotify:     true,
		AutoSubfolder:     true,
		PreventIdle:       true,
		Theme:                     "system",
		AddSteam:                  false,
		LiveProgressNotifications: true,
	}
}

func loadSettings() AppSettings {
	settingsMu.RLock()
	defer settingsMu.RUnlock()

	cfg := defaultAppSettings()
	path := getSettingsFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		bakPath := path + ".corrupt.bak"
		_ = os.WriteFile(bakPath, data, 0644)
		log.Printf("[SETTINGS:WARN] Corrupted settings JSON in %s (%v); backed up to %s, using defaults", path, err, bakPath)
		return defaultAppSettings()
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.BufferMB <= 0 {
		cfg.BufferMB = defaultBufferMB
	}
	if cfg.ConflictMode == "" {
		cfg.ConflictMode = "ask"
	}
	if cfg.DefaultDest == "" {
		cfg.DefaultDest = getDefaultDownloadsDir()
	}
	return cfg
}

func saveSettings(cfg AppSettings) error {
	settingsMu.Lock()
	defer settingsMu.Unlock()

	if cfg.DefaultDest == "" {
		cfg.DefaultDest = getDefaultDownloadsDir()
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.BufferMB <= 0 {
		cfg.BufferMB = defaultBufferMB
	}
	if cfg.ConflictMode == "" {
		cfg.ConflictMode = "ask"
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	path := getSettingsFilePath()
	if err := os.WriteFile(path, data, 0644); err != nil {
		return err
	}
	currentSettings = cfg

	if cfg.SpeedLimitMBs > 0 {
		globalSpeedLimitBytesPerSec.Store(int64(cfg.SpeedLimitMBs) * 1024 * 1024)
	} else {
		globalSpeedLimitBytesPerSec.Store(0)
	}

	manager.Update(func(s *ExtractionState) {
		s.SpeedLimitMBs = cfg.SpeedLimitMBs
		s.LiveProgressNotifications = cfg.LiveProgressNotifications
	})

	log.Printf("[SETTINGS] Configuration persisted: Buffer=%dMB, SpeedLimit=%dMB/s, Dest=%q",
		cfg.BufferMB, cfg.SpeedLimitMBs, cfg.DefaultDest)
	return nil
}

func getHistoryFilePath() string {
	return filepath.Join(getAppConfigDir(), "history.json")
}

func getLogFilePath() string {
	return filepath.Join(getAppConfigDir(), "directcrate.log")
}

func getCrashLogFilePath() string {
	return filepath.Join(getAppConfigDir(), "crash.log")
}

func rotateLogIfExceeds(logPath string, maxBytes int64) {
	fi, err := os.Stat(logPath)
	if err != nil || fi.Size() < maxBytes {
		return
	}
	oldPath := logPath + ".old"
	_ = os.Remove(oldPath)
	_ = os.Rename(logPath, oldPath)
}

func initLogging() {
	logMu.Lock()
	defer logMu.Unlock()

	logPath := getLogFilePath()
	rotateLogIfExceeds(logPath, 5*1024*1024)

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.SetOutput(os.Stdout)
		return
	}
	appLogFile = f
	multi := io.MultiWriter(os.Stdout, f)
	log.SetOutput(multi)
	log.SetFlags(log.Ldate | log.Ltime)
}

func clearLogs() error {
	logMu.Lock()
	defer logMu.Unlock()

	if appLogFile != nil {
		_ = appLogFile.Close()
		appLogFile = nil
	}

	logPath := getLogFilePath()
	crashPath := getCrashLogFilePath()
	_ = os.Remove(logPath)
	_ = os.Remove(logPath + ".old")
	_ = os.Remove(crashPath)

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		appLogFile = f
		log.SetOutput(io.MultiWriter(os.Stdout, f))
	} else {
		log.SetOutput(os.Stdout)
	}
	return nil
}

func formatCrashReport(contextName string, panicVal interface{}, stack []byte) string {
	var b strings.Builder
	b.WriteString("================================================================================\n")
	b.WriteString(fmt.Sprintf("DIRECTCRATE CRASH REPORT — %s\n", time.Now().Format("2006-01-02 15:04:05 MST")))
	b.WriteString(fmt.Sprintf("Version:   %s (%s/%s)\n", appVersion, runtime.GOOS, runtime.GOARCH))
	b.WriteString(fmt.Sprintf("Context:   %s\n", contextName))
	b.WriteString(fmt.Sprintf("Panic:     %v\n", panicVal))
	b.WriteString("--------------------------------- STACK TRACE ----------------------------------\n")
	b.Write(stack)
	b.WriteString("\n================================================================================\n\n")
	return b.String()
}

func recoverAndLogPanic(contextName string) {
	if r := recover(); r != nil {
		stack := debug.Stack()
		report := formatCrashReport(contextName, r, stack)

		log.Printf("[CRASH:PANIC] Trapped in %s: %v", contextName, r)
		fmt.Fprint(os.Stderr, report)

		crashPath := getCrashLogFilePath()
		if cf, err := os.OpenFile(crashPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			_, _ = cf.WriteString(report)
			_ = cf.Close()
		}

		manager.Update(func(s *ExtractionState) {
			s.Status = "error"
			s.Message = fmt.Sprintf("Fatal internal error trapped in %s: %v (see crash.log)", contextName, r)
		})
	}
}

func readTailLines(filePath string, maxLines int) ([]string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > maxLines*2 {
			lines = lines[len(lines)-maxLines:]
		}
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines, scanner.Err()
}

func appendHistory(entry HistoryEntry) {
	path := getHistoryFilePath()
	var list []HistoryEntry
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &list); err != nil {
			bakPath := path + ".corrupt.bak"
			_ = os.WriteFile(bakPath, data, 0644)
			log.Printf("[HISTORY:WARN] Corrupted history JSON in %s (%v); backed up to %s", path, err, bakPath)
			list = []HistoryEntry{}
		}
	}

	list = append([]HistoryEntry{entry}, list...)
	if len(list) > 100 {
		list = list[:100]
	}

	data, err := json.MarshalIndent(list, "", "  ")
	if err == nil {
		_ = os.WriteFile(path, data, 0644)
	}
}

func executeTask(opts TaskOptions) {
	defer recoverAndLogPanic("task:execute")
	defer func() {
		manager.checkAndScheduleIdleShutdown()
	}()
	manager.cancelIdleTimer()

	if opts.BufferSizeMB <= 0 {
		opts.BufferSizeMB = defaultBufferMB
	}
	if opts.ParallelConns <= 0 {
		opts.ParallelConns = 4
	}
	if opts.ConflictPolicy == "" {
		opts.ConflictPolicy = "merge"
	}
	if opts.PowerAction == "" {
		opts.PowerAction = "none"
	}
	if opts.SpeedLimitMBs < 0 {
		opts.SpeedLimitMBs = 0
	}
	if opts.SpeedLimitMBs > 0 {
		globalSpeedLimitBytesPerSec.Store(int64(opts.SpeedLimitMBs) * 1024 * 1024)
	}

	urls := parseURLList(opts.URL)
	if len(urls) == 0 {
		return
	}
	log.Printf("[TASK:START] Dest=%q, Parts=%d, Buffer=%dMB, Conns=%d, Conflict=%s", opts.Dest, len(urls), opts.BufferSizeMB, opts.ParallelConns, opts.ConflictPolicy)

	excludePatterns := compileExcludePatterns(opts.ExcludePatterns)
	selectionSet := buildSelectionSet(opts.SelectedFiles)

	ctx, cancel := context.WithCancel(context.Background())
	manager.ResetURLWait()
	manager.mu.Lock()
	manager.cancelFunc = cancel
	manager.lastTaskOpts = opts
	manager.createdRoot = ""
	manager.activeBodies = nil
	manager.mu.Unlock()

	defer func() {
		manager.ResetURLWait()
		manager.mu.Lock()
		manager.cancelFunc = nil
		manager.activeBodies = nil
		manager.mu.Unlock()
		cancel()
	}()

	if opts.PreventIdle {
		startIdleInhibitor(ctx)
	}

	fallbackFolderName := deriveFallbackName(urls[0])

	manager.Update(func(s *ExtractionState) {
		s.Status = "running"
		s.Message = fmt.Sprintf("Validating storage & probing %d archive part(s)...", len(urls))
		s.CurrentFile = "Probing remote headers..."
		s.CurrentPart = 1
		s.TotalParts = len(urls)
		s.DownloadedMB = 0
		s.TotalMB = 0
		s.HasContentLen = false
		s.Percent = 0
		s.SpeedMBs = 0
		s.PeakSpeedMBs = 0
		s.ETA = "--:--"
		s.ExtractedFiles = 0
		s.SkippedFiles = 0
		s.UncompressedMB = 0
		s.UncompressedGB = 0
		s.FinalDest = opts.Dest
		s.IntegrityStatus = "Pending"
		s.DetectedExes = nil
		s.ActiveStreams = opts.ParallelConns
		s.RetryCount = 0
	})

	if opts.Dest == "" {
		opts.Dest = getDefaultDownloadsDir()
	}
	if err := os.MkdirAll(opts.Dest, 0755); err != nil {
		log.Printf("[TASK:ERROR] Failed creating dest dir %s: %v", opts.Dest, err)
		manager.Update(func(s *ExtractionState) {
			s.Status = "error"
			s.Message = fmt.Sprintf("Failed to initialize destination directory: %v", err)
		})
		return
	}

	// Test destination directory writability by creating and deleting a temporary probe file
	probeFile := filepath.Join(opts.Dest, fmt.Sprintf(".directcrate_write_probe_%d.tmp", time.Now().UnixNano()))
	if err := os.WriteFile(probeFile, []byte("ok"), 0644); err != nil {
		log.Printf("[TASK:ERROR] Destination directory %s is not writable: %v", opts.Dest, err)
		manager.Update(func(s *ExtractionState) {
			s.Status = "error"
			s.Message = fmt.Sprintf("Destination directory '%s' is not writable (permission denied): %v", opts.Dest, err)
		})
		return
	}
	_ = os.Remove(probeFile)

	freeBytes, err := getFreeDiskSpace(opts.Dest)
	if err == nil {
		manager.Update(func(s *ExtractionState) {
			s.FreeDiskGB = float64(freeBytes) / (1024 * 1024 * 1024)
		})
	}

	probeClient := createOptimizedHTTPClient(30 * time.Second)
	resp, resolvedURL, contentLength, err := probeRemoteURL(ctx, probeClient, urls[0], opts.Headers)
	if err != nil {
		log.Printf("[TASK:ERROR] Target probe unreachable %s: %v", urls[0], err)
		manager.Update(func(s *ExtractionState) {
			s.Status = "error"
			s.Message = fmt.Sprintf("Failed to reach archive server: %v", err)
		})
		return
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		_ = resp.Body.Close()
		log.Printf("[TASK:ERROR] Target probe returned HTTP %s for %s", resp.Status, urls[0])
		manager.Update(func(s *ExtractionState) {
			s.Status = "error"
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone {
				s.Message = "Download link expired or blocked by host. Refresh the link and click Retry to resume."
			} else {
				s.Message = fmt.Sprintf("Archive server returned HTTP error: %s", resp.Status)
			}
		})
		return
	}

	if contentLength > 0 && freeBytes > 0 && freeBytes < uint64(contentLength) {
		_ = resp.Body.Close()
		neededGB := float64(contentLength) / (1024 * 1024 * 1024)
		availGB := float64(freeBytes) / (1024 * 1024 * 1024)
		log.Printf("[TASK:ERROR] Insufficient disk space in %s: need >= %.2f GB, only %.2f GB available", opts.Dest, neededGB, availGB)
		manager.Update(func(s *ExtractionState) {
			s.Status = "error"
			s.Message = "Not enough free storage space. Free up space on your device and click Retry."
		})
		return
	}

	magicBuffer := make([]byte, 16)
	var magicRead int
	var probeRespHeader = resp.Header
	if resp.Request != nil && resp.Request.Method == "HEAD" {
		_ = resp.Body.Close()
		getReq, err := http.NewRequestWithContext(ctx, "GET", resolvedURL, nil)
		if err == nil {
			applyCustomHeaders(getReq, opts.Headers)
			getReq.Header.Set("Range", "bytes=0-15")
			if getResp, err := probeClient.Do(getReq); err == nil {
				probeRespHeader = getResp.Header
				magicRead, _ = io.ReadFull(getResp.Body, magicBuffer)
				_ = getResp.Body.Close()
			}
		}
	} else {
		magicRead, _ = io.ReadFull(resp.Body, magicBuffer)
		_ = resp.Body.Close()
	}

	contentType := probeRespHeader.Get("Content-Type")
	isHTML := strings.Contains(strings.ToLower(contentType), "text/html") ||
		bytes.HasPrefix(bytes.TrimSpace(magicBuffer[:magicRead]), []byte("<!DOCTYPE")) ||
		bytes.HasPrefix(bytes.TrimSpace(magicBuffer[:magicRead]), []byte("<html")) ||
		bytes.HasPrefix(bytes.TrimSpace(magicBuffer[:magicRead]), []byte("<HTML"))

	archiveType := detectFormat(magicBuffer[:magicRead], resolvedURL)
	if archiveType == ArchiveUnknown {
		log.Printf("[TASK:ERROR] Unsupported archive format at %s (isHTML=%v)", resolvedURL, isHTML)
		manager.Update(func(s *ExtractionState) {
			s.Status = "error"
			if isHTML {
				s.Message = "This link opened a webpage instead of a direct archive file. Copy the direct download link and try again."
			} else {
				s.Message = "Unsupported archive format. Supported formats: RAR5, ZIP, 7Z, TAR.GZ, TAR.ZST."
			}
		})
		return
	}

	var extractedCount, skippedCount int
	var totalUncompressedBytes int64
	var finalExtractedDir string
	var extractionErr error

	startTime := time.Now()
	speedCalc := newRollingSpeedCalculator()
	var lastTelemetryTime = startTime
	var peakSpeed float64
	var currentTotalRead int64

	telemetryCallback := func(n int64) {
		manager.mu.Lock()
		st := manager.state.Status
		manager.mu.Unlock()
		if st == "aborted" || st == "error" {
			return
		}

		now := time.Now()
		currentTotalRead += n
		rollingSpeedMBs := speedCalc.addSample(now, currentTotalRead)

		elapsedSinceLast := now.Sub(lastTelemetryTime).Seconds()
		if elapsedSinceLast >= 0.20 { // 200ms throttle (at most 5 updates/second)
			if rollingSpeedMBs > peakSpeed {
				peakSpeed = rollingSpeedMBs
			}

			var pct float64
			var etaStr = "--:--"
			if contentLength > 0 {
				pct = (float64(currentTotalRead) / float64(contentLength)) * 100
				remBytes := contentLength - currentTotalRead
				speedBytesSec := rollingSpeedMBs * 1024 * 1024
				if speedBytesSec > 0 && remBytes > 0 {
					etaSec := int(float64(remBytes) / speedBytesSec)
					etaStr = fmt.Sprintf("%02d:%02d", etaSec/60, etaSec%60)
				}
			}

			memMB := sampleProcessMemMB()
			tempC, thermStatus := sampleDeviceTemperature()

			manager.Update(func(s *ExtractionState) {
				s.DownloadedMB = float64(currentTotalRead) / (1024 * 1024)
				s.TotalMB = float64(contentLength) / (1024 * 1024)
				s.HasContentLen = contentLength > 0
				s.Percent = pct
				s.SpeedMBs = rollingSpeedMBs
				s.PeakSpeedMBs = peakSpeed
				s.ETA = etaStr
				s.MemMB = memMB
				if runtime.GOOS == "linux" && tempC > 0 {
					s.TempC = tempC
					s.ThermalStatus = thermStatus
				}
			})

			lastTelemetryTime = now
		}
	}

	switch archiveType {
	case ArchiveRAR5, ArchiveTar, ArchiveTarGz, ArchiveTarZst:
		streamClient := createStreamingHTTPClient(0)
		multiReader := NewMultiPartStreamReader(ctx, streamClient, urls, opts.ParallelConns, opts.Headers, telemetryCallback)
		defer multiReader.Close()

		if archiveType == ArchiveRAR5 {
			extractedCount, skippedCount, totalUncompressedBytes, finalExtractedDir, extractionErr = extractRAR(
				ctx, multiReader, opts.Dest, opts.AutoSubfolder, opts.CustomFolder, opts.ConflictPolicy, fallbackFolderName, opts.Password, opts.BufferSizeMB, excludePatterns,
			)
		} else {
			extractedCount, skippedCount, totalUncompressedBytes, finalExtractedDir, extractionErr = extractTarStream(
				ctx, multiReader, archiveType, opts.Dest, opts.AutoSubfolder, opts.CustomFolder, opts.ConflictPolicy, fallbackFolderName, opts.BufferSizeMB, excludePatterns,
			)
		}

	case ArchiveZIP:
		rangeClient := createOptimizedHTTPClient(30 * time.Second)
		extractedCount, skippedCount, totalUncompressedBytes, finalExtractedDir, extractionErr = extractZIP(
			ctx, rangeClient, resolvedURL, contentLength, opts.Dest, opts.AutoSubfolder, opts.CustomFolder, opts.ConflictPolicy, fallbackFolderName, opts.BufferSizeMB, excludePatterns, selectionSet, opts.Headers,
		)

	case Archive7Z:
		rangeClient := createOptimizedHTTPClient(30 * time.Second)
		extractedCount, skippedCount, totalUncompressedBytes, finalExtractedDir, extractionErr = extract7Z(
			ctx, rangeClient, resolvedURL, contentLength, opts.Dest, opts.AutoSubfolder, opts.CustomFolder, opts.ConflictPolicy, fallbackFolderName, opts.Password, opts.BufferSizeMB, excludePatterns, selectionSet, opts.Headers,
		)
	}

	duration := time.Since(startTime).Seconds()
	if duration <= 0 {
		duration = 0.001
	}
	avgSpeedMBs := (float64(totalUncompressedBytes) / (1024 * 1024)) / duration

	if extractionErr != nil {
		if errors.Is(extractionErr, errPasswordRequired) {
			log.Printf("[TASK:AUTH] Password required for archive: %s", urls[0])
			manager.Update(func(s *ExtractionState) {
				s.Status = "needs_password"
				s.Message = "Archive is encrypted. Enter password to resume extraction."
			})
			return
		}

		if ctx.Err() != nil {
			log.Printf("[TASK:ABORT] Task aborted by user: %s", urls[0])
			manager.AbortCleanup(opts.Dest)
			manager.Update(func(s *ExtractionState) {
				s.Status = "aborted"
				s.Message = "Task was aborted. Incomplete files cleaned up."
			})
			return
		}

		log.Printf("[TASK:ERROR] Extraction failed: %v", extractionErr)
		manager.Update(func(s *ExtractionState) {
			s.Status = "error"
			s.Message = fmt.Sprintf("Extraction failed: %v", extractionErr)
		})
		return
	}

	if opts.FlattenSingleRoot && finalExtractedDir != "" {
		_ = flattenSingleRootDir(finalExtractedDir)
	}

	var detectedExes []string
	if finalExtractedDir != "" {
		detectedExes = detectPrimaryExecutables(finalExtractedDir)
	}

	integrityStatus := "Skipped"
	if finalExtractedDir != "" {
		integrityStatus = verifyChecksums(finalExtractedDir)
	}

	folderName := filepath.Base(finalExtractedDir)
	if folderName == "" || folderName == "." {
		folderName = fallbackFolderName
	}

	entry := HistoryEntry{
		Timestamp:       time.Now().Format("2006-01-02 15:04:05"),
		FolderName:      folderName,
		FinalDest:       finalExtractedDir,
		DetectedExes:    detectedExes,
		UncompressedGB:  float64(totalUncompressedBytes) / (1024 * 1024 * 1024),
		ExtractedFiles:  extractedCount,
		SkippedFiles:    skippedCount,
		AverageSpeedMBs: avgSpeedMBs,
		DurationSec:     duration,
		IntegrityStatus: integrityStatus,
	}
	appendHistory(entry)

	log.Printf("[TASK:DONE] Unpacked %d files (%.2f GB) in %.1fs (%.1f MB/s avg) to %s. Integrity=%s",
		extractedCount, entry.UncompressedGB, duration, avgSpeedMBs, finalExtractedDir, integrityStatus)

	manager.Update(func(s *ExtractionState) {
		s.Status = "completed"
		s.Message = fmt.Sprintf("Unpacked %d file(s) directly to disk.", extractedCount)
		s.CurrentFile = "Done"
		s.ExtractedFiles = extractedCount
		s.SkippedFiles = skippedCount
		s.Percent = 100
		s.SpeedMBs = 0
		s.ETA = "00:00"
		s.UncompressedGB = entry.UncompressedGB
		s.TimeTakenSec = duration
		s.AverageSpeedMBs = avgSpeedMBs
		s.FinalDest = finalExtractedDir
		s.IntegrityStatus = integrityStatus
		s.DetectedExes = detectedExes
	})

	if opts.PlaySound {
		playCompletionSound()
	}
	sendDesktopNotification("DirectCrate Extraction Complete", fmt.Sprintf("Successfully unpacked '%s' (%.2f GB)", folderName, entry.UncompressedGB), "normal")

	if nextTask, ok := manager.PopNextQueue(); ok {
		log.Printf("[QUEUE:NEXT] Automatically advancing to queued task %s (%s)", nextTask.ID, nextTask.Label)
		go executeTask(nextTask.Options)
		return
	}

	if opts.PowerAction != "none" {
		triggerPowerAction(opts.PowerAction)
	}
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	manager.mu.Lock()
	manager.state.MemMB = sampleProcessMemMB()
	if runtime.GOOS == "linux" && manager.state.TempC == 0 {
		if t, st := sampleDeviceTemperature(); t > 0 {
			manager.state.TempC = t
			manager.state.ThermalStatus = st
		}
	}
	st := manager.state
	manager.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

func handleThermals(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		TempC         float64 `json:"temp_c"`
		ThermalStatus string  `json:"thermal_status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	manager.Update(func(s *ExtractionState) {
		s.TempC = payload.TempC
		s.ThermalStatus = payload.ThermalStatus
	})
	w.WriteHeader(http.StatusOK)
}

func handleCheckFolder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		Dest         string `json:"dest"`
		CustomFolder string `json:"custom_folder"`
		URL          string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if payload.Dest == "" {
		payload.Dest = getDefaultDownloadsDir()
	}
	folderName := sanitizeFolderName(payload.CustomFolder)
	if strings.TrimSpace(payload.CustomFolder) == "" {
		urls := parseURLList(payload.URL)
		if len(urls) > 0 {
			folderName = deriveFallbackName(urls[0])
		} else {
			folderName = ""
		}
	}

	exists := false
	fileCount := 0
	targetPath := payload.Dest
	if folderName != "" {
		targetPath = filepath.Join(payload.Dest, folderName)
		if entries, err := os.ReadDir(targetPath); err == nil && len(entries) > 0 {
			exists = true
			fileCount = len(entries)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"exists":      exists,
		"file_count":  fileCount,
		"target_path": targetPath,
	})
}

func handleDefaults(w http.ResponseWriter, r *http.Request) {
	home, _ := os.UserHomeDir()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"default_dest": getDefaultDownloadsDir(),
		"home_dir":     home,
		"os":           runtime.GOOS,
		"arch":         runtime.GOARCH,
		"is_android":   runtime.GOOS == "android",
		"version":      appVersion,
		"log_path":     getLogFilePath(),
		"lan_urls":     getLANURLs(activeServerPort),
		"settings":     loadSettings(),
	})
}

func handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := loadSettings()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cfg)

	case http.MethodDelete:
		cfgPath := getSettingsFilePath()
		_ = os.Remove(cfgPath)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(defaultAppSettings())

	case http.MethodPost:
		cfg := loadSettings()
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if cfg.DefaultDest == "" {
			cfg.DefaultDest = getDefaultDownloadsDir()
		}
		if cfg.Workers <= 0 {
			cfg.Workers = 4
		}
		if cfg.BufferMB <= 0 {
			cfg.BufferMB = defaultBufferMB
		}
		if cfg.ConflictMode == "" {
			cfg.ConflictMode = "merge"
		}
		if err := saveSettings(cfg); err != nil {
			http.Error(w, fmt.Sprintf("Failed to save settings: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cfg)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload struct {
		URL     string            `json:"url"`
		Dest    string            `json:"dest"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	urls := parseURLList(payload.URL)
	if len(urls) == 0 {
		http.Error(w, "URL is required", http.StatusBadRequest)
		return
	}
	if payload.Dest == "" {
		payload.Dest = getDefaultDownloadsDir()
	}

	res, err := inspectRemoteArchive(r.Context(), urls, payload.Dest, payload.Headers)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

type UpdateCheckResponse struct {
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version"`
	UpdateAvailable bool   `json:"update_available"`
	ReleaseURL      string `json:"release_url"`
	ReleaseNotes    string `json:"release_notes"`
	Error           string `json:"error,omitempty"`
}

var githubReleaseURL = "https://api.github.com/repos/musakpg/directcrate/releases/latest"

func compareSemVer(v1, v2 string) int {
	clean := func(v string) []int {
		v = strings.TrimSpace(v)
		v = strings.TrimPrefix(v, "v")
		v = strings.TrimPrefix(v, "V")
		if idx := strings.IndexAny(v, "-+"); idx != -1 {
			v = v[:idx]
		}
		parts := strings.Split(v, ".")
		res := make([]int, len(parts))
		for i, p := range parts {
			val, _ := strconv.Atoi(strings.TrimSpace(p))
			res[i] = val
		}
		return res
	}

	p1 := clean(v1)
	p2 := clean(v2)
	maxLen := len(p1)
	if len(p2) > maxLen {
		maxLen = len(p2)
	}

	for i := 0; i < maxLen; i++ {
		var n1, n2 int
		if i < len(p1) {
			n1 = p1[i]
		}
		if i < len(p2) {
			n2 = p2[i]
		}
		if n1 > n2 {
			return 1
		}
		if n1 < n2 {
			return -1
		}
	}
	return 0
}

func handleCheckUpdate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	resp := UpdateCheckResponse{
		CurrentVersion: appVersion,
		ReleaseURL:     "https://github.com/musakpg/directcrate/releases",
	}

	client := &http.Client{
		Timeout: 5 * time.Second,
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, githubReleaseURL, nil)
	if err != nil {
		resp.Error = err.Error()
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	req.Header.Set("User-Agent", "DirectCrate-Updater")
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	httpResp, err := client.Do(req)
	if err != nil {
		resp.Error = err.Error()
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		resp.Error = fmt.Sprintf("GitHub API returned HTTP %s", httpResp.Status)
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	var ghRelease struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Body    string `json:"body"`
	}
	if err := json.NewDecoder(httpResp.Body).Decode(&ghRelease); err != nil {
		resp.Error = fmt.Sprintf("failed to parse release JSON: %v", err)
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	resp.LatestVersion = ghRelease.TagName
	if ghRelease.HTMLURL != "" {
		resp.ReleaseURL = ghRelease.HTMLURL
	}
	resp.ReleaseNotes = ghRelease.Body

	if compareSemVer(ghRelease.TagName, appVersion) > 0 {
		resp.UpdateAvailable = true
	}

	_ = json.NewEncoder(w).Encode(resp)
}

func handleUnlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	manager.mu.RLock()
	isRunning := manager.state.Status == "running"
	opts := manager.lastTaskOpts
	manager.mu.RUnlock()

	if isRunning {
		http.Error(w, "Extraction already running", http.StatusConflict)
		return
	}

	var payload struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	opts.Password = payload.Password
	go executeTask(opts)
	w.WriteHeader(http.StatusOK)
}

func isFlatpakSandbox() bool {
	if os.Getenv("FLATPAK_ID") != "" {
		return true
	}
	_, err := os.Stat("/.flatpak-info")
	return err == nil
}

func openFolderInOS(folderPath string) error {
	if flag.Lookup("test.v") != nil {
		return nil
	}
	cleanPath := filepath.Clean(folderPath)
	if runtime.GOOS == "windows" {
		return exec.Command("explorer", cleanPath).Start()
	} else if runtime.GOOS == "darwin" {
		return exec.Command("open", cleanPath).Start()
	} else if runtime.GOOS == "android" {
		return nil
	}

	if isFlatpakSandbox() {
		if err := exec.Command("flatpak-spawn", "--host", "xdg-open", cleanPath).Start(); err == nil {
			return nil
		}
	}

	if err := exec.Command("xdg-open", cleanPath).Start(); err == nil {
		return nil
	}

	if err := exec.Command("gio", "open", cleanPath).Start(); err == nil {
		return nil
	}

	for _, cmd := range []string{"nautilus", "dolphin", "thunar", "pcmanfm"} {
		if err := exec.Command(cmd, cleanPath).Start(); err == nil {
			return nil
		}
	}

	return fmt.Errorf("no supported file manager found to open %s", cleanPath)
}

func handleOpenFolder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Path == "" {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}
	err := openFolderInOS(payload.Path)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func handleAddToSteam(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		ExePath string `json:"exe_path"`
		AppName string `json:"app_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if payload.AppName == "" {
		payload.AppName = filepath.Base(filepath.Dir(payload.ExePath))
	}

	msg, err := addNonSteamGameToSteam(payload.ExePath, payload.AppName)
	if err != nil {
		log.Printf("[STEAM:ERROR] Failed adding %q (%s) to Steam: %v", payload.AppName, payload.ExePath, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[STEAM:SUCCESS] %s", msg)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

func handleHistory(w http.ResponseWriter, r *http.Request) {
	path := getHistoryFilePath()
	if r.Method == http.MethodDelete {
		_ = os.WriteFile(path, []byte("[]"), 0644)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		return
	}
	var list []HistoryEntry
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &list); err != nil {
			bakPath := path + ".corrupt.bak"
			_ = os.WriteFile(bakPath, data, 0644)
			log.Printf("[HISTORY:WARN] Corrupted history JSON in %s (%v); backed up to %s", path, err, bakPath)
			list = []HistoryEntry{}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

func handleLogs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		logPath := getLogFilePath()
		lines, err := readTailLines(logPath, 200)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to read log file: %v", err), http.StatusInternalServerError)
			return
		}
		content := strings.Join(lines, "\n")

		crashPath := getCrashLogFilePath()
		if crashData, crashErr := os.ReadFile(crashPath); crashErr == nil && len(bytes.TrimSpace(crashData)) > 0 {
			if content != "" {
				content += "\n\n"
			}
			content += "=== RECENT CRASH REPORTS (crash.log) ===\n" + strings.TrimSpace(string(crashData)) + "\n"
		}

		if strings.TrimSpace(content) == "" {
			content = "(No log entries recorded yet)"
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"path":    logPath,
			"content": content,
		})

	case http.MethodDelete:
		if err := clearLogs(); err != nil {
			http.Error(w, fmt.Sprintf("Failed to clear logs: %v", err), http.StatusInternalServerError)
			return
		}
		log.Printf("[SYSTEM] Diagnostic and crash logs cleared by user.")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message": "Logs cleared",
		})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

type ShortcutItem struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type DirItem struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
}

func getBrowseShortcuts() []ShortcutItem {
	var shortcuts []ShortcutItem

	// 1. Host user home directory
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cleanHome := filepath.Clean(home)
		if fi, err := os.Stat(cleanHome); err == nil && fi.IsDir() {
			shortcuts = append(shortcuts, ShortcutItem{
				Name: "Home",
				Path: cleanHome,
			})
		}
	}

	// 2. Host Downloads directory
	if d := getDefaultDownloadsDir(); d != "" {
		cleanD := filepath.Clean(d)
		if fi, err := os.Stat(cleanD); err == nil && fi.IsDir() {
			shortcuts = append(shortcuts, ShortcutItem{
				Name: "Downloads",
				Path: cleanD,
			})
		}
	} else if home, err := os.UserHomeDir(); err == nil && home != "" {
		dPath := filepath.Join(home, "Downloads")
		if fi, err := os.Stat(dPath); err == nil && fi.IsDir() {
			shortcuts = append(shortcuts, ShortcutItem{
				Name: "Downloads",
				Path: dPath,
			})
		}
	}

	// 3. Linux / SteamOS / Bazzite media & mounted drives
	if runtime.GOOS == "linux" {
		mediaRoots := []string{"/run/media", "/media", "/mnt"}
		for _, mRoot := range mediaRoots {
			entries, err := os.ReadDir(mRoot)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
					continue
				}
				subPath := filepath.Join(mRoot, e.Name())
				// Check if subPath contains user mount points (e.g. /run/media/deck/...)
				deeper, errD := os.ReadDir(subPath)
				if errD == nil && len(deeper) > 0 {
					foundDrives := false
					for _, d := range deeper {
						if d.IsDir() && !strings.HasPrefix(d.Name(), ".") {
							mountPath := filepath.Join(subPath, d.Name())
							shortcuts = append(shortcuts, ShortcutItem{
								Name: fmt.Sprintf("Drive (%s)", d.Name()),
								Path: mountPath,
							})
							foundDrives = true
						}
					}
					if !foundDrives {
						shortcuts = append(shortcuts, ShortcutItem{
							Name: fmt.Sprintf("Media (%s)", e.Name()),
							Path: subPath,
						})
					}
				} else {
					shortcuts = append(shortcuts, ShortcutItem{
						Name: fmt.Sprintf("Media (%s)", e.Name()),
						Path: subPath,
					})
				}
			}
		}

		// Root filesystem
		if fi, err := os.Stat("/"); err == nil && fi.IsDir() {
			shortcuts = append(shortcuts, ShortcutItem{
				Name: "Root (/)",
				Path: "/",
			})
		}
	}

	// 4. Windows Drives (C:\, D:\, etc.)
	if runtime.GOOS == "windows" {
		for c := 'C'; c <= 'Z'; c++ {
			drive := fmt.Sprintf("%c:\\", c)
			if fi, err := os.Stat(drive); err == nil && fi.IsDir() {
				shortcuts = append(shortcuts, ShortcutItem{
					Name: fmt.Sprintf("Drive (%c:)", c),
					Path: drive,
				})
			}
		}
	}

	// 5. Android Storage Roots
	if runtime.GOOS == "android" {
		androidPaths := []struct{ name, path string }{
			{"Downloads", "/storage/emulated/0/Download"},
			{"Internal Storage", "/storage/emulated/0"},
			{"SD Card", "/sdcard"},
		}
		for _, ap := range androidPaths {
			if fi, err := os.Stat(ap.path); err == nil && fi.IsDir() {
				shortcuts = append(shortcuts, ShortcutItem{
					Name: ap.name,
					Path: ap.path,
				})
			}
		}
	}

	// Deduplicate shortcuts by normalized path
	var unique []ShortcutItem
	seen := make(map[string]bool)
	for _, sc := range shortcuts {
		clean := filepath.Clean(sc.Path)
		if !seen[clean] {
			seen[clean] = true
			unique = append(unique, ShortcutItem{
				Name: sc.Name,
				Path: clean,
			})
		}
	}
	return unique
}

func resolveValidBrowsePath(rawPath string) string {
	raw := strings.TrimSpace(rawPath)
	if raw != "" {
		clean := filepath.Clean(raw)
		if fi, err := os.Stat(clean); err == nil && fi.IsDir() {
			return clean
		}
		// If file or does not exist, walk up parent directories
		curr := clean
		for {
			parent := filepath.Dir(curr)
			if parent == curr || parent == "." || parent == "/" || parent == "\\" {
				if fi, err := os.Stat(parent); err == nil && fi.IsDir() {
					return parent
				}
				break
			}
			if fi, err := os.Stat(parent); err == nil && fi.IsDir() {
				return parent
			}
			curr = parent
		}
	}

	// Fallback to configured or detected downloads directory
	if d := getDefaultDownloadsDir(); d != "" {
		cleanD := filepath.Clean(d)
		if fi, err := os.Stat(cleanD); err == nil && fi.IsDir() {
			return cleanD
		}
	}

	// Fallback to UserHomeDir
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cleanH := filepath.Clean(home)
		if fi, err := os.Stat(cleanH); err == nil && fi.IsDir() {
			return cleanH
		}
	}

	// Fallback to root or current directory
	if runtime.GOOS == "windows" {
		if sysDrive := os.Getenv("SystemDrive"); sysDrive != "" {
			drivePath := sysDrive + "\\"
			if fi, err := os.Stat(drivePath); err == nil && fi.IsDir() {
				return drivePath
			}
		}
		return "C:\\"
	}
	if fi, err := os.Stat("/"); err == nil && fi.IsDir() {
		return "/"
	}
	return "."
}

func handleBrowse(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		reqPath := resolveValidBrowsePath(r.URL.Query().Get("path"))

		var results []DirItem
		parent := filepath.Dir(reqPath)
		if parent != reqPath {
			results = append(results, DirItem{
				Name:  "..",
				Path:  parent,
				IsDir: true,
			})
		}

		entries, err := os.ReadDir(reqPath)
		if err != nil {
			// If reading fails (e.g. permission denied), fallback to default home
			fallbackPath := resolveValidBrowsePath("")
			if fallbackPath != reqPath {
				reqPath = fallbackPath
				entries, _ = os.ReadDir(reqPath)
				parent = filepath.Dir(reqPath)
				results = nil
				if parent != reqPath {
					results = append(results, DirItem{
						Name:  "..",
						Path:  parent,
						IsDir: true,
					})
				}
			}
		}

		var dirs []DirItem
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				dirs = append(dirs, DirItem{
					Name:  e.Name(),
					Path:  filepath.Join(reqPath, e.Name()),
					IsDir: true,
				})
			}
		}

		sort.Slice(dirs, func(i, j int) bool {
			return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name)
		})
		results = append(results, dirs...)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"current":   reqPath,
			"parent":    parent,
			"items":     results,
			"shortcuts": getBrowseShortcuts(),
		})

	case http.MethodPost:
		var payload struct {
			Parent string `json:"parent"`
			Name   string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		cleanName := strings.TrimSpace(payload.Name)
		if cleanName == "" || strings.ContainsAny(cleanName, `/\:*?"<>|`) || cleanName == "." || cleanName == ".." {
			http.Error(w, "Invalid directory name", http.StatusBadRequest)
			return
		}
		parentDir := resolveValidBrowsePath(payload.Parent)
		targetDir := filepath.Join(parentDir, cleanName)
		if err := os.MkdirAll(targetDir, 0755); err != nil {
			http.Error(w, fmt.Sprintf("Failed to create folder: %v", err), http.StatusInternalServerError)
			return
		}
		log.Printf("[BROWSE] Created new directory: %s", targetDir)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"path":    targetDir,
			"created": cleanName,
		})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleSpeedLimit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		SpeedLimitMBs int `json:"speed_limit_mbs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if payload.SpeedLimitMBs < 0 {
		http.Error(w, "speed_limit_mbs must be >= 0", http.StatusBadRequest)
		return
	}

	globalSpeedLimitBytesPerSec.Store(int64(payload.SpeedLimitMBs) * 1024 * 1024)
	manager.Update(func(s *ExtractionState) {
		s.SpeedLimitMBs = payload.SpeedLimitMBs
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int{"speed_limit_mbs": payload.SpeedLimitMBs})
}

func handleResumeURL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	newURL := strings.TrimSpace(payload.URL)
	if newURL == "" {
		http.Error(w, "URL is required", http.StatusBadRequest)
		return
	}

	if !manager.ProvideRefreshedURL(newURL) {
		http.Error(w, "No stream is currently waiting for a refreshed URL", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func handleQueue(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(manager.ListQueue())

	case http.MethodPost:
		var opts TaskOptions
		if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(parseURLList(opts.URL)) == 0 {
			http.Error(w, "URL is required", http.StatusBadRequest)
			return
		}
		if opts.Dest == "" {
			opts.Dest = getDefaultDownloadsDir()
		}
		manager.cancelIdleTimer()
		item := manager.EnqueueTask(opts)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(item)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleQueueItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/queue/")
	id = strings.TrimSpace(id)
	if id == "" {
		http.Error(w, "Queue item ID is required", http.StatusBadRequest)
		return
	}
	if !manager.RemoveQueuedTask(id) {
		http.Error(w, "Queue item not found", http.StatusNotFound)
		return
	}
	manager.checkAndScheduleIdleShutdown()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(manager.ListQueue())
}

//go:embed web/index.html
var htmlIndex []byte

//go:embed web/favicon.ico
var faviconBytes []byte

//go:embed web/icon.png
var iconPngBytes []byte

// parseListenAddr normalizes a -port flag value such as ":9090", "9090", or "127.0.0.1:9090".
func parseListenAddr(rawPort string) (string, int, error) {
	trimmed := strings.TrimSpace(rawPort)
	if trimmed == "" {
		trimmed = ":9090"
	}
	if !strings.Contains(trimmed, ":") {
		trimmed = ":" + trimmed
	}

	host, portStr, err := net.SplitHostPort(trimmed)
	if err != nil {
		return "", 0, err
	}

	portNum, err := strconv.Atoi(portStr)
	if err != nil || portNum < 0 || portNum > 65535 {
		return "", 0, fmt.Errorf("port must be an integer between 0 and 65535")
	}
	return host, portNum, nil
}

// isAddrInUse reports whether a bind error occurred because the target port is already occupied.
func isAddrInUse(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "address already in use") || strings.Contains(msg, "only one usage of each socket address")
}

// listenWithFallback binds to the requested port or falls back sequentially across the next 9 ports.
func listenWithFallback(host string, startPort int) (net.Listener, int, error) {
	const maxPortAttempts = 10
	attempts := maxPortAttempts
	if startPort == 0 {
		attempts = 1
	}

	for offset := 0; offset < attempts; offset++ {
		candidatePort := startPort + offset
		if candidatePort > 65535 {
			break
		}

		addr := net.JoinHostPort(host, strconv.Itoa(candidatePort))
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			actualPort := candidatePort
			if tcpAddr, ok := ln.Addr().(*net.TCPAddr); ok {
				actualPort = tcpAddr.Port
			}
			return ln, actualPort, nil
		}

		if !isAddrInUse(err) {
			return nil, 0, err
		}
	}

	endPort := startPort + attempts - 1
	if endPort > 65535 {
		endPort = 65535
	}
	return nil, 0, fmt.Errorf("ports %d-%d are all already in use", startPort, endPort)
}

func formatDisplayURL(host string, port int) string {
	if host == "" || host == "0.0.0.0" || host == "::" {
		return fmt.Sprintf("http://localhost:%d", port)
	}
	return fmt.Sprintf("http://%s", net.JoinHostPort(host, strconv.Itoa(port)))
}

type ProgressSummary struct {
	Active                    bool    `json:"active"`
	Paused                    bool    `json:"paused"`
	Filename                  string  `json:"filename"`
	Percent                   float64 `json:"percent"`
	Speed                     string  `json:"speed"`
	Status                    string  `json:"status"`
	QueuedCount               int     `json:"queued_count"`
	LiveProgressNotifications bool    `json:"live_progress_notifications"`
}

func getActiveProgressSummary() ProgressSummary {
	manager.mu.RLock()
	defer manager.mu.RUnlock()

	st := manager.state
	active := st.Status == "running" || st.Status == "needs_url" || st.Status == "needs_password" || st.Status == "needs_disk"
	paused := st.Status == "needs_url" || st.Status == "needs_password" || st.Status == "needs_disk"
	speedStr := fmt.Sprintf("%.1f MB/s", st.SpeedMBs)
	liveNotify := loadSettings().LiveProgressNotifications

	fn := st.CurrentFile
	if fn == "" && active {
		fn = st.Message
	}

	percent := st.Percent
	if percent < 0 {
		percent = 0
	} else if percent > 100 {
		percent = 100
	}

	return ProgressSummary{
		Active:                    active,
		Paused:                    paused,
		Filename:                  fn,
		Percent:                   percent,
		Speed:                     speedStr,
		Status:                    st.Status,
		QueuedCount:               len(manager.queue),
		LiveProgressNotifications: liveNotify,
	}
}

func getActiveProgressSummaryJSON() string {
	sum := getActiveProgressSummary()
	data, err := json.Marshal(sum)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func launchBrowser(targetURL string) {
	if flag.Lookup("test.v") != nil {
		return
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		switch runtime.GOOS {
		case "linux":
			if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
				log.Printf("[BROWSER] Headless/console environment detected (no DISPLAY or WAYLAND_DISPLAY); skipping browser launch. Connect at %s", targetURL)
				return
			}
			userProfileDir := filepath.Join(getAppConfigDir(), "webapp-profile")
			_ = os.MkdirAll(userProfileDir, 0755)

			desktopPath := ""
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				desktopPath = filepath.Join(home, ".local", "share", "applications", "directcrate.desktop")
			}
			desktopEnv := []string{
				"CHROME_DESKTOP=directcrate.desktop",
			}
			if desktopPath != "" {
				desktopEnv = append(desktopEnv, "BAMF_DESKTOP_FILE_HINT="+desktopPath)
			}

			// 1. Prefer native Chromium-based browser binaries in standalone app window mode
			nativeCandidates := []string{
				"google-chrome",
				"google-chrome-stable",
				"chromium",
				"chromium-browser",
				"brave-browser",
				"microsoft-edge",
				"vivaldi",
			}
			for _, bin := range nativeCandidates {
				if _, err := exec.LookPath(bin); err == nil {
					cmd := exec.Command(bin,
						"--app="+targetURL,
						"--class=DirectCrate",
						"--name=DirectCrate",
						"--user-data-dir="+userProfileDir,
					)
					cmd.Env = append(os.Environ(), desktopEnv...)
					if err := cmd.Start(); err == nil {
						return
					}
				}
			}

			// 2. Check Flatpak Chromium-based browser installations (common on Bazzite, SteamOS, Silverblue)
			if _, err := exec.LookPath("flatpak"); err == nil {
				flatpakCandidates := []string{
					"com.google.Chrome",
					"org.chromium.Chromium",
					"com.brave.Browser",
					"com.microsoft.Edge",
					"com.vivaldi.Vivaldi",
				}
				for _, appID := range flatpakCandidates {
					if exec.Command("flatpak", "info", appID).Run() == nil {
						// Omit --user-data-dir for Flatpak to avoid container sandbox path restriction errors
						flatpakArgs := []string{
							"run",
							"--env=CHROME_DESKTOP=directcrate.desktop",
						}
						if desktopPath != "" {
							flatpakArgs = append(flatpakArgs, "--env=BAMF_DESKTOP_FILE_HINT="+desktopPath)
						}
						flatpakArgs = append(flatpakArgs,
							appID,
							"--app="+targetURL,
							"--class=DirectCrate",
							"--name=DirectCrate",
						)
						cmd := exec.Command("flatpak", flatpakArgs...)
						cmd.Env = append(os.Environ(), desktopEnv...)
						if err := cmd.Start(); err == nil {
							return
						}
					}
				}
			}

			// 3. Fallback to default browser tab via xdg-open
			_ = exec.Command("xdg-open", targetURL).Start()

		case "windows":
			// Prefer standalone app window via Edge or Chrome
			var candidates []string
			progFiles := os.Getenv("ProgramFiles")
			progFilesX86 := os.Getenv("ProgramFiles(x86)")
			localAppData := os.Getenv("LOCALAPPDATA")

			knownPaths := []string{
				filepath.Join(progFilesX86, "Microsoft", "Edge", "Application", "msedge.exe"),
				filepath.Join(progFiles, "Microsoft", "Edge", "Application", "msedge.exe"),
				filepath.Join(localAppData, "Microsoft", "Edge", "Application", "msedge.exe"),
				filepath.Join(progFiles, "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(progFilesX86, "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(localAppData, "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(progFiles, "BraveSoftware", "Brave-Browser", "Application", "brave.exe"),
				filepath.Join(progFilesX86, "BraveSoftware", "Brave-Browser", "Application", "brave.exe"),
				filepath.Join(localAppData, "BraveSoftware", "Brave-Browser", "Application", "brave.exe"),
			}
			for _, p := range knownPaths {
				if p != "" {
					if _, err := os.Stat(p); err == nil {
						candidates = append(candidates, p)
					}
				}
			}
			candidates = append(candidates, "msedge", "chrome", "brave")

			userProfileDir := filepath.Join(getAppConfigDir(), "webapp-profile")
			_ = os.MkdirAll(userProfileDir, 0755)

			seen := make(map[string]bool)
			for _, bin := range candidates {
				if bin == "" || seen[bin] {
					continue
				}
				seen[bin] = true
				cmd := exec.Command(bin, "--app="+targetURL, "--user-data-dir="+userProfileDir)
				if err := cmd.Start(); err == nil {
					return
				}
			}
			// Fallback to default browser handler
			_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", targetURL).Start()

		case "darwin":
			_ = exec.Command("open", targetURL).Start()
		}
	}()
}

func handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	msgChan := make(chan string, 10)
	manager.clientsMu.Lock()
	manager.clients[msgChan] = true
	manager.hadUiClient = true
	if manager.idleTimer != nil {
		manager.idleTimer.Stop()
		manager.idleTimer = nil
		log.Printf("[LIFECYCLE] UI client connected; cancelled idle auto-exit timer")
	}
	manager.clientsMu.Unlock()

	defer func() {
		manager.clientsMu.Lock()
		delete(manager.clients, msgChan)
		shouldCheckExit := len(manager.clients) == 0 && manager.hadUiClient && runtime.GOOS != "android"
		manager.clientsMu.Unlock()
		close(msgChan)

		if shouldCheckExit {
			manager.checkAndScheduleIdleShutdown()
		}
	}()

	manager.mu.RLock()
	initPayload, _ := json.Marshal(manager.state)
	manager.mu.RUnlock()
	_, _ = fmt.Fprintf(w, "data: %s\n\n", initPayload)
	w.(http.Flusher).Flush()

	notify := r.Context().Done()
	for {
		select {
		case <-notify:
			return
		case msg := <-msgChan:
			_, _ = fmt.Fprintf(w, "data: %s\n\n", msg)
			w.(http.Flusher).Flush()
		}
	}
}

func handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	manager.cancelIdleTimer()

	manager.mu.RLock()
	st := manager.state.Status
	isRunning := st == "running" || st == "needs_password" || st == "needs_url" || st == "needs_disk"
	manager.mu.RUnlock()

	if isRunning {
		http.Error(w, "Extraction already running", http.StatusConflict)
		return
	}

	var opts TaskOptions
	if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if strings.TrimSpace(opts.URL) == "" {
		if nextTask, ok := manager.PopNextQueue(); ok {
			opts = nextTask.Options
		} else {
			http.Error(w, "URL is required", http.StatusBadRequest)
			return
		}
	}

	if opts.Dest == "" {
		opts.Dest = getDefaultDownloadsDir()
	}

	go executeTask(opts)
	w.WriteHeader(http.StatusOK)
}

func handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	manager.InstantAbort()
	w.WriteHeader(http.StatusOK)
}

func handleFocus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	targetURL := fmt.Sprintf("http://localhost:%d", activeServerPort)
	if activeServerPort == 0 {
		targetURL = "http://localhost:9090"
	}
	log.Printf("[LIFECYCLE] Received focus request; refocusing standalone app window: %s", targetURL)
	go launchBrowser(targetURL)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok","message":"focus triggered"}`))
}

// probeDirectCrateInstance checks if a DirectCrate server is already actively listening
// on the given target URL (e.g. "http://127.0.0.1:9090"). It sends a GET request to /api/status
// with a fast 300ms timeout and validates that the returned JSON contains a valid status string.
func probeDirectCrateInstance(targetBaseURL string) bool {
	client := &http.Client{
		Timeout: 300 * time.Millisecond,
	}
	resp, err := client.Get(targetBaseURL + "/api/status")
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false
	}

	var st struct {
		App    string `json:"app"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return false
	}
	return st.App == "DirectCrate" && st.Status != ""
}

// focusExistingInstance instructs the already-running DirectCrate instance to re-open or focus
// its standalone app window via POST /api/focus.
func focusExistingInstance(targetBaseURL string) {
	client := &http.Client{
		Timeout: 1 * time.Second,
	}
	resp, err := client.Post(targetBaseURL+"/api/focus", "application/json", strings.NewReader("{}"))
	if err == nil {
		_ = resp.Body.Close()
	}
}

var (
	globalServer     *http.Server
	shutdownSignalCh = make(chan struct{}, 1)
)

func triggerGracefulExit() {
	select {
	case shutdownSignalCh <- struct{}{}:
	default:
	}
	if manager != nil {
		manager.InstantAbort()
	}
	if globalServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = globalServer.Shutdown(ctx)
	}
}

func setupRouter() http.Handler {
	mux := http.NewServeMux()
	wrap := func(name string, h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			defer recoverAndLogPanic("http:" + name)
			h(w, r)
		}
	}

	mux.HandleFunc("/", wrap("index", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(htmlIndex)
	}))

	mux.HandleFunc("/favicon.ico", wrap("favicon", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(faviconBytes)
	}))

	mux.HandleFunc("/icon.png", wrap("icon", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(iconPngBytes)
	}))

	mux.HandleFunc("/api/status", wrap("status", handleStatus))
	mux.HandleFunc("/api/thermals", wrap("thermals", handleThermals))
	mux.HandleFunc("/api/focus", wrap("focus", handleFocus))
	mux.HandleFunc("/api/check-folder", wrap("check-folder", handleCheckFolder))
	mux.HandleFunc("/api/defaults", wrap("defaults", handleDefaults))
	mux.HandleFunc("/api/settings", wrap("settings", handleSettings))
	mux.HandleFunc("/api/inspect", wrap("inspect", handleInspect))
	mux.HandleFunc("/api/unlock", wrap("unlock", handleUnlock))
	mux.HandleFunc("/api/resume-url", wrap("resume-url", handleResumeURL))
	mux.HandleFunc("/api/speed-limit", wrap("speed-limit", handleSpeedLimit))
	mux.HandleFunc("/api/queue", wrap("queue", handleQueue))
	mux.HandleFunc("/api/queue/", wrap("queue-item", handleQueueItem))
	mux.HandleFunc("/api/browse", wrap("browse", handleBrowse))
	mux.HandleFunc("/api/open-folder", wrap("open-folder", handleOpenFolder))
	mux.HandleFunc("/api/add-steam", wrap("add-steam", handleAddToSteam))
	mux.HandleFunc("/api/history", wrap("history", handleHistory))
	mux.HandleFunc("/api/logs", wrap("logs", handleLogs))
	mux.HandleFunc("/api/check-update", wrap("check-update", handleCheckUpdate))

	mux.HandleFunc("/events", wrap("events", handleEvents))
	mux.HandleFunc("/start", wrap("start", handleStart))
	mux.HandleFunc("/cancel", wrap("cancel", handleCancel))

	return mux
}

func main() {
	portFlag := flag.String("port", ":9090", "HTTP server listen address or port (e.g. :9090)")
	versionFlag := flag.Bool("version", false, "Print DirectCrate version and exit")
	noOpenFlag := flag.Bool("no-open", false, "Disable automatic browser launch on startup")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("DirectCrate %s\n", appVersion)
		return
	}

	host, startPort, err := parseListenAddr(*portFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid -port value %q: %v\n", *portFlag, err)
		log.Printf("[SYSTEM] Invalid -port value %q: %v", *portFlag, err)
		os.Exit(1)
	}

	// Single-instance enforcement on Desktop (Linux / Windows):
	// Probe the configured port before binding or fallback. If another DirectCrate process
	// is already running, instruct it to focus its window and exit immediately without writing
	// duplicate startup logs or spawning duplicate background processes.
	if runtime.GOOS != "android" && startPort > 0 {
		probeTarget := fmt.Sprintf("http://127.0.0.1:%d", startPort)
		if probeDirectCrateInstance(probeTarget) {
			fmt.Printf("DirectCrate is already running on port %d; focusing existing instance.\n", startPort)
			if !*noOpenFlag {
				focusExistingInstance(probeTarget)
			}
			os.Exit(0)
		}
	}

	initLogging()
	defer recoverAndLogPanic("main")
	log.Printf("[SYSTEM] DirectCrate %s starting on %s/%s", appVersion, runtime.GOOS, runtime.GOARCH)

	currentSettings = loadSettings()
	if currentSettings.SpeedLimitMBs > 0 {
		globalSpeedLimitBytesPerSec.Store(int64(currentSettings.SpeedLimitMBs) * 1024 * 1024)
		log.Printf("[SYSTEM] Loaded saved speed limit: %d MB/s", currentSettings.SpeedLimitMBs)
	}

	handler := setupRouter()

	ln, activePort, err := listenWithFallback(host, startPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start server: %v\n", err)
		log.Printf("[SYSTEM] Failed to start server: %v", err)
		os.Exit(1)
	}
	activeServerPort = activePort

	fmt.Println("DirectCrate — Direct-to-disk remote archive streamer")
	if startPort != 0 && activePort != startPort {
		msg := fmt.Sprintf("Port :%d is already in use; bound to fallback port :%d", startPort, activePort)
		fmt.Println(msg)
		log.Printf("[SYSTEM] %s", msg)
	}
	displayURL := formatDisplayURL(host, activePort)
	fmt.Printf("Listening on %s\n", displayURL)
	lanURLs := getLANURLs(activePort)
	for _, lanURL := range lanURLs {
		fmt.Printf("  Network: %s\n", lanURL)
	}
	log.Printf("[SYSTEM] HTTP server active on %s (log: %s)", displayURL, getLogFilePath())

	startTelemetrySampler()

	isSSH := os.Getenv("SSH_CLIENT") != "" || os.Getenv("SSH_TTY") != "" || os.Getenv("SSH_CONNECTION") != ""
	if !*noOpenFlag && runtime.GOOS != "android" && !isSSH {
		launchBrowser(displayURL)
	}

	if runtime.GOOS == "linux" {
		ensureLinuxDesktopIntegration()
		startLinuxTaskbarProgressMonitor()
	}
	if runtime.GOOS == "windows" {
		go ensureWindowsStartMenuShortcut()
		startWindowsTaskbarProgressMonitor()
	}

	globalServer = &http.Server{
		Handler: handler,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-sigCh:
			log.Printf("[SYSTEM] Received signal %v; initiating graceful shutdown", sig)
			triggerGracefulExit()
		case <-shutdownSignalCh:
		}
	}()

	if err := globalServer.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		log.Printf("[SYSTEM] Server terminated: %v", err)
		os.Exit(1)
	}
	log.Printf("[SYSTEM] Server exited cleanly")
	os.Exit(0)
}
