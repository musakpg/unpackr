# DirectCrate

DirectCrate is a high-performance, single-binary remote archive streamer written in Go with an embedded web UI. It downloads and decompresses archives **directly to disk over HTTP/HTTPS** without ever writing an intermediate `.rar`, `.zip`, `.7z`, or `.tar` archive file to local storage.

Engineered specifically for storage-constrained devices such as handheld gaming PCs (Steam Deck, ROG Ally, Lenovo Legion Go running SteamOS or Bazzite), Linux & Windows desktops, and Android 15/16 mobile devices.

---

## Table of Contents

- [Quick Start (Pre-Built Releases)](#quick-start-pre-built-releases)
- [Core Value: Zero-Temp-Disk Architecture](#core-value-zero-temp-disk-architecture)
- [Beginner & Everyday User Guide (Plain Language FAQ)](#beginner--everyday-user-guide-plain-language-faq)
- [Format Compatibility Matrix](#format-compatibility-matrix)
- [Device Resource Footprint & Hardware Transparency (Network, RAM, CPU, Storage)](#device-resource-footprint--hardware-transparency-network-ram-cpu-storage)
- [Complete Local Storage Transparency](#complete-local-storage-transparency)
- [Lifecycle Safeguards & Fault Tolerance Architecture](#lifecycle-safeguards--fault-tolerance-architecture)
- [Linux (Desktop, SteamOS & Bazzite) Behavior & OS Hooks](#linux-desktop-steamos--bazzite-behavior--os-hooks)
- [Windows Behavior & OS Hooks](#windows-behavior--os-hooks)
- [Android 15/16 Architecture & Native Integration](#android-1516-architecture--native-integration)
- [Process Lifecycle & Single-Instance Architecture](#process-lifecycle--single-instance-architecture)
- [Cross-Platform Web UI & Custom Theme-Matched Controls](#cross-platform-web-ui--custom-theme-matched-controls)
- [Keyboard Shortcuts & UI Controls](#keyboard-shortcuts--ui-controls)
- [Diagnostics & Crash Logging](#diagnostics--crash-logging)
- [Installation, Developer Guide & CLI](#installation-developer-guide--cli)
- [License](#license)

---

## Quick Start (Pre-Built Releases)

Download pre-compiled standalone release binaries and Android packages directly from the official [GitHub Releases](https://github.com/musakpg/directcrate/releases):

| Platform | Recommended Download | Quick Start Instructions |
| :--- | :--- | :--- |
| **Linux / Steam Deck / Bazzite** | `directcrate-linux-amd64`<br>`directcrate-linux-arm64` | `chmod +x directcrate-linux-amd64 && ./directcrate-linux-amd64`<br>Automatically launches in a borderless app window and registers desktop/taskbar icons. |
| **Windows 10 / 11** | `directcrate-windows-amd64.exe` | Double-click to launch. Official PE icon, Start Menu shortcut, and taskbar progress work out of the box. |
| **Android 11–16** | `DirectCrate.apk`<br>`directcrate-android-arm64` (CLI) | Install `DirectCrate.apk`. Includes live foreground service progress notifications, thermal telemetry, and clipboard bridge. (Or run CLI in Termux). |

---

## Core Value: Zero-Temp-Disk Architecture

Traditional download utilities require **2x the storage capacity** of the final uncompressed content: 1x to store the downloaded archive (`.zip` / `.rar`), and 1x to unpack the contents. On a 64 GB or 256 GB drive, unpacking a 50 GB game or dataset fails even when plenty of space exists for the final unpacked game itself.

DirectCrate solves this by decompressing byte streams in memory on the fly:

```text
┌────────────────────────────────────────────────────────────────────────┐
│                        Remote HTTP(S) Server                           │
└───────────────────────────────────┬────────────────────────────────────┘
                                    │
                                    ├──► ParallelRangeStream (2 MB chunks across 1–8 workers)
                                    ├──► ResilientSingleStream (Automatic HTTP Range resume)
                                    └──► MultiPartStreamReader (Sequential part1 → part2 → ... → partN)
                                    │
                                    ▼
┌────────────────────────────────────────────────────────────────────────┐
│                        Streaming Decompressor                          │
│        (RAR5 / ZIP / 7Z / TAR.GZ / TAR.ZST via io.CopyBuffer)          │
└───────────────────────────────────┬────────────────────────────────────┘
                                    │
                                    ▼
┌────────────────────────────────────────────────────────────────────────┐
│                    Configurable sync.Pool I/O Buffer                   │
│                       (1 MB, 2 MB, 4 MB, 8 MB, 16 MB)                  │
└───────────────────────────────────┬────────────────────────────────────┘
                                    │
                                    ▼
┌────────────────────────────────────────────────────────────────────────┐
│                   Target Destination Storage on Disk                   │
│        (Direct extracted game, application, or dataset files)          │
└────────────────────────────────────────────────────────────────────────┘
```

### Key Capabilities

- **Parallel HTTP Range Prefetching**: Splits downloads into `2 MB` segments across 1–8 concurrent workers when remote servers support `Accept-Ranges: bytes`.
- **Automatic Range Auto-Reconnect**: Resumes dropped TCP sockets transparently with exponential backoff (up to 10 retries) via `Range: bytes=<offset>-`.
- **403/410 Expiring CDN URL Hot-Swap**: If a temporary CDN download ticket expires mid-stream with HTTP `403 Forbidden` or `410 Gone`, DirectCrate pauses without discarding extracted files, allowing you to paste a refreshed direct link to resume from the exact byte offset.
- **Selective File Inspection & Extraction**: For ZIP and 7Z archives, clicking **Inspect** queries the remote Central Directory or 7Z metadata header via Range requests without downloading archive payloads. You can inspect the complete file hierarchy and uncheck unwanted files (such as bonus soundtracks, making-of videos, or redundant language packs) so DirectCrate downloads only the selected ranges.
- **Dynamic Bandwidth Limiter**: Throttle transfer speeds mid-stream (`Unlimited`, `5`, `10`, `20`, `35`, or `50` MB/s) using an atomic token-bucket limiter without interrupting the extraction.
- **Sequential Batch Queue**: Add multiple archive URLs to the queue; DirectCrate processes each archive in FIFO sequence and executes post-completion power actions only after the entire queue drains.
- **Existing Folder Conflict Policies**: Choose how DirectCrate handles existing folders:
  - **Prompt Every Time**: Ask before extracting if a destination folder already contains files.
  - **Merge with Existing**: Overwrite identical files while keeping non-conflicting files.
  - **Clean & Overwrite**: Wipe the existing directory and extract cleanly.
  - **Auto-Rename**: Append an incremental suffix (`_2`, `_3`) to the subfolder.
- **Cross-Filesystem Path Sanitizer & Traversal Defense**: All archive entry paths are strictly validated. Any entries containing relative path traversal components (`../`) are immediately rejected. Illegal filesystem characters (`:`, `*`, `?`, `"`, `<`, `>`, `|`) and trailing dots/spaces are stripped or normalized. Windows reserved device names (`CON`, `PRN`, `AUX`, `NUL`, `COM1`–`COM9`, `LPT1`–`LPT9`, case-insensitive, matching full segment or stem before the first dot, e.g. `aux.c` or `con.txt`) are automatically prefixed with `_`, ensuring archives unpack safely onto `exFAT` micro-SD cards, `NTFS` external SSDs, and Windows partitions.
- **Smart Directory Hoisting**: Automatically detects redundant double-nested wrappers (e.g., `Destination/GameName/GameName/game.exe` → `Destination/GameName/game.exe`) and hoists them cleanly before checksum verification and executable detection.
- **Pre-Flight Write Probe & Storage Margin Validation**: Before allocating network streams or buffers, DirectCrate drops a temporary probe file (`.directcrate_write_probe_*.tmp`) to guarantee destination write permissions (`EACCES` check) and compares remote `Content-Length` with available free space (`+1 GB` safety buffer).
- **Mid-Stream Storage Floor & Safe Auto-Pause (`needs_disk`)**: If destination storage is exhausted mid-stream (`ENOSPC`) or write permission is lost (`EACCES`), DirectCrate enters `needs_disk` pause mode instead of crashing or discarding extracted files. It automatically polls the filesystem every 2 seconds and resumes extraction the instant at least `512 MB` of space is freed.
- **Decompression Panic Traps**: All extractors (RAR5, ZIP, 7Z, TAR) and decompression workers are shielded with panic recovery handlers (`recover()`), ensuring malformed or corrupted archive headers produce clean status errors rather than unhandled process terminations.
- **Instant Abort & Cleanup**: Immediately aborts all active network connections, deletes partially extracted files, and removes any newly created empty directories.
- **Automated Checksum Verification**: Scans for `.sha256`, `sha256sums.txt`, `SHA256SUMS`, `.md5`, `md5sums.txt`, `checksums.md5`, or `MD5SUMS` in the root of the extracted folder and verifies file integrity with streaming hashing.

---

## Beginner & Everyday User Guide (Plain Language FAQ)

If you have zero technical or IT background, this guide explains everything you need to know about DirectCrate in simple, everyday language.

### 1. 100% On-Device & Private (No Cloud Servers)
**Does DirectCrate send my files or links to a cloud server?**
No. DirectCrate runs **100% on your own device** (your smartphone, Steam Deck, laptop, or desktop PC). When you paste a link, your device connects directly to that download link and streams the files straight onto your drive. No external servers, proxies, or middlemen ever touch, see, or store your links or files. DirectCrate is completely offline-first, private, and has zero tracking, zero ads, zero user accounts, and zero analytics.

### 2. Saves Half Your Storage Space (Zero Temporary Archive File)
**How does DirectCrate save 50% of my drive space?**
Traditional download programs have a major hidden flaw—the **double storage penalty**:
- If you download a 10 GB `.rar` or `.zip` game, traditional programs first save the 10 GB archive onto your drive.
- Then, they extract the 10 GB game folder into a second location, requiring another 10 GB of free space.
- **Total space needed: 20 GB!** If your drive only has 14 GB free, traditional downloads fail.

DirectCrate eliminates the temporary archive entirely. It unpacks the data directly in memory as it downloads over the internet and writes only the final unpacked files to your drive. A 10 GB game only needs 10 GB of storage total, saving half your space and leaving zero temporary files to delete.

### 3. Why Your Device Gets Warm & Uses Battery
**Why does my phone or handheld feel warm and use more battery during extraction?**
Downloading and unpacking massive files in real time at high speed puts all 4 major hardware subsystems of your device to work at once:
- **Network:** Downloading gigabytes of data over your Wi-Fi or cellular chip.
- **RAM (Memory):** Holding incoming stream buffers in working memory.
- **CPU (Processor):** Actively running decompression calculations (unpacking complex formats like RAR5 or 7Z on the fly).
- **Storage:** Writing thousands of files directly to your SSD or flash drive.

This combined workload is very similar to running a modern 3D video game. Moderate warmth and battery usage are completely normal and safe. You can observe your real-time **App RAM** and **Device Temp** right inside the app at the bottom of the screen.

### 4. Full Speed (`Unlimited`) vs. Cool / Balanced Mode (`Speed Cap`)
**Should I download at Unlimited speed or set a Speed Cap?**
- **Unlimited Mode:** DirectCrate downloads and extracts as fast as your internet connection and device can go. This finishes your download in the shortest possible time. It is ideal when your device is plugged into a wall charger or running on a desktop PC.
- **Speed Cap (Cool & Battery Saver):** If you are playing on a battery-powered handheld (like a Steam Deck) or a smartphone, you can select a speed cap (e.g., 10 MB/s or 25 MB/s) from the speed dropdown. Capping the speed gives your hardware breathing room, lowers processor usage, keeps your device cool and comfortable to hold, reduces fan noise, and significantly extends battery life.

### 5. Direct Links vs. Webpage Links
**What kind of links can DirectCrate download?**
DirectCrate requires a **direct file download link**—a URL that immediately starts transmitting the archive file when accessed (usually ending in `.rar`, `.zip`, `.7z`, `.tar.gz`, or containing a direct stream token).

DirectCrate **cannot** download from webpage links (such as landing pages with countdown timers, pop-up ads, "Wait 30 seconds" buttons, or "Click to verify you are human" CAPTCHA challenges).
- **How to get a direct link:** In your web browser, click the download button so the file begins downloading in your browser. Then, open your browser's download manager (`Ctrl+J` or menu → Downloads), right-click the active download, and choose **"Copy download link"** (or right-click the final "Download" button on the site and choose **"Copy Link Address"**). Paste that direct link into DirectCrate.

### 6. Multi-Part Archives (`part1.rar`, `part2.rar`, etc.)
**How do I download archives split into multiple parts?**
Many large games and datasets are divided into smaller numbered parts (`game.part1.rar`, `game.part2.rar`, etc.).
1. In DirectCrate, switch the mode selector from **Single URL** to **Multi-Part URLs**.
2. Paste all your part links into the input box (one link per line or separated by spaces).
3. Select your destination folder and click **Download**.
DirectCrate will download each part in order, automatically stitching them together into one complete, seamless folder with zero extra steps.

### 7. Password-Protected Archives
**What happens if an archive has a password?**
If an archive is locked with a password (common with `.7z` and `.rar` files), DirectCrate detects the encryption before extraction begins. A clean password prompt will appear on your screen. Type or paste the password once, click Confirm, and DirectCrate will decrypt and extract all files seamlessly on the fly.

### 8. Speed Fluctuations During Extraction
**Why does the download speed dip or fluctuate while extracting?**
Temporary speed dips are completely normal and part of how computer file storage works:
- When an archive reaches a section containing thousands of tiny files (such as icons, game scripts, shaders, or text configs), your device's operating system must create and register thousands of individual filesystem entries on your drive. This briefly slows down throughput.
- As soon as the stream moves on to larger files (like 3D textures, models, or video cutscenes), the download speed instantly jumps back up to your maximum internet speed.

### 9. Background Downloads & Abort / Disconnect
**Can I leave the app downloading in the background? What if the Wi-Fi drops or I want to cancel?**
- **Background Downloads:** On Android, you can press Home, switch to other apps, or turn off your screen while downloading—DirectCrate continues running in the background with a live progress bar in your notification shade (just avoid swiping away or force-closing the app). On desktop, minimizing or closing the window keeps active downloads running safely until finished.
- **Connection Drops:** If your Wi-Fi or cellular network temporarily drops, DirectCrate safely pauses the stream without deleting any files already unpacked. Once your connection returns (or if the link expired and you provide a refreshed link), it resumes right from the exact byte where it paused.
- **Abort Button:** If you change your mind and click the red **Abort** button, DirectCrate immediately halts all network traffic and cleanly deletes any unfinished files so no leftover clutter remains on your storage.

### 10. Unknown App Warnings & Storage Permission Prompts
**Why did Windows or Android show an "Unknown App" warning and ask for storage permission?**
- **Unknown App / SmartScreen Warning:** DirectCrate is an independent, open-source project published directly on GitHub rather than sold through commercial app stores (like Google Play or the Microsoft Store). Because it is distributed directly by the developer, Windows SmartScreen and Android display an "Unknown App" prompt on first install. DirectCrate is completely transparent, open-source, and contains zero adware, malware, or trackers.
- **Storage Permission on Android:** Android requires the "All files access" (`MANAGE_EXTERNAL_STORAGE`) permission so DirectCrate has the authority to create your extracted game or application folder inside your public `Download` folder or on an external micro-SD card. DirectCrate strictly writes to the folder you choose and never reads, scans, or uploads your private photos, contacts, or personal files.

---

## Format Compatibility Matrix

| Format | Multi-Part Chaining | Selective File Tree | Password Support | Streaming Strategy |
| :--- | :---: | :---: | :---: | :--- |
| **RAR5** (`.rar`, `.part1.rar`, `.r00`) | **Yes** | Exclude patterns | **Yes** | Sequential streaming via `rardecode/v2` across single or multi-part URLs with mid-stream disk checks. |
| **ZIP** (`.zip`) | Single URL | **Yes** | — | Range-backed `httpreaderat` parses the Central Directory first; selectively streams only chosen files. |
| **7Z** (`.7z`) | Single URL | **Yes** | **Yes** | Range-backed `httpreaderat` inspects 7Z metadata headers; extracts selected files with AES-256 password support. |
| **TAR / TAR.GZ / TAR.ZST** | **Yes** | Exclude patterns | — | Direct sequential streaming decompression (`archive/tar`, `gzip`, `zstd`) with multi-part URL chaining. |

---

## Device Resource Footprint & Hardware Transparency (Network, RAM, CPU, Storage)

DirectCrate is engineered for extreme hardware efficiency with full transparency across 4 core physical subsystems. Operating directly in memory without writing temporary archive files drastically lowers power draw, memory bandwidth, and storage wear on handhelds and laptops.

| Hardware Subsystem | How DirectCrate Utilizes It | Device Safeguards & Limits |
| :--- | :--- | :--- |
| **Network (Bandwidth)** | Direct HTTP/HTTPS streaming with parallel HTTP Range prefetching (1–8 concurrent connections, default 4). Supports token-bucket rate limiting (`5–50 MB/s` or `Unlimited`), persistent HTTP/2 and HTTP/1.1 connection pooling, automatic TCP socket reconnection with exponential backoff, and mid-stream URL hot-swapping for expiring CDN tokens. | `globalRateLimiter` enforces strict bandwidth caps without packet spikes; `DisableCompression: true` prevents upstream proxy tampering; range probing falls back gracefully from `HEAD` to `GET Range: bytes=0-0` on `405`/`403`. |
| **RAM (Process Memory)** | Constant, predictable process memory footprint typically hovering between **30 MB and 64 MB** across Linux, Windows, and Android. Memory is tracked live in the telemetry card (`App RAM`). | Uses `sync.Pool` to recycle 1 MB–16 MB I/O buffers and 256 KB buffered writers without triggering Go runtime GC pauses, eliminating Out-Of-Memory (OOM) risks on RAM-constrained 4 GB–16 GB devices. |
| **CPU (Decompression)** | Decompresses streaming bytes (RAR5, ZIP/deflate, 7Z/LZMA2, TAR Zstd/Gzip) on the fly using native Go decoders. Workers execute at normal user-space priority. | Lightweight CPU footprint preserves battery longevity on Steam Deck and Android mobile devices, preventing thermal throttling and keeping fan noise minimal. |
| **Storage (Disk I/O)** | **Zero intermediate archive files.** Decompressed data streams directly to final target files on disk (`io.CopyBuffer` via 256 KB `bufio.Writer`). | Pre-flight write probe validates directory writability; pre-flight capacity check verifies space against remote `Content-Length` (`+1 GB` safety buffer); mid-stream `needs_disk` pauses extraction on `ENOSPC` and auto-resumes once `512 MB` is cleared. |

### Live Hardware Telemetry

DirectCrate samples and displays hardware vitals live in the web interface and API:
- **`APP RAM` (MB)**: Real-time Go runtime system memory (`m.Sys / 1024 / 1024`) updated every 200ms–1s across Linux, Windows, and Android.
- **`DEVICE TEMP` (°C)**: Device temperature sampled directly from hardware thermal sensors:
  - **Linux / SteamOS / Bazzite**: Scans `/sys/class/thermal/thermal_zone*/temp` (selecting the peak package/SoC temperature between 15°C and 115°C).
  - **Android**: Samples battery and SoC thermals via `BatteryManager.EXTRA_TEMPERATURE` and Android 10+ `PowerManager.currentThermalStatus`, categorized into `Normal`, `Warm` (amber), and `Hot` (red).
  - **Windows & Unsupported Platforms**: Cleanly hides the temperature badge without breaking layout or spamming errors.

---

## Complete Local Storage Transparency

DirectCrate has a strict and deterministic storage footprint. It never scatters files across undocumented directories.

### 1. Application Configuration & State Directory

All configuration, state, history, and diagnostics are stored in an OS-standard directory:
- **Linux & SteamOS**: `~/.config/directcrate/`
- **Windows**: `%APPDATA%\directcrate\` (typically `C:\Users\<User>\AppData\Roaming\directcrate\`)
- **Android APK**: Application-internal private storage (`/data/user/0/com.musakpg.directcrate/files/`)
- **Android Termux**: `~/.config/directcrate/`

| File / Subdirectory | Purpose | Management & Lifecycle |
| :--- | :--- | :--- |
| **`settings.json`** | Stores persistent user configuration (default destination, worker count, buffer size, speed limit, directory conflict mode, power action, notification preferences, live progress notifications, exclude patterns, and theme). | Saved atomically on change; loaded on startup. |
| **`settings.json.corrupt.bak`** | Preserves corrupted settings JSON if power loss or sudden cutoff occurs during writes. | Created automatically when invalid JSON is detected; clean defaults are loaded without crashing. |
| **`history.json`** | Records completed and failed extraction records (folder name, destination, detected executables, uncompressed gigabytes, file counts, average speed, elapsed time, and checksum verification status). | Append-only; can be cleared via UI or `DELETE /api/history`. |
| **`history.json.corrupt.bak`** | Preserves corrupted history JSON if interrupted writes corrupt the history file. | Created automatically when invalid JSON is detected; resets history safely. |
| **`directcrate.log`** | Standard operational log recording port binding, lifecycle events, format detection, CDN pause/resume events, checksum validations, and Steam integrations. | Automatically rotated to `directcrate.log.old` when exceeding `5 MB`. |
| **`directcrate.log.old`** | Archived backup of the previous operational log. | Overwritten on subsequent rotation. |
| **`crash.log`** | Captures unhandled runtime panics with full stack traces (`runtime/debug.Stack()`). | Appended only when an unexpected crash occurs; inspectable in Log Viewer. |
| **`webapp-profile/`** | Isolated Chromium/Edge user-data profile for dedicated desktop app-mode windows. | Created automatically when launching standalone app windows; kept separate from your default browsing profile. |

### 2. Operating System Desktop & Launcher Registrations

To ensure native application icons and launcher shortcuts work out-of-the-box when running release binaries directly from GitHub Releases:

- **Windows Native PE Executable Icon & Start Menu Shortcut**:
  - `directcrate-windows-amd64.exe` embeds the official DirectCrate application icon directly into the executable as a Windows PE resource (`.syso` COFF `.rsrc` section). File Explorer, the Windows Taskbar, and Alt+Tab immediately render the branded icon without requiring separate `.ico` files.
  - On startup in a user session, DirectCrate ensures a Start Menu shortcut at `%APPDATA%\Microsoft\Windows\Start Menu\Programs\DirectCrate.lnk` pointing to `os.Executable()` with AppUserModelID `DirectCrate.App` so Windows Search and Taskbar pinning bind cleanly to the application identity.
- **Linux Desktop Entry & Scalable Icon Registration**:
  - On startup in a user session (`$HOME` set), DirectCrate automatically registers:
    - `~/.local/share/icons/hicolor/scalable/apps/directcrate.svg`: Scalable vector icon (unified `#09090B` + `#FAFAFA` + `#10B981` geometry).
    - `~/.local/share/applications/directcrate.desktop`: Standard XDG desktop entry with `Exec=<resolved_executable_path>`, `Icon=directcrate`, and `StartupWMClass=DirectCrate`.
  - DirectCrate executes `update-desktop-database` and `gtk-update-icon-cache` silently in the background so Wayland compositors (KDE Plasma, GNOME Wayland) immediately recognize and bind the taskbar icon.
  - When launching Chromium or Brave standalone app windows on Linux (including Flatpaks), DirectCrate sets `CHROME_DESKTOP=directcrate.desktop` and `BAMF_DESKTOP_FILE_HINT` in the child process environment so the window reports `directcrate.desktop` as its Wayland app ID, preventing the generic "W" Wayland placeholder icon.

### 3. Destination Extraction Directory

Inside your chosen extraction folder, DirectCrate creates only:
1. **Extracted Content**: The files and subdirectories extracted from the archive.
2. **Temporary Folder-Flattening Workspace**: If an archive has a redundant single top-level folder, DirectCrate hoists the contents via a temporary directory named `.directcrate_flatten_<timestamp>`. This folder exists only during the hoisting operation and is deleted immediately upon completion.
3. **No Leftover Partial Files**: If an extraction is canceled or fails fatally, the active partially written file is closed and deleted (`os.Remove`), and newly created empty folders are pruned.

### 4. Zero Telemetry & Privacy Guarantee

DirectCrate is 100% private, self-hosted, and offline-first:
- **Zero Telemetry**: No usage metrics, pings, crash reports, or analytics are ever transmitted to external servers.
- **Zero Third-Party APIs**: External network requests occur strictly to your provided archive URLs, plus public Steam Store APIs for non-Steam game cover art lookups when explicitly requested.

---

## Lifecycle Safeguards & Fault Tolerance Architecture

DirectCrate incorporates multi-layer fault tolerance across every stage of the extraction lifecycle:

```text
┌────────────────────────────────────────────────────────────────────────┐
│                   DirectCrate Fault Tolerance Pipeline                 │
├────────────────────────────────────────────────────────────────────────┤
│ 1. Pre-Open & Bootstrap                                                │
│    ├── JSON Corrupt Recovery: <file>.corrupt.bak backup + safe default │
│    ├── Single-Instance Identity: GET /api/status ("app": "DirectCrate")│
│    └── Android Idempotent JNI: background startServer + port detection │
├────────────────────────────────────────────────────────────────────────┤
│ 2. Pre-Download Pre-Flight                                             │
│    ├── Writable Probe Test: .directcrate_write_probe_*.tmp             │
│    ├── Free Space Check: freeBytes >= remoteSize + 1GB safety buffer   │
│    └── Permission Verifier: Android MANAGE_EXTERNAL_STORAGE checks     │
├────────────────────────────────────────────────────────────────────────┤
│ 3. Active Stream & Extraction                                          │
│    ├── Path Traversal Defense: Blocks '../' and sanitizes bad chars    │
│    ├── Decompression Panic Traps: recover() wraps all 4 extractors     │
│    ├── ENOSPC / EACCES Auto-Pause: needs_disk state (512MB resume)     │
│    └── HTTP 403/410 Auto-Pause: needs_url state (in-place link swap)   │
├────────────────────────────────────────────────────────────────────────┤
│ 4. Post-Download Cleanup                                               │
│    ├── Lock Release: systemd-inhibit, SetThreadExecutionState, locks   │
│    ├── Taskbar Bar Reset: D-Bus Unity.LauncherEntry / ITaskbarList3    │
│    └── Smart Auto-Exit: 10s Desktop idle exit / 5s Android service stop│
└────────────────────────────────────────────────────────────────────────┘
```

### 1. Pre-Download Disk Space & Directory Write-Permission Validation
- **Ephemeral Write-Probe**: Before negotiating remote connections or allocating decompression buffers, DirectCrate creates and immediately removes an ephemeral probe file (`.directcrate_write_probe_<timestamp>.tmp`) inside the chosen destination directory. If permission is denied (`os.ErrPermission`, `syscall.EACCES`), extraction aborts immediately with a clear explanatory error before any data transfer begins.
- **Pre-Flight Capacity Calculation**: For archives broadcasting remote `Content-Length` headers, DirectCrate queries filesystem metrics via OS-level disk statistics (`statvfs` on Linux/Android, `GetDiskFreeSpaceExW` on Windows) and enforces a pre-flight formula:
  $$\text{Free Space} \ge \text{Archive Size} + 1.0\text{ GB Safety Buffer}$$
  If available capacity is below this threshold, the task fails gracefully before allocating network buffers.
- **Mid-Stream Storage Floor & Safe Auto-Pause (`needs_disk`)**: When extracting sequential multi-gigabyte archives onto small drives or micro-SD cards, free space can fluctuate due to other running processes or game installs. If a disk write encounters `ENOSPC` ("no space left on device"), `EACCES`, or "disk full":
  1. The extraction stream automatically enters the `needs_disk` paused state without discarding partially unpacked files.
  2. The Web UI and notifications switch to alert mode, indicating the required destination folder.
  3. DirectCrate polls filesystem capacity every 2 seconds. The moment the user frees up disk space and available capacity exceeds `512 MB`, the stream automatically resumes extraction at the exact byte boundary.

### 2. Automatic State Backup on Corrupted JSON (`.corrupt.bak`)
Sudden battery cutoffs, hard reboots, or power failures can leave JSON configuration or history files truncated or containing null bytes. DirectCrate guarantees startup resilience:
- When parsing `settings.json` or `history.json`, any `json.SyntaxError` or deserialization failure triggers an automatic backup to `<file>.corrupt.bak`.
- DirectCrate logs a diagnostic warning (`[SETTINGS:WARN]` or `[HISTORY:WARN]`) in `directcrate.log`.
- Valid built-in defaults are instantiated immediately, allowing DirectCrate to boot without crashing, infinite loops, or requiring manual configuration deletion.

### 3. Archive Path Sanitization & Traversal Prevention
Malicious or poorly authored archives may contain relative path traversal components (`../../`) or characters prohibited on certain filesystems:
- **Directory Traversal Defense**: All archive headers (RAR, ZIP, 7Z, TAR) pass through `sanitizeArchiveEntryPath()`. Any path attempting directory traversal (e.g., `../../etc/shadow` or `..\Windows\System32`) resolves to an empty path and is rejected immediately.
- **Cross-Filesystem Normalization**: Characters that are illegal on Windows, NTFS, or Android FAT/exFAT filesystems (`:`, `*`, `?`, `"`, `<`, `>`, `|`) are replaced with underscores (`_`). Trailing spaces and periods (`Folder... /file.txt `) are trimmed to prevent filesystem corruption or unreachable directories. ASCII control characters (`< 0x20` and `0x7F`) and drive letter roots (`C:\`) are safely neutralized.
- **Windows Reserved Device Name Prefixing**: MS-DOS and Windows reserved device names (`CON`, `PRN`, `AUX`, `NUL`, `COM1`–`COM9`, `LPT1`–`LPT9`, case-insensitive, matching full segment or stem before the first dot, e.g. `aux.c` or `con.txt`) are automatically prefixed with `_` (e.g. `_aux.c`, `_con.txt`), preventing Win32 creation failures or system device locks.

### 4. Android 15/16 Fail-Safe Engine Bootstrap & One-Tap Recovery
On Android 12 through 16, modern system restrictions can delay or restrict background foreground service startup (`ForegroundServiceStartNotAllowedException`):
- **Direct JNI Background Bootstrap**: In `MainActivity.kt`, the native Go engine is launched directly via `DirectCrateBridge.startServer()` on a background IO thread before polling begins. This ensures the loopback HTTP server (`127.0.0.1:<port>`) binds immediately, independent of service scheduler timing.
- **Dynamic Mutex-Protected Port Resolution**: `startServer` is protected by a thread-safe mutex and returns the exact bound port (`int`), cleanly adapting to port collisions without hardcoded mismatches.
- **Safe Foreground Service Invocation**: `startDirectCrateService()` is executed inside guarded exception blocks to ensure foreground service startup delays never crash the app.
- **Resilient 15-Second Status Polling**: The frontend polls `http://127.0.0.1:<boundPort>/api/status` for up to 15 seconds. If connection is not established after 3 seconds, `startServer` is re-triggered automatically as an idempotent safeguard.
- **One-Tap Connection Recovery**: If startup encounters an unusual system blockage, an interactive error screen renders with a **"Retry Connection"** button, allowing users to re-initialize the backend engine with a single tap.

---

## Linux (Desktop, SteamOS & Bazzite) Behavior & OS Hooks

DirectCrate provides dedicated desktop integration for Linux, Steam Deck (SteamOS), and gaming distributions like Bazzite:

```text
┌────────────────────────────────────────────────────────────────────────┐
│                         Linux Integration Hooks                        │
├───────────────────────┬────────────────────────────────────────────────┤
│ Browser Launcher      │ Standalone Chromium App Window (--app=<url>)   │
│ Taskbar Progress      │ D-Bus Unity.LauncherEntry Update via gdbus     │
│ Power Management      │ systemd-inhibit (sleep/idle lock during tasks) │
│ Desktop Notifications │ notify-send -a DirectCrate                     │
│ Audio Cues            │ pw-play / paplay (freedesktop complete.oga)    │
│ Steam Integration     │ shortcuts.vdf parser/writer + Steam Grid CDN   │
│ Post-Task Power       │ systemctl suspend / poweroff / reboot          │
└───────────────────────┴────────────────────────────────────────────────┘
```

1. **Standalone App-Mode Launcher**:
   - On startup, DirectCrate inspects native `PATH` binaries (`google-chrome`, `google-chrome-stable`, `chromium`, `chromium-browser`, `brave-browser`, `microsoft-edge`, `vivaldi`).
   - If native binaries are absent, it queries Flatpak app IDs via `flatpak info <id>` (`com.google.Chrome`, `org.chromium.Chromium`, `com.brave.Browser`, `com.microsoft.Edge`, `com.vivaldi.Vivaldi`).
   - Launches in dedicated application window mode via `--app=http://localhost:<port>`, giving it a borderless, native desktop look without browser URL bars or tabs.
   - For native binaries, uses `--user-data-dir=~/.config/directcrate/webapp-profile` to isolate state. For Flatpaks, omits `--user-data-dir` to prevent sandbox permission errors.
   - Sets `CHROME_DESKTOP=directcrate.desktop` and `BAMF_DESKTOP_FILE_HINT` in the child environment so Wayland window managers immediately map the window to `directcrate.desktop` and display the official DirectCrate taskbar icon.
   - Returns immediately upon launch without calling `xdg-open` or opening duplicate tabs.
2. **Real-Time Taskbar Progress Bar (`D-Bus Unity.LauncherEntry`)**:
   - Emits `com.canonical.Unity.LauncherEntry.Update` signals via `gdbus` for `application://directcrate.desktop`.
   - Displays a live progress bar on the taskbar/dock in KDE Plasma, Bazzite, and GNOME environments.
   - Can be toggled on/off in Settings via **Live progress notifications**. When disabled or idle, the taskbar bar clears immediately (`progress-visible=false`).
3. **Idle & Sleep Prevention (`systemd-inhibit`)**:
   - During active extractions, DirectCrate acquires an inhibitor lock:
     `systemd-inhibit --what=idle:sleep --who=DirectCrate --why="Streaming archive extraction in progress" sleep infinity`
   - Prevents your Steam Deck or Linux PC from going to sleep while downloading large multi-gigabyte archives.
4. **Desktop & Audio Notifications**:
   - Sends desktop completion notifications using `notify-send -a DirectCrate`.
   - Plays audio completion cues via PipeWire (`pw-play`) or PulseAudio (`paplay`) using `/usr/share/sounds/freedesktop/stereo/complete.oga`.
5. **Steam Shortcuts & Grid Cover Art**:
   - Automatically detects the active Steam user under `~/.local/share/Steam/userdata/<id>/config/` or `~/.steam/steam/userdata/<id>/config/`.
   - Backs up `shortcuts.vdf` to `shortcuts.vdf.bak` and appends a Non-Steam Game shortcut.
   - Queries the public Steam Store API to download official game artwork into Steam's `grid/` directory:
     - Vertical capsule cover: `<appid>p.jpg`
     - Hero banner: `<appid>_hero.jpg`
     - Transparent title logo: `<appid>_logo.png`
6. **Overnight Power Actions**:
   - Configure DirectCrate to automatically execute `systemctl suspend`, `systemctl poweroff`, `systemctl reboot`, or exit the process once the queue empties.

---

## Windows Behavior & OS Hooks

On Windows 10 and 11, DirectCrate provides native system hooks:

```text
┌────────────────────────────────────────────────────────────────────────┐
│                        Windows Integration Hooks                       │
├───────────────────────┬────────────────────────────────────────────────┤
│ Browser Launcher      │ Standalone Edge/Chrome App Window (--app=...)  │
│ Taskbar Progress      │ ITaskbarList3 COM (Normal Green / Paused Yellow)│
│ Power Management      │ Native Thread Execution State Sleep Inhibit    │
│ Steam Integration     │ shortcuts.vdf parser/writer + Steam Grid CDN   │
│ Post-Task Power       │ powrprof.dll SetSuspendState / shutdown.exe    │
│ Remote LAN Access     │ 0.0.0.0 binding for Handheld / Mobile access   │
└───────────────────────┴────────────────────────────────────────────────┘
```

1. **Standalone App-Mode Launcher**:
   - Inspects standard installation directories (`%ProgramFiles(x86)%`, `%ProgramFiles%`, `%LOCALAPPDATA%`) and `PATH` for Microsoft Edge (`msedge.exe`) or Google Chrome (`chrome.exe`).
   - If located, launches `msedge.exe --app=http://localhost:<port> --user-data-dir=...` as a dedicated standalone window and returns immediately.
   - Falls back to `rundll32.exe url.dll,FileProtocolHandler` only if neither Edge nor Chrome can be started.
   - Automatically registers `%AppData%\Microsoft\Windows\Start Menu\Programs\DirectCrate.lnk` pointing to `os.Executable()` with AppUserModelID `DirectCrate.App` and embeds the branded icon directly into the `.exe` as a Windows PE resource.
2. **Native Taskbar Progress Bar (`ITaskbarList3`)**:
   - Integrates with the Windows Taskbar COM interface (`ITaskbarList3::SetProgressValue` and `SetProgressState`) bound to the standalone DirectCrate window handle (`HWND`).
   - While streaming: Displays a green normal progress bar (`TBPF_NORMAL`, 0–100%).
   - While paused (`needs_url` / `needs_password`): Switches to a yellow paused bar (`TBPF_PAUSED`).
   - When disabled in Settings or idle: Resets to `TBPF_NOPROGRESS` to clear the taskbar bar.
3. **Steam Shortcuts & Grid Cover Art**:
   - Scans standard Windows Steam installations (`C:\Program Files (x86)\Steam\userdata\<id>\config\shortcuts.vdf`).
   - Creates a `.bak` backup and injects shortcuts and Steam Grid artwork.
4. **Windows Overnight Power Actions**:
   - Supports automated post-queue power management:
     - **Sleep**: `rundll32.exe powrprof.dll,SetSuspendState 0,1,0`
     - **Shutdown**: `shutdown /s /t 15`
5. **Network & Remote Handheld Access**:
   - Binds to `0.0.0.0:<port>` by default, allowing you to access DirectCrate from your phone, laptop, or gaming handheld on your local Wi-Fi or across a Tailscale / VPN mesh. Includes a built-in remote directory picker.

---

## Android 15/16 Architecture & Native Integration

DirectCrate supports Android through a native application architecture designed specifically for Android 15 and 16.

```text
┌────────────────────────────────────────────────────────────────────────┐
│                   DirectCrate Android Application                      │
├────────────────────────────────────────────────────────────────────────┤
│ MainActivity (AppCompatActivity)                                       │
│   ├── Edge-to-Edge System Bars (WindowInsetsControllerCompat)          │
│   ├── Fullscreen Dark/Light WebView                                    │
│   └── AndroidTheme JavaScript Bridge (Live dynamic bar tinting)        │
├────────────────────────────────────────────────────────────────────────┤
│ DirectCrateService (Foreground Service - FOREGROUND_SERVICE_DATA_SYNC) │
│   ├── PowerManager.PARTIAL_WAKE_LOCK (Keeps CPU active during screen-off)│
│   ├── WifiManager.WifiLock (Maintains high-throughput Wi-Fi streaming) │
│   ├── Real-Time Notification Shade Progress Bar (setProgress 0-100)    │
│   └── Distinct Task Completion Notification Alert                      │
├────────────────────────────────────────────────────────────────────────┤
│ Embedded Native Engine (libdirectcrate.so for arm64-v8a)               │
│   ├── Mandatory 16KB ELF Page Alignment (-Wl,-z,max-page-size=16384)   │
│   └── JNI Bridge: startServer / stopServer / getProgressJson           │
└────────────────────────────────────────────────────────────────────────┘
```

### 1. Mandatory 16KB ELF Page Alignment (Android 15 & 16)

Starting with Android 15, devices with 16KB kernel memory page sizes require all shared libraries to be built with 16KB page alignment. DirectCrate builds `libdirectcrate.so` using:

```bash
-ldflags="-s -w -extldflags=-Wl,-z,max-page-size=16384"
```

This guarantees `libdirectcrate.so` loads cleanly on modern 16KB page-size hardware without startup crashes.

### 2. Live Notification Shade Progress Bar & Interactive Actions

In `DirectCrateService.kt`, a background monitor polls `DirectCrateBridge.getProgressJson()` every second:
- **When Active & Enabled**: Updates the ongoing foreground notification with `.setProgress(100, percent, false)` and displays `"$percent% • $speed"` or `"Paused"`. Includes immediate action buttons:
  - **"Abort"**: Instantly halts all active connections and cleans up partial files.
  - **"Stop / Exit"**: Safely terminates the background engine.
- **Clean Shutdown on Swipe-Kill When Idle**: Overriding `onTaskRemoved` guarantees that when DirectCrate is dismissed from Recent Apps while idle, the service immediately cancels the foreground notification and cleanly stops itself (`stopForeground(STOP_FOREGROUND_REMOVE)` and `stopSelf()`). If an extraction is active, the foreground service stays alive until the download finishes.
- **When Disabled**: If **Live progress notifications** is toggled off in Settings, the notification remains in a static `"DirectCrate Engine Ready"` state.
- **Completion Alert**: When extraction finishes, posts an auto-canceling completion notification (`"DirectCrate Extraction Complete"`) and restores the foreground service notification to idle.

### 3. Android Permissions Transparency Table

Every permission declared in `android/app/src/main/AndroidManifest.xml` serves a specific technical function:

| Android Permission | Why DirectCrate Needs It |
| :--- | :--- |
| `android.permission.INTERNET` | Mandatory to download and stream remote archives from HTTP/HTTPS endpoints. |
| `android.permission.ACCESS_NETWORK_STATE` | Verifies active network status and detects interface transitions. |
| `android.permission.ACCESS_WIFI_STATE` | Resolves local IPv4 addresses for LAN streaming and status reporting. |
| `android.permission.FOREGROUND_SERVICE` | Runs the Go streaming backend in the background so tasks are not killed when switching apps. |
| `android.permission.FOREGROUND_SERVICE_DATA_SYNC` | Complies with Android 14+ foreground service classification for data transfer and synchronization. |
| `android.permission.WAKE_LOCK` | Holds a `PARTIAL_WAKE_LOCK` and high-performance `WifiLock` so extractions complete when the screen turns off. |
| `android.permission.POST_NOTIFICATIONS` | (Android 13+) Required to show the persistent foreground service notification and task completion alerts. |
| `android.permission.MANAGE_EXTERNAL_STORAGE` | (Android 11+) Grants access to write extracted files directly to micro-SD cards, external drives, and `/storage/emulated/0/Download/DirectCrate`. Falls back to standard media storage permissions if denied. |

### 4. Adaptive & Monochrome Icons

Includes vector adaptive launcher icons with dark zinc (`#09090B`) backgrounds, brand white (`#FAFAFA`) isometric cube geometry, and emerald green (`#10B981`) downward arrows (`ic_launcher.xml`, `ic_launcher_round.xml`, `ic_launcher_foreground.xml`), full monochrome icon support for Material You theming on Android 13+, and pre-rendered fallback mipmap PNGs (`mipmap-mdpi` through `mipmap-xxxhdpi`) ensuring clean rendering across legacy and modern Android launchers and the Recents task switcher.

### 5. Fail-Safe Engine Bootstrap & One-Tap Recovery

To ensure immediate responsiveness across diverse Android 15/16 OEM power and background execution limits:
- **Direct Engine JNI Initialization**: Starts the Go backend on a background IO thread directly upon activity launch, ensuring `:9090` (or fallback `:9091+`) is bound immediately without depending on foreground service scheduling.
- **Fail-Safe Exception Handling**: Wraps `startForegroundService` in guarded exception blocks to absorb `ForegroundServiceStartNotAllowedException` on modern Android versions.
- **Idempotent 15-Second Watchdog**: Polls `127.0.0.1:<boundPort>/api/status` for up to 15 seconds with an automatic 3-second re-invocation safeguard.
- **Interactive Recovery Screen**: If loopback connection is delayed, renders an in-app error page with an interactive **"Retry Connection"** button calling `window.AndroidTheme.retryConnection()` to re-initialize the engine with a single tap.

---

## Process Lifecycle & Single-Instance Architecture

DirectCrate implements intelligent, battery- and memory-conscious process management across both Desktop (Linux & Windows) and Mobile (Android 15/16). It ensures that multiple invocations never create conflicting background processes, while keeping active extractions alive in the background and eliminating orphan idle processes.

### 1. Desktop Single-Instance Locking & Window Refocus

When launching DirectCrate on Linux or Windows (e.g. from an application launcher, terminal, or desktop shortcut):
- **Fast Startup Probe**: Before binding to fallback ports (`:9091+`) or initializing logs, DirectCrate issues a 300ms HTTP `GET` probe to `http://127.0.0.1:<port>/api/status` (default `:9090`).
- **Instant Focus Signal**: If an active DirectCrate instance is already listening on that port, the newly spawned CLI sends a `POST /api/focus` signal to the existing instance and immediately exits (`os.Exit(0)`).
- **Zero Log or Process Clutter**: The second process exits cleanly without writing duplicate logs or starting secondary background workers. The running server receives `/api/focus` and invokes `launchBrowser()` to refocus or reopen the standalone app window.

### 2. Desktop Graceful Idle Auto-Exit (10-Second Window)

DirectCrate automatically monitors active client connections using Server-Sent Events (`/events`):
- **UI Client Tracking**: Once at least one web UI client has connected (`hadUiClient == true`), DirectCrate enters active session mode.
- **Background Extraction Protection**: When you close all browser tabs or standalone app windows (`len(clients) == 0`), DirectCrate verifies whether any archive extraction is actively decompressing, waiting for a refreshed URL/password, or waiting in the sequential batch queue.
  - If a job or queue is active, DirectCrate **never terminates**. It continues downloading, decompressing, calculating checksums, and executing post-completion power actions in the background.
- **10-Second Graceful Shutdown**: Once all active and queued extractions are finished and zero UI clients remain connected for 10 consecutive seconds, DirectCrate initiates a graceful shutdown (`http.Server.Shutdown()`) and exits cleanly, leaving **0 orphan processes** running in the background.
- **Seamless Reconnect**: If you refresh the page or open a new window within the 10-second window, the auto-exit timer is immediately cancelled.

### 3. Android Smart Foreground Service Lifecycle

On Android 15/16, DirectCrate operates as an official foreground service (`FOREGROUND_SERVICE_DATA_SYNC`):
- **UI Visibility Tracking**: `MainActivity` tracks lifecycle state (`onResume`, `onPause`, `onDestroy`).
- **Uninterrupted Background Downloads**: Swiping DirectCrate away from Android's Recents app switcher or switching to another app does **not** terminate active extractions. DirectCrate holds a `PARTIAL_WAKE_LOCK` and high-performance `WifiLock`, continuing the stream and updating the Android notification shade in real-time.
- **Auto-Stop 5s After Batch Completion**: When all active and queued extractions complete while the UI is closed, DirectCrate posts a persistent completion notification (`"DirectCrate Extraction Complete"`), waits 5 seconds to ensure no new jobs are initiated, and automatically stops the foreground service (`stopForeground(STOP_FOREGROUND_REMOVE)` and `stopSelf()`), releasing all locks and eliminating background battery drain.
- **Idle Auto-Stop (2.5s Grace Period)**: If the user dismisses `MainActivity` while DirectCrate is completely idle, the foreground service automatically shuts down after a 2.5-second grace period so no persistent notification icon lingers in the notification tray.
- **Instant Resume**: Launching DirectCrate from the home screen or tapping a completion notification immediately wakes or restarts `DirectCrateService` in `onResume()` and reconnects the WebView.

---

## Cross-Platform Web UI & Custom Theme-Matched Controls

The user interface (`web/index.html`) is a standalone, dependency-free web application embedded directly into the Go binary via `//go:embed web/index.html`.

### 1. Adaptive Studio Grid & Mobile Bottom Sheets

- **Desktop 2-Column Studio Grid (`>= 860px`)**:
  - **Left Column (Controls & Inputs)**: Archive URL, Extraction Mode selector, collapsible Advanced HTTP Headers (Bearer tokens, Cookies, referrers), Destination Directory picker, and primary action buttons (**Start Unpack**, **Add to Queue**, **Abort**).
  - **Right Column (Live Telemetry & Queue)**: Hero Progress & Speed card with large percentage readout, 2x2 Bento live metric tiles (`Streamed`, `Free Disk`, `App RAM`, `Device Temp`), Speed Cap selector, 60s Throughput History sparkline with peak & running average speed metrics, and active Queue panel.
  - **Centered Elevated Modals**: Desktop dialogs (Settings, History, Logs, Directory Picker) render as elevated, centered cards (`width: 640px; height: 72vh; max-height: 760px;`) with uniform dimensions across all views, blur backdrop, and `Escape` key dismissal.
- **Mobile Native App Layout (`< 860px`)**:
  - Streamlined single-column flow with responsive Bento metric cards.
  - Fixed, thumb-friendly bottom action bar (`.actions`) with `env(safe-area-inset-bottom)` padding for bezel-less devices.
  - Native-style slide-up Bottom Sheets with uniform height (`height: 78dvh; min-height: 78dvh; max-height: 78dvh;`), rounded 20px top corners, drag handles, and 2x2 multi-button grid footers preventing any action button clipping.

### 2. Live Telemetry, Speed Sparkline & Stream Health

- **60fps High-DPI Sparkline**: Real-time throughput graph rendered onto HTML5 canvas with `window.devicePixelRatio` scaling, displaying live transfer rate along with **Peak** and running **Average** speed calculations.
- **Stream Health Pill**: Live counter displaying estimated temporary storage space saved by zero-temp streaming (e.g. `Saved ~14.2 GB temp storage`), active prefetch streams (`4 streams`), and TCP socket reconnect retries.

### 3. Smart Thermal & Memory Safeguards

- **Contextual In-Line Action Banner**: Displays a non-intrusive warning banner inside the telemetry card if device thermals reach warm (`>= 42°C`) or hot (`>= 45°C`) states with a one-click button to switch to Balanced mode (15 MB/s).
- **Non-Blocking Smart Thermal Modal**: Theme-matched modal dialog triggered at most once per extraction session when temperatures reach hot thresholds, allowing users to throttle to 10 MB/s or continue without interrupting active streams.
- **Android Background Notifications**: Posts a background warning notification if handheld thermals heat up during background extractions.

### 4. GitHub Release Update Checker

- **Automatic Silent Check**: Queries the official GitHub Releases API silently on startup with a 5-second timeout and semantic version comparison against the current build.
- **Dismissible Header Banner**: Unobtrusively notifies users when a newer release is available with a direct "Update" link to GitHub Releases.
- **Manual Verification**: Accessible on demand inside Settings -> About tab with one-click "Check for Updates" and "GitHub Releases" actions.

### 5. Custom Theme-Matched Dropdowns & ARIA Accessibility

Replaces native OS `<select>` elements with accessible, custom-styled dropdowns matching the application's CSS design system:
- **Styled with CSS variables**: Uses `--bg`, `--surface`, `--surface-hover`, `--border`, `--border-focus`, `--text`, and `--accent`.
- **Keyboard Navigation**: Full support for `Enter`, `Space`, `ArrowUp`, `ArrowDown`, and `Escape`.
- **Accessibility**: Standard ARIA attributes (`role="dialog"`, `role="combobox"`, `role="listbox"`, `role="option"`, `aria-modal="true"`, `aria-labelledby`).
- **Outside-Click Dismissal**: Automatically closes when clicking anywhere outside the menu.

### 6. Dynamic System-Synced Theming

DirectCrate features a theme engine with live operating system synchronization:
- **`System (Default)`**: Listens to `window.matchMedia('(prefers-color-scheme: dark)')` and updates live when your OS switches between dark and light modes.
- **`Dark` Override**: Zinc/emerald palette (`#0B0D10` background, `#14181F` Bento surface, `#232B36` border, `#10B981` accent, `#FAFAFA` text).
- **`Light` Override**: Slate/emerald palette (`#F4F6F9` background, `#FFFFFF` Bento surface, `#E2E8F0` border, `#059669` accent, `#0F172A` text).
- **Live Android Bar Synchronization**: Whenever the active theme changes, the web UI calls `window.AndroidTheme?.onThemeChanged(isDark)` over JavaScript bridge to update Android status bars, navigation bars, and icon contrast (`WindowInsetsControllerCompat`) in real time.
- **Mobile-Tailored UI Scoping**: Automatically hides desktop-only power actions and sleep settings when running natively on Android, while keeping them visible when controlling a remote Linux/Windows desktop over LAN.

---

## Keyboard Shortcuts & UI Controls

| Shortcut / Action | Action |
| :--- | :--- |
| `Ctrl + Enter` / `Cmd + Enter` | Start extraction (when no modal is active) |
| `Escape` | Close the topmost modal or dialog |
| `Enter` | Submit password when prompted by an encrypted archive |
| `Ctrl + V` / `Cmd + V` | Paste a URL anywhere on the screen (auto-switches to multi-part mode if multiple lines are pasted) |
| **Drag & Drop** | Drop a single URL or a `.txt` file with one URL per line anywhere onto the window |
| **Speed Sparkline** | Real-time transfer speed graph with live throttle selector |

---

## Diagnostics & Crash Logging

DirectCrate provides built-in diagnostics accessible from the top navigation bar:

- **Trailing Log Viewer**: Click **Logs** in the header to view the last 200 log entries from `directcrate.log`.
- **One-Click Diagnostic Report**: Click **Copy Report** in the Logs modal to generate a pre-formatted Markdown summary containing version, OS, architecture, browser agent, and sanitized recent logs ready to paste into GitHub Issues.
- **Crash Recovery**: All HTTP handlers and background loops are wrapped in panic recovery handlers that write full stack traces to `crash.log`.

---

## Installation, Developer Guide & CLI

### Quick Start (Pre-Built Binaries)

For everyday users, standalone pre-compiled release binaries and Android APK packages are available directly on [GitHub Releases](https://github.com/musakpg/directcrate/releases):

- **Linux / SteamOS / Bazzite (x86_64 & ARM64)**: Download `directcrate-linux-amd64` (or `directcrate-linux-arm64`), make it executable (`chmod +x directcrate-linux-amd64`), and run `./directcrate-linux-amd64`. On first launch, DirectCrate automatically creates your application launcher shortcut and high-resolution icons.
- **Windows (x86_64)**: Download `directcrate-windows-amd64.exe` and double-click to launch. The official icon, Start Menu shortcut, and taskbar progress integrate automatically.
- **Android**: Download `DirectCrate.apk` and tap to install on Android 11 through 16. (Or run `directcrate-android-arm64` via Termux for headless/CLI streaming).

### Command-Line Flags

```text
Usage: directcrate [flags]

Flags:
  -port string
        HTTP server listen address or port (default ":9090")
  -no-open
        Disable automatic desktop browser launch on startup
  -version
        Print DirectCrate version and exit
```

### Automatic Port Fallback

If the default port `:9090` is already in use by another application, DirectCrate automatically scans and binds to the next available port up to `:9099`, printing the active address to `stdout`.

### Developer & Automated Testing Workflow

**Prerequisites**: Go 1.22+ installed with pure Go toolchain (`CGO_ENABLED=0`).

#### Running the Test Suites

DirectCrate features an automated two-layer test suite with atomic race detection and full coverage profiling:

```bash
# 1. Run backend unit and integration tests with race detection
make test

# 2. Run the complete automated test suite (unit + integration + automated headless Chromium E2E UI tests) with coverage
make test-all
```

The automated headless E2E test suite discovers any installed Chromium, Google Chrome, Brave, or Flatpak browser on your system, executing full browser workflows to verify:
- Responsive desktop (1280x800) and mobile (390x844) viewports with zero horizontal overflow
- Interactive URL input, multipart pasting, speed limit dropdowns, and batch queue reordering
- Remote archive inspection, Central Directory/7Z file trees, and selective range exclusion
- Modal dialogs, Escape key dismissal, and 100% WCAG ARIA accessibility compliance
- Zero unhandled JavaScript errors or CDP console exceptions

#### Building from Source

```bash
# Build native binary for your current OS with stripped symbols (-s -w)
make build

# Or build directly with go
go build -ldflags="-s -w" -o directcrate .
```

#### Cross-Compilation Targets

To generate icons, build Windows PE resources (`.syso`), and cross-compile all release targets into the `dist/` directory:

```bash
make build-all
```

Produces:
- `dist/directcrate-linux-amd64` (Linux Desktop, Steam Deck, Handhelds)
- `dist/directcrate-linux-arm64` (ARM64 Linux, Raspberry Pi)
- `dist/directcrate-windows-amd64.exe` (Windows 10/11 with embedded PE icon resource)
- `dist/directcrate-android-arm64` (Standalone CLI binary for Termux/Android)

#### Building the Android APK

Inside the `android/` directory:

```bash
# 1. Compile the 16KB-aligned shared library
CGO_ENABLED=1 GOOS=android GOARCH=arm64 \
  go build -buildmode=c-shared \
  -ldflags="-s -w -extldflags=-Wl,-z,max-page-size=16384" \
  -o android/app/src/main/jniLibs/arm64-v8a/libdirectcrate.so .

# 2. Build the Android release APK
cd android
./gradlew assembleRelease --no-daemon
```

---

## License

DirectCrate is open-source software licensed under the [MIT License](LICENSE).
