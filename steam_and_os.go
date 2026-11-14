package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

func detectPrimaryExecutables(dir string) []string {
	ignoreKeywords := []string{
		"unins", "redist", "vcredist", "dxsetup", "directx", "crash",
		"unitycrash", "ue4prereq", "ueprereq", "dotnet", "physx", "launcher_helper",
	}

	type exeCandidate struct {
		path  string
		size  int64
		depth int
	}

	var candidates []exeCandidate
	baseClean := filepath.Clean(dir)

	_ = filepath.WalkDir(baseClean, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		lowerName := strings.ToLower(d.Name())
		if !strings.HasSuffix(lowerName, ".exe") {
			return nil
		}
		for _, kw := range ignoreKeywords {
			if strings.Contains(lowerName, kw) {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(baseClean, path)
		depth := len(strings.Split(rel, string(filepath.Separator)))
		candidates = append(candidates, exeCandidate{
			path:  path,
			size:  info.Size(),
			depth: depth,
		})
		return nil
	})

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].depth != candidates[j].depth && (candidates[i].depth <= 2 || candidates[j].depth <= 2) {
			return candidates[i].depth < candidates[j].depth
		}
		return candidates[i].size > candidates[j].size
	})

	var results []string
	for _, c := range candidates {
		results = append(results, c.path)
	}
	return results
}

func computeSteamAppID(exePath, appName string) uint32 {
	key := exePath + appName
	crc := crc32.ChecksumIEEE([]byte(key))
	return crc | 0x80000000
}

// cleanGameSearchQuery normalizes folder/executable names for accurate Steam Store cover art lookup
func cleanGameSearchQuery(raw string) string {
	clean := strings.ReplaceAll(raw, "_", " ")
	clean = strings.ReplaceAll(clean, "-", " ")
	clean = strings.ReplaceAll(clean, ".", " ")
	re := regexp.MustCompile(`(?i)\b(v\d+[\d.]*|build\s*\d+|repack|fitgirl|dodi|elamigos|codex|rune|tenoke|goldberg|gog|deluxe|edition|goty|ultimate|complete|win64|x64)\b`)
	clean = re.ReplaceAllString(clean, " ")
	clean = strings.Join(strings.Fields(clean), " ")
	return strings.TrimSpace(clean)
}

// fetchSteamCoverArt queries the public Steam Store search API and saves grid poster, hero banner, and logo
func fetchSteamCoverArt(gridDir string, shortcutAppID uint32, rawTitle string) bool {
	query := cleanGameSearchQuery(rawTitle)
	if query == "" {
		return false
	}

	client := createOptimizedHTTPClient(12 * time.Second)
	searchURL := "https://store.steampowered.com/api/storesearch/?term=" + url.QueryEscape(query) + "&l=english&cc=US"

	resp, err := client.Get(searchURL)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	var payload struct {
		Items []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil || len(payload.Items) == 0 {
		return false
	}

	steamStoreID := payload.Items[0].ID
	_ = os.MkdirAll(gridDir, 0755)

	assets := []struct {
		remoteURL string
		localPath string
	}{
		{
			remoteURL: fmt.Sprintf("https://cdn.cloudflare.steamstatic.com/steam/apps/%d/library_600x900_2x.jpg", steamStoreID),
			localPath: filepath.Join(gridDir, fmt.Sprintf("%dp.jpg", shortcutAppID)),
		},
		{
			remoteURL: fmt.Sprintf("https://cdn.cloudflare.steamstatic.com/steam/apps/%d/library_hero.jpg", steamStoreID),
			localPath: filepath.Join(gridDir, fmt.Sprintf("%d_hero.jpg", shortcutAppID)),
		},
		{
			remoteURL: fmt.Sprintf("https://cdn.cloudflare.steamstatic.com/steam/apps/%d/logo.png", steamStoreID),
			localPath: filepath.Join(gridDir, fmt.Sprintf("%d_logo.png", shortcutAppID)),
		},
	}

	downloadedAny := false
	for _, a := range assets {
		r, err := client.Get(a.remoteURL)
		if err != nil {
			continue
		}
		if r.StatusCode == http.StatusOK {
			data, err := io.ReadAll(r.Body)
			if err == nil && len(data) > 1024 {
				if os.WriteFile(a.localPath, data, 0644) == nil {
					downloadedAny = true
				}
			}
		}
		_ = r.Body.Close()
	}

	return downloadedAny
}

func addNonSteamGameToSteam(exePath, appName string) (string, error) {
	if exePath == "" {
		return "", fmt.Errorf("no executable path provided")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	var steamBaseDirs []string
	if runtime.GOOS == "windows" {
		if pf86 := os.Getenv("ProgramFiles(x86)"); pf86 != "" {
			steamBaseDirs = append(steamBaseDirs, filepath.Join(pf86, "Steam", "userdata"))
		}
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			steamBaseDirs = append(steamBaseDirs, filepath.Join(pf, "Steam", "userdata"))
		}
		steamBaseDirs = append(steamBaseDirs,
			`C:\Program Files (x86)\Steam\userdata`,
			`C:\Program Files\Steam\userdata`,
		)
	} else {
		steamBaseDirs = append(steamBaseDirs,
			filepath.Join(home, ".local/share/Steam/userdata"),
			filepath.Join(home, ".steam/steam/userdata"),
			filepath.Join(home, ".steam/root/userdata"),
			filepath.Join(home, ".var/app/com.valvesoftware.Steam/data/Steam/userdata"),
			filepath.Join(home, ".var/app/com.valvesoftware.Steam/.local/share/Steam/userdata"),
		)
	}

	var targetVDF string
	var configDir string

	for _, base := range steamBaseDirs {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() && e.Name() != "0" && e.Name() != "ac" {
				candConfig := filepath.Join(base, e.Name(), "config")
				if _, err := os.Stat(candConfig); err == nil {
					configDir = candConfig
					targetVDF = filepath.Join(configDir, "shortcuts.vdf")
					break
				}
			}
		}
		if targetVDF != "" {
			break
		}
	}

	if targetVDF == "" {
		return "", fmt.Errorf("Steam userdata directory not found")
	}

	var existingData []byte
	if data, err := os.ReadFile(targetVDF); err == nil && len(data) > 2 {
		existingData = data
		_ = os.WriteFile(targetVDF+".bak", data, 0644)
	} else {
		existingData = append([]byte{0x00}, []byte("shortcuts")...)
		existingData = append(existingData, 0x00, 0x08, 0x08)
	}

	entryIndex := 0
	for i := 0; i < len(existingData)-10; i++ {
		if existingData[i] == 0x00 && existingData[i+1] >= '0' && existingData[i+1] <= '9' {
			entryIndex++
		}
	}

	appID := computeSteamAppID(exePath, appName)
	startDir := filepath.Dir(exePath)

	var entry bytes.Buffer
	writeStr := func(key, val string) {
		entry.WriteByte(0x01)
		entry.WriteString(key)
		entry.WriteByte(0x00)
		entry.WriteString(val)
		entry.WriteByte(0x00)
	}
	writeInt := func(key string, val uint32) {
		entry.WriteByte(0x02)
		entry.WriteString(key)
		entry.WriteByte(0x00)
		_ = binary.Write(&entry, binary.LittleEndian, val)
	}

	entry.WriteByte(0x00)
	entry.WriteString(fmt.Sprintf("%d", entryIndex))
	entry.WriteByte(0x00)

	writeInt("appid", appID)
	writeStr("AppName", appName)
	writeStr("Exe", fmt.Sprintf("\"%s\"", exePath))
	writeStr("StartDir", fmt.Sprintf("\"%s\"", startDir))
	writeStr("icon", "")
	writeStr("ShortcutPath", "")
	writeStr("LaunchOptions", "")
	writeInt("IsHidden", 0)
	writeInt("AllowDesktopConfig", 1)
	writeInt("AllowOverlay", 1)
	writeInt("OpenVR", 0)
	writeInt("Devkit", 0)
	writeStr("DevkitGameID", "")
	writeInt("DevkitOverrideAppID", 0)
	writeInt("LastPlayTime", 0)
	writeStr("FlatpakAppID", "")

	entry.WriteByte(0x00)
	entry.WriteString("tags")
	entry.WriteByte(0x00)
	entry.WriteByte(0x08)
	entry.WriteByte(0x08)

	if len(existingData) >= 2 && existingData[len(existingData)-1] == 0x08 && existingData[len(existingData)-2] == 0x08 {
		var updated bytes.Buffer
		updated.Write(existingData[:len(existingData)-2])
		updated.Write(entry.Bytes())
		updated.Write([]byte{0x08, 0x08})
		if err := os.WriteFile(targetVDF, updated.Bytes(), 0644); err != nil {
			return "", fmt.Errorf("failed writing shortcuts.vdf: %w", err)
		}
	} else {
		return "", fmt.Errorf("unrecognized shortcuts.vdf binary structure")
	}

	gridDir := filepath.Join(configDir, "grid")
	artFetched := fetchSteamCoverArt(gridDir, appID, appName)
	if !artFetched {
		exeBase := strings.TrimSuffix(filepath.Base(exePath), filepath.Ext(exePath))
		artFetched = fetchSteamCoverArt(gridDir, appID, exeBase)
	}

	artNote := "No matching Steam cover artwork found."
	if artFetched {
		artNote = "Official Steam poster, hero banner, and logo downloaded."
	}

	return fmt.Sprintf("Added '%s' to Steam (AppID: %d). %s Restart Steam or switch modes to see it in Library.", appName, appID, artNote), nil
}

func sendDesktopNotification(title, message, urgency string) {
	if !loadSettings().DesktopNotify {
		return
	}
	go func() {
		if runtime.GOOS == "linux" {
			_ = exec.Command("notify-send", "-a", "DirectCrate", "-u", urgency, title, message).Run()
		} else if runtime.GOOS == "windows" {
			psCmd := fmt.Sprintf(`[reflection.assembly]::loadwithpartialname('System.Windows.Forms'); [reflection.assembly]::loadwithpartialname('System.Drawing'); $n = new-object system.windows.forms.notifyicon; $n.icon = [system.drawing.systemicons]::information; $n.visible = $true; $n.showballoontip(5000, '%s', '%s', [system.windows.forms.tooltipicon]::info)`, strings.ReplaceAll(title, "'", "''"), strings.ReplaceAll(message, "'", "''"))
			_ = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", psCmd).Start()
		}
	}()
}

func playCompletionSound() {
	go func() {
		if runtime.GOOS == "windows" {
			_ = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", "[System.Media.SystemSounds]::Asterisk.Play()").Start()
			return
		}
		if runtime.GOOS != "linux" {
			return
		}
		soundCandidates := []string{
			"/usr/share/sounds/freedesktop/stereo/complete.oga",
			"/usr/share/sounds/oxygen/stereo/dialog-information.ogg",
			"/usr/share/sounds/freedesktop/stereo/bell.oga",
		}

		var targetSound string
		for _, s := range soundCandidates {
			if _, err := os.Stat(s); err == nil {
				targetSound = s
				break
			}
		}

		if targetSound == "" {
			return
		}

		if _, err := exec.LookPath("pw-play"); err == nil {
			_ = exec.Command("pw-play", targetSound).Run()
		} else if _, err := exec.LookPath("paplay"); err == nil {
			_ = exec.Command("paplay", targetSound).Run()
		}
	}()
}

// triggerPowerAction executes the post-completion sleep or shutdown sequence after queue drain.
func triggerPowerAction(action string) {
	switch action {
	case "sleep":
		go func() {
			sendDesktopNotification("DirectCrate Overnight Mode", "Extraction finished. Device will sleep in 15 seconds...", "normal")
			time.Sleep(15 * time.Second)
			if runtime.GOOS == "linux" {
				_ = exec.Command("systemctl", "suspend").Run()
			} else if runtime.GOOS == "windows" {
				_ = exec.Command("rundll32.exe", "powrprof.dll,SetSuspendState", "0,1,0").Run()
			}
		}()
	case "shutdown":
		go func() {
			sendDesktopNotification("DirectCrate Overnight Mode", "Extraction finished. Device will shut down in 15 seconds...", "normal")
			if runtime.GOOS == "linux" {
				time.Sleep(15 * time.Second)
				_ = exec.Command("systemctl", "poweroff").Run()
			} else if runtime.GOOS == "windows" {
				_ = exec.Command("shutdown", "/s", "/t", "15").Run()
			}
		}()
	}
}

func startIdleInhibitor(ctx context.Context) {
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("systemd-inhibit"); err == nil {
			cmd := exec.CommandContext(ctx, "systemd-inhibit", "--what=idle:sleep", "--who=DirectCrate", "--why=Streaming archive extraction in progress", "sleep", "infinity")
			_ = cmd.Start()
		}
	} else if runtime.GOOS == "windows" {
		startWindowsIdleInhibitor(ctx)
	}
}

func getLocalIPv4s() []string {
	var ips []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				ips = append(ips, ip4.String())
			}
		}
	}
	return ips
}

func getLANURLs(port int) []string {
	if port <= 0 {
		return nil
	}
	ips := getLocalIPv4s()
	urls := make([]string, 0, len(ips))
	for _, ip := range ips {
		urls = append(urls, fmt.Sprintf("http://%s", net.JoinHostPort(ip, strconv.Itoa(port))))
	}
	return urls
}


func getDefaultDownloadsDir() string {
	customConfigDirMu.RLock()
	if customDefaultDest != "" {
		dest := customDefaultDest
		customConfigDirMu.RUnlock()
		return dest
	}
	customConfigDirMu.RUnlock()

	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, "Downloads")
}

// updateLinuxTaskbarProgress emits Unity/KDE LauncherEntry progress via gdbus
func updateLinuxTaskbarProgress(progress float64, visible bool) {
	if runtime.GOOS != "linux" {
		return
	}
	if _, err := exec.LookPath("gdbus"); err != nil {
		return
	}
	if progress < 0.0 {
		progress = 0.0
	} else if progress > 1.0 {
		progress = 1.0
	}
	visStr := "false"
	if visible {
		visStr = "true"
	}
	payload := fmt.Sprintf("{'progress': <%.4f>, 'progress-visible': <%s>}", progress, visStr)
	cmd := exec.Command("gdbus", "emit",
		"--session",
		"--object-path", "/com/canonical/unity/launcherentry/1",
		"--signal", "com.canonical.Unity.LauncherEntry.Update",
		"application://directcrate.desktop",
		payload,
	)
	_ = cmd.Run()
}

// startLinuxTaskbarProgressMonitor begins a 1-second ticker polling active extraction progress
// and updating the taskbar progress bar via D-Bus on supported Linux desktop environments.
func startLinuxTaskbarProgressMonitor() {
	if runtime.GOOS != "linux" {
		return
	}
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		wasActive := false
		for range ticker.C {
			sum := getActiveProgressSummary()
			if sum.Active && sum.LiveProgressNotifications {
				wasActive = true
				updateLinuxTaskbarProgress(sum.Percent/100.0, true)
			} else if wasActive {
				wasActive = false
				updateLinuxTaskbarProgress(0.0, false)
			}
		}
	}()
}

const directcrateBrandSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 108 108" fill="none">
<rect width="108" height="108" rx="24" fill="#09090B"/>
<path d="M54 28 L76 40.5 L76 65.5 L54 78 L32 65.5 L32 40.5 Z" stroke="#FAFAFA" stroke-width="4.5" stroke-linecap="round" stroke-linejoin="round"/>
<path d="M32 40.5 L54 53 L76 40.5 M54 53 L54 78" stroke="#FAFAFA" stroke-width="4.5" stroke-linecap="round" stroke-linejoin="round"/>
<path d="M54 35 L54 45 M49 41 L54 46 L59 41" stroke="#10B981" stroke-width="4.2" stroke-linecap="round" stroke-linejoin="round"/>
</svg>
`

// ensureLinuxDesktopIntegration registers directcrate.svg and directcrate.desktop in the user's
// ~/.local/share directory and triggers silent desktop database and icon cache updates.
func ensureLinuxDesktopIntegration() {
	if runtime.GOOS != "linux" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return
	}
	exePath, err := os.Executable()
	if err != nil {
		return
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil && resolved != "" {
		exePath = resolved
	}

	// 1. Write or update scalable icon
	iconDir := filepath.Join(home, ".local", "share", "icons", "hicolor", "scalable", "apps")
	_ = os.MkdirAll(iconDir, 0755)
	iconPath := filepath.Join(iconDir, "directcrate.svg")
	existingIcon, _ := os.ReadFile(iconPath)
	if string(existingIcon) != directcrateBrandSVG {
		_ = os.WriteFile(iconPath, []byte(directcrateBrandSVG), 0644)
	}

	// 2. Write or update .desktop file
	appsDir := filepath.Join(home, ".local", "share", "applications")
	_ = os.MkdirAll(appsDir, 0755)
	desktopPath := filepath.Join(appsDir, "directcrate.desktop")

	execCmd := exePath
	if strings.Contains(execCmd, " ") && !strings.HasPrefix(execCmd, "\"") {
		execCmd = fmt.Sprintf("\"%s\"", execCmd)
	}

	desktopContent := fmt.Sprintf(`[Desktop Entry]
Name=DirectCrate
Comment=Direct-to-disk remote archive streamer
Exec=%s
Icon=directcrate
Terminal=false
Type=Application
Categories=Utility;Archiving;Network;
StartupWMClass=DirectCrate
Keywords=directcrate;streamer;archive;unpacker;download;
`, execCmd)

	existingDesktop, _ := os.ReadFile(desktopPath)
	if string(existingDesktop) != desktopContent {
		_ = os.WriteFile(desktopPath, []byte(desktopContent), 0644)
	}

	// 3. Silently update desktop database and icon cache in the background
	go func() {
		if p, err := exec.LookPath("update-desktop-database"); err == nil {
			_ = exec.Command(p, appsDir).Run()
		}
		if p, err := exec.LookPath("gtk-update-icon-cache"); err == nil {
			hicolorDir := filepath.Join(home, ".local", "share", "icons", "hicolor")
			_ = exec.Command(p, "-f", "-t", hicolorDir).Run()
		}
	}()
}

// buildPropertyStoreDataBlock constructs an MS-SHLLINK PropertyStoreDataBlock
// containing PKEY_AppUserModel_ID (fmtid {9F4C2855-9F79-4BDE-9E46-37E7497502C8}, pid 5).
func buildPropertyStoreDataBlock(appID string) []byte {
	runes := []rune(appID)
	valBytes := make([]byte, (len(runes)+1)*2)
	for i, r := range runes {
		binary.LittleEndian.PutUint16(valBytes[i*2:], uint16(r))
	}

	var valData bytes.Buffer
	_ = binary.Write(&valData, binary.LittleEndian, uint16(0x001F))       // VT_LPWSTR
	_ = binary.Write(&valData, binary.LittleEndian, uint16(0))            // padding
	_ = binary.Write(&valData, binary.LittleEndian, uint32(len(runes)+1)) // char count including null terminator
	valData.Write(valBytes)

	var propRec bytes.Buffer
	recSize := uint32(4 + 4 + 1 + valData.Len())
	_ = binary.Write(&propRec, binary.LittleEndian, recSize)
	_ = binary.Write(&propRec, binary.LittleEndian, uint32(5)) // PID 5 for AppUserModelID
	propRec.WriteByte(0)                                       // reserved
	propRec.Write(valData.Bytes())

	fmtID := []byte{0x55, 0x28, 0x4C, 0x9F, 0x79, 0x9F, 0xDE, 0x4B, 0x9E, 0x46, 0x37, 0xE7, 0x49, 0x75, 0x02, 0xC8}
	var storage bytes.Buffer
	storage.WriteString("1SPS")
	storage.Write(fmtID)
	storage.Write(propRec.Bytes())
	_ = binary.Write(&storage, binary.LittleEndian, uint32(0)) // storage terminal

	var fullStorage bytes.Buffer
	_ = binary.Write(&fullStorage, binary.LittleEndian, uint32(4+storage.Len()))
	fullStorage.Write(storage.Bytes())

	var block bytes.Buffer
	blockSize := uint32(8 + fullStorage.Len())
	_ = binary.Write(&block, binary.LittleEndian, blockSize)
	_ = binary.Write(&block, binary.LittleEndian, uint32(0xA0000009)) // PropertyStoreDataBlock signature
	block.Write(fullStorage.Bytes())

	return block.Bytes()
}

// injectAppUserModelIDToLnk appends an AppUserModelID PropertyStoreDataBlock to a Windows .lnk file
// if not already present, ensuring Windows Search and taskbar pinning recognize DirectCrate.App.
func injectAppUserModelIDToLnk(lnkPath string, appID string) {
	data, err := os.ReadFile(lnkPath)
	if err != nil || len(data) < 4 {
		return
	}
	// Check UTF-16LE representation
	utf16AppID := []byte("D\x00i\x00r\x00e\x00c\x00t\x00C\x00r\x00a\x00t\x00e\x00.\x00A\x00p\x00p\x00")
	if bytes.Contains(data, utf16AppID) {
		return
	}
	block := buildPropertyStoreDataBlock(appID)

	// Strip terminal block if present at the end
	if len(data) >= 4 && binary.LittleEndian.Uint32(data[len(data)-4:]) == 0 {
		data = data[:len(data)-4]
	}
	var out bytes.Buffer
	out.Write(data)
	out.Write(block)
	_ = binary.Write(&out, binary.LittleEndian, uint32(0)) // new terminal block

	_ = os.WriteFile(lnkPath, out.Bytes(), 0644)
}

// ensureWindowsStartMenuShortcut creates or updates DirectCrate.lnk in the user's Start Menu
// pointing to the resolved executable path with AppUserModelID "DirectCrate.App".
func ensureWindowsStartMenuShortcut() {
	if runtime.GOOS != "windows" {
		return
	}
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return
	}
	exePath, err := os.Executable()
	if err != nil {
		return
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil && resolved != "" {
		exePath = resolved
	}
	exeDir := filepath.Dir(exePath)

	programsDir := filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs")
	_ = os.MkdirAll(programsDir, 0755)
	lnkPath := filepath.Join(programsDir, "DirectCrate.lnk")

	psScript := fmt.Sprintf(`$wsh = New-Object -ComObject WScript.Shell; $s = $wsh.CreateShortcut('%s'); $s.TargetPath = '%s'; $s.WorkingDirectory = '%s'; $s.IconLocation = '%s,0'; $s.Description = 'DirectCrate - Direct-to-disk remote archive streamer'; $s.Save()`,
		strings.ReplaceAll(lnkPath, "'", "''"),
		strings.ReplaceAll(exePath, "'", "''"),
		strings.ReplaceAll(exeDir, "'", "''"),
		strings.ReplaceAll(exePath, "'", "''"),
	)

	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", psScript)
	if err := cmd.Run(); err != nil {
		return
	}

	injectAppUserModelIDToLnk(lnkPath, "DirectCrate.App")
}


