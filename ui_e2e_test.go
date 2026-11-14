package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// locateBrowserExec finds an existing Chromium, Chrome, Brave, or Edge executable,
// checking both host PATH and Flatpak installations, returning an executable path and a cleanup func.
func locateBrowserExec(t *testing.T) (string, func()) {
	t.Helper()
	// 1. Check native browser executables in PATH
	candidates := []string{
		"google-chrome",
		"google-chrome-stable",
		"chromium",
		"chromium-browser",
		"brave",
		"brave-browser",
		"microsoft-edge",
		"edge",
	}
	for _, c := range candidates {
		if path, err := exec.LookPath(c); err == nil {
			return path, func() {}
		}
	}

	// 2. Check Flatpak browser installations
	for _, appID := range []string{"com.brave.Browser", "com.google.Chrome", "org.chromium.Chromium"} {
		cmd := exec.Command("flatpak", "info", appID)
		if err := cmd.Run(); err == nil {
			tmpDir := t.TempDir()
			wrapper := filepath.Join(tmpDir, "browser_wrapper.sh")
			script := fmt.Sprintf("#!/bin/sh\nexec flatpak run %s \"$@\"\n", appID)
			if err := os.WriteFile(wrapper, []byte(script), 0755); err == nil {
				return wrapper, func() {
					_ = os.Remove(wrapper)
				}
			}
		}
	}

	return "", func() {}
}

func TestEndToEndUI(t *testing.T) {
	browserPath, cleanup := locateBrowserExec(t)
	defer cleanup()
	if browserPath == "" {
		t.Skip("No supported Chromium browser found; skipping headless UI E2E test")
	}

	// Ensure manager starts in clean idle state
	manager.mu.Lock()
	manager.state = ExtractionState{Status: "idle", Message: "Ready"}
	manager.queue = nil
	manager.mu.Unlock()
	defer func() {
		manager.mu.Lock()
		manager.state = ExtractionState{Status: "idle", Message: "Ready"}
		manager.queue = nil
		manager.mu.Unlock()
	}()

	// Create mock zip data for archive inspection tests
	mockFiles := map[string]string{
		"game/bin/game.exe":    "MZ_EXECUTABLE_HEADER_CONTENT",
		"game/data/sounds.bin": "AUDIO_SAMPLES_BYTES_12345",
		"game/readme.txt":      "DirectCrate Automated Test Archive",
	}
	zipData := createTestZipBytes(t, mockFiles)

	// Setup HTTP test server combining DirectCrate router with mock archive endpoints
	mainRouter := setupRouter()
	testMux := http.NewServeMux()
	testMux.HandleFunc("/mock-archive.zip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.Itoa(len(zipData)))
		if r.Method == http.MethodHead {
			return
		}
		http.ServeContent(w, r, "mock-archive.zip", time.Now(), bytes.NewReader(zipData))
	})
	testMux.HandleFunc("/mock-page.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<!DOCTYPE html><html><head><title>Mock Landing Page</title></head><body><h1>Redirecting...</h1></body></html>"))
	})
	testMux.Handle("/", mainRouter)

	srv := httptest.NewServer(testMux)
	defer srv.Close()

	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(browserPath),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
	)

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), allocOpts...)
	defer cancelAlloc()

	ctx, cancelCtx := chromedp.NewContext(allocCtx)
	defer cancelCtx()

	ctx, cancelTimeout := context.WithTimeout(ctx, 45*time.Second)
	defer cancelTimeout()

	// Track uncaught JavaScript exceptions and errors
	var jsErrors []string
	var jsMu sync.Mutex
	chromedp.Listen(ctx, func(ev any) {
		if exp, ok := ev.(*runtime.EventExceptionThrown); ok {
			jsMu.Lock()
			jsErrors = append(jsErrors, exp.ExceptionDetails.Text)
			jsMu.Unlock()
		}
	})

	// 1. Initial Page Load and Global Error Trap Injection
	t.Log("Navigating to DirectCrate UI at", srv.URL)
	initJS := `
		window.__e2eErrors = [];
		window.addEventListener('error', function(e) {
			window.__e2eErrors.push(e.message || String(e));
		});
		window.addEventListener('unhandledrejection', function(e) {
			window.__e2eErrors.push(e.reason ? (e.reason.message || String(e.reason)) : String(e));
		});
	`
	if err := chromedp.Do(ctx,
		chromedp.Navigate(srv.URL),
		chromedp.WaitVisible(chromedp.CSS("#urlSingle")),
		chromedp.Evaluate[chromedp.Void](initJS),
	); err != nil {
		t.Fatalf("failed initial page navigation: %v", err)
	}

	// =========================================================================
	// SUBTEST 1: DESKTOP VIEWPORT (1280x800)
	// =========================================================================
	t.Run("Desktop_Viewport_1280x800", func(t *testing.T) {
		if err := chromedp.Do(ctx,
			chromedp.EmulateViewport(1280, 800),
			chromedp.Sleep(100*time.Millisecond),
		); err != nil {
			t.Fatalf("failed to emulate desktop viewport: %v", err)
		}

		// A. Assert Zero Horizontal Overflow
		overflowCheckJS := `(document.documentElement.scrollWidth <= window.innerWidth + 1)`
		hasNoOverflow, err := chromedp.Run(ctx, chromedp.Evaluate[bool](overflowCheckJS))
		if err != nil || !hasNoOverflow {
			t.Fatalf("desktop viewport has horizontal overflow: scrollWidth > innerWidth")
		}

		// B. Single URL <-> Multi-Part switching
		modeSwitchJS := `
			(function() {
				const modeSelect = document.getElementById('urlMode');
				const singleRow = document.getElementById('singleUrlRow');
				const multiRow = document.getElementById('multiUrlRow');
				const singleInput = document.getElementById('urlSingle');
				const multiInput = document.getElementById('urlMulti');

				// Switch to multipart
				modeSelect.value = 'multipart';
				toggleUrlMode();
				const multiVisible = (multiRow.style.display !== 'none' && singleRow.style.display === 'none');

				// Input two URLs and check badge
				multiInput.value = "https://cdn.example.com/part1.rar\nhttps://cdn.example.com/part2.rar";
				updatePartBadge();
				const badgeText = document.getElementById('partBadge').textContent;

				// Switch back to single
				modeSelect.value = 'single';
				toggleUrlMode();
				const singleVisible = (singleRow.style.display !== 'none' && multiRow.style.display === 'none');

				return multiVisible && singleVisible && (badgeText.indexOf('2') !== -1);
			})()
		`
		modeSwitchOk, err := chromedp.Run(ctx, chromedp.Evaluate[bool](modeSwitchJS))
		if err != nil || !modeSwitchOk {
			t.Fatalf("URL mode switching test failed: %v", err)
		}

		// C. Clear button functionality
		clearBtnJS := `
			(function() {
				const singleInput = document.getElementById('urlSingle');
				singleInput.value = 'https://example.com/temporary.zip';
				const clearBtn = document.getElementById('clearBtn');
				clearBtn.click();
				return singleInput.value === '' && document.getElementById('partBadge').textContent === '';
			})()
		`
		clearOk, err := chromedp.Run(ctx, chromedp.Evaluate[bool](clearBtnJS))
		if err != nil || !clearOk {
			t.Fatalf("Clear button test failed: %v", err)
		}

		// D. Android Clipboard Paste Bridge & Notification Progress Bridge
		pasteBridgeJS := `
			(function() {
				window.AndroidBridge = {
					readClipboard: function() {
						return 'https://example.com/mock_clipboard_game.zip';
					},
					updateNotificationProgress: function(filename, percent, speedText, etaText, isActive) {
						window.__notifProgressCalls = window.__notifProgressCalls || [];
						window.__notifProgressCalls.push({ filename, percent, speedText, etaText, isActive });
					}
				};
				const pasteBtn = document.getElementById('pasteBtn');
				pasteBtn.click();
				return document.getElementById('urlSingle').value === 'https://example.com/mock_clipboard_game.zip';
			})()
		`
		pasteOk, err := chromedp.Run(ctx, chromedp.Evaluate[bool](pasteBridgeJS))
		if err != nil || !pasteOk {
			t.Fatalf("Android clipboard paste bridge test failed: %v", err)
		}

		// E. Inspect Valid Archive -> verify file tree rendering
		archiveURL := srv.URL + "/mock-archive.zip"
		inspectArchiveJS := fmt.Sprintf(`
			(function() {
				document.getElementById('urlSingle').value = %q;
				inspectURL();
			})()
		`, archiveURL)
		if err := chromedp.Do(ctx,
			chromedp.Evaluate[chromedp.Void](inspectArchiveJS),
			chromedp.WaitVisible(chromedp.CSS("#fileTreeBox.active")),
		); err != nil {
			t.Fatalf("failed waiting for #fileTreeBox.active after archive inspection: %v", err)
		}

		// Verify files appear in tree list and checkboxes are toggleable
		verifyTreeJS := `
			(function() {
				const items = document.querySelectorAll('.file-tree-row');
				if (items.length === 0) return false;
				const firstCheckbox = items[0].querySelector('.file-chk');
				if (!firstCheckbox) return false;
				const prevChecked = firstCheckbox.checked;
				firstCheckbox.click();
				return firstCheckbox.checked !== prevChecked;
			})()
		`
		treeOk, err := chromedp.Run(ctx, chromedp.Evaluate[bool](verifyTreeJS))
		if err != nil || !treeOk {
			t.Fatalf("File tree items or checkboxes verification failed: %v", err)
		}

		// F. Inspect HTML Webpage -> assert plain-language error translation
		htmlPageURL := srv.URL + "/mock-page.html"
		inspectHTMLJS := fmt.Sprintf(`
			(function() {
				document.getElementById('urlSingle').value = %q;
				inspectURL();
			})()
		`, htmlPageURL)
		if err := chromedp.Do(ctx,
			chromedp.Evaluate[chromedp.Void](inspectHTMLJS),
			chromedp.Sleep(400*time.Millisecond),
		); err != nil {
			t.Fatalf("failed triggering inspect on HTML page: %v", err)
		}

		verifyErrorMsgJS := `
			(function() {
				const statusText = document.getElementById('statusMsg') ? document.getElementById('statusMsg').textContent : '';
				const toastTexts = Array.from(document.querySelectorAll('.toast')).map(t => t.textContent).join(' ');
				const combined = (statusText + ' ' + toastTexts).toLowerCase();
				return combined.indexOf('webpage') !== -1 || combined.indexOf('html') !== -1;
			})()
		`
		errorMsgOk, err := chromedp.Run(ctx, chromedp.Evaluate[bool](verifyErrorMsgJS))
		if err != nil || !errorMsgOk {
			t.Fatalf("Plain-language HTML error banner verification failed: %v", err)
		}

		// G. Add to Queue and Remove from Queue
		queueTestJS := fmt.Sprintf(`
			(function() {
				document.getElementById('urlSingle').value = %q;
				document.getElementById('dest').value = '/tmp';
				addToQueue();
			})()
		`, archiveURL)
		if err := chromedp.Do(ctx,
			chromedp.Evaluate[chromedp.Void](queueTestJS),
			chromedp.WaitVisible(chromedp.CSS("#queuePanel.active")),
		); err != nil {
			t.Fatalf("failed waiting for #queuePanel.active: %v", err)
		}

		verifyQueueJS := `
			(function() {
				const countText = document.getElementById('queueCount').textContent;
				if (countText.indexOf('1 queued') === -1) return false;

				// Remove the item from queue
				const delBtn = document.querySelector('.queue-rm');
				if (!delBtn) return false;
				delBtn.click();
				return true;
			})()
		`
		queueItemOk, err := chromedp.Run(ctx, chromedp.Evaluate[bool](verifyQueueJS))
		if err != nil || !queueItemOk {
			t.Fatalf("Queue add/remove test failed: %v", err)
		}

		// Wait briefly for queue removal to update
		if err := chromedp.Do(ctx, chromedp.Sleep(200*time.Millisecond)); err != nil {
			t.Fatal(err)
		}

		// H. Speed Cap & Thermal Quick-Action Banner
		thermalBannerTestJS := `
			(function() {
				// Reset speed limit to Unlimited (0)
				const speedSel = document.getElementById('liveSpeedLimit');
				speedSel.value = '0';
				speedSel.dispatchEvent(new Event('change'));

				// Dispatch mock thermal warming telemetry
				applyStateUpdate({
					status: 'running',
					temp_c: 44.2,
					speed_mbs: 8.5,
					downloaded_mb: 25.0,
					total_mb: 200.0,
					percent: 12.5,
					eta: '00:25'
				});

				const banner = document.getElementById('thermalQuickBanner');
				const bannerVisible = banner && (banner.style.display === 'flex' || window.getComputedStyle(banner).display === 'flex');
				if (!bannerVisible) return false;

				// Click the banner action button: "Switch to Balanced 15 MB/s"
				const actionBtn = banner.querySelector('.btn-thermal-action');
				if (!actionBtn) return false;
				actionBtn.click();

				const newLimit = document.getElementById('liveSpeedLimit').value;
				const bannerHidden = banner.style.display === 'none';

				// Verify live notification progress bridge received the update
				const notifCalls = window.__notifProgressCalls || [];
				const lastNotif = notifCalls.length > 0 ? notifCalls[notifCalls.length - 1] : null;
				const notifOk = lastNotif && lastNotif.isActive === true && lastNotif.percent === 13;

				// Reset state to idle so any opened thermal alerts or state are cleaned up
				applyStateUpdate({ status: 'idle', temp_c: 35.0, message: 'Ready' });

				return (newLimit === '15') && bannerHidden && notifOk;
			})()
		`
		thermalBannerOk, err := chromedp.Run(ctx, chromedp.Evaluate[bool](thermalBannerTestJS))
		if err != nil || !thermalBannerOk {
			t.Fatalf("Thermal quick-action banner test failed: %v", err)
		}

		// I. Settings Modal (Preferences, FAQ, About tabs)
		settingsModalJS := `
			(function() {
				const settingsBtn = document.getElementById('settingsBtn');
				settingsBtn.click();
				const modal = document.getElementById('settingsModal');
				if (!modal.classList.contains('open')) return false;

				// Click FAQ tab
				const tabBtns = modal.querySelectorAll('.tab-btn');
				let faqBtn = null, aboutBtn = null, prefBtn = null;
				tabBtns.forEach(btn => {
					if (btn.textContent.indexOf('FAQ') !== -1) faqBtn = btn;
					if (btn.textContent.indexOf('About') !== -1) aboutBtn = btn;
					if (btn.textContent.indexOf('Preferences') !== -1) prefBtn = btn;
				});

				if (!faqBtn || !aboutBtn || !prefBtn) return false;

				faqBtn.click();
				if (!document.getElementById('tab-faq').classList.contains('active')) return false;

				aboutBtn.click();
				if (!document.getElementById('tab-about').classList.contains('active')) return false;

				prefBtn.click();
				if (!document.getElementById('tab-pref').classList.contains('active')) return false;

				// Close via Close button (✕)
				const closeBtn = modal.querySelector('.close-x');
				closeBtn.click();
				return !modal.classList.contains('open');
			})()
		`
		settingsOk, err := chromedp.Run(ctx, chromedp.Evaluate[bool](settingsModalJS))
		if err != nil || !settingsOk {
			t.Fatalf("Settings modal and tabs test failed: %v", err)
		}

		// J. History Modal
		historyModalJS := `
			(function() {
				const historyBtn = document.getElementById('historyBtn');
				historyBtn.click();
				const modal = document.getElementById('historyModal');
				if (!modal.classList.contains('open')) return false;

				const closeBtn = modal.querySelector('.close-x');
				closeBtn.click();
				return !modal.classList.contains('open');
			})()
		`
		historyOk, err := chromedp.Run(ctx, chromedp.Evaluate[bool](historyModalJS))
		if err != nil || !historyOk {
			t.Fatalf("History modal open/close test failed: %v", err)
		}

		// K. Logs Modal & Copy Report
		logsModalJS := `
			(function() {
				const logsBtn = document.getElementById('logsBtn');
				logsBtn.click();
				const modal = document.getElementById('logsModal');
				if (!modal.classList.contains('open')) return false;

				const copyBtn = document.getElementById('copyReportBtn');
				if (!copyBtn) return false;
				copyBtn.click();

				const closeBtn = modal.querySelector('.close-x');
				closeBtn.click();
				return !modal.classList.contains('open');
			})()
		`
		logsOk, err := chromedp.Run(ctx, chromedp.Evaluate[bool](logsModalJS))
		if err != nil || !logsOk {
			t.Fatalf("Logs modal and copy report test failed: %v", err)
		}

		// L. Directory Picker Modal & Escape Key dismiss
		pickerModalJS := `
			(function() {
				const browseBtn = document.getElementById('browseDestBtn');
				if (!browseBtn) return false;
				browseBtn.click();
				const modal = document.getElementById('pickerModal');
				if (!modal || !modal.classList.contains('open')) return false;

				// Simulate Escape keydown
				document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
				return !modal.classList.contains('open');
			})()
		`
		pickerOk, err := chromedp.Run(ctx, chromedp.Evaluate[bool](pickerModalJS))
		if err != nil || !pickerOk {
			t.Fatalf("Directory picker modal open & Escape dismiss test failed: %v", err)
		}

		// M. Accessibility & ARIA compliance verification
		ariaCheckJS := `
			(function() {
				const dialogs = Array.from(document.querySelectorAll('.overlay > .dialog'));
				if (dialogs.length === 0) return false;
				for (const d of dialogs) {
					if (d.getAttribute('role') !== 'dialog') return false;
					if (d.getAttribute('aria-modal') !== 'true') return false;
					if (!d.getAttribute('aria-labelledby')) return false;
				}
				const closeBtns = Array.from(document.querySelectorAll('.close-x'));
				for (const btn of closeBtns) {
					if (!btn.getAttribute('aria-label')) return false;
				}
				return true;
			})()
		`
		ariaOk, err := chromedp.Run(ctx, chromedp.Evaluate[bool](ariaCheckJS))
		if err != nil || !ariaOk {
			t.Fatalf("Aria accessibility validation failed: dialogs must have role='dialog', aria-modal='true', aria-labelledby, and close-x must have aria-label")
		}

		// N. About Tab Update Checker
		updateCheckJS := `
			(function() {
				const settingsBtn = document.getElementById('settingsBtn');
				settingsBtn.click();
				const aboutTabBtn = Array.from(document.querySelectorAll('.tabs .tab-btn')).find(b => b.textContent.includes('About'));
				if (!aboutTabBtn) return false;
				aboutTabBtn.click();

				const updateBtn = document.getElementById('manualCheckUpdateBtn');
				const releaseBtn = document.getElementById('openReleaseBtn');
				const updateStatus = document.getElementById('manualUpdateStatus');
				if (!updateBtn || !releaseBtn || !updateStatus) return false;

				// Click check for updates and verify it triggers without exception
				updateBtn.click();

				// Close settings modal
				closeSettings();
				return true;
			})()
		`
		updateOk, err := chromedp.Run(ctx, chromedp.Evaluate[bool](updateCheckJS))
		if err != nil || !updateOk {
			t.Fatalf("About tab update checker verification failed: %v", err)
		}

		// O. Non-Blocking Thermal & Memory Alert Modal
		thermalModalJS := `
			(function() {
				const modal = document.getElementById('thermalAlertModal');
				if (!modal) return false;

				modal.classList.add('open');
				if (!modal.classList.contains('open')) return false;

				dismissThermalAlertModal(true);
				return !modal.classList.contains('open');
			})()
		`
		thermalOk, err := chromedp.Run(ctx, chromedp.Evaluate[bool](thermalModalJS))
		if err != nil || !thermalOk {
			t.Fatalf("Thermal alert modal test failed: %v", err)
		}

		// P. Update Banner
		updateBannerJS := `
			(function() {
				const banner = document.getElementById('updateBanner');
				if (!banner) return false;
				banner.style.display = 'flex';
				dismissUpdateBanner();
				return banner.style.display === 'none';
			})()
		`
		bannerOk, err := chromedp.Run(ctx, chromedp.Evaluate[bool](updateBannerJS))
		if err != nil || !bannerOk {
			t.Fatalf("Update banner test failed: %v", err)
		}

		// Q. Hero Progress & Telemetry Readouts
		heroElementsJS := `
			(function() {
				const pct = document.getElementById('percentVal');
				const avgSpeed = document.getElementById('avgSpeedLabel');
				const healthPill = document.getElementById('streamHealthPill');
				return Boolean(pct && avgSpeed && healthPill);
			})()
		`
		heroOk, err := chromedp.Run(ctx, chromedp.Evaluate[bool](heroElementsJS))
		if err != nil || !heroOk {
			t.Fatalf("Hero telemetry elements (percentVal, avgSpeedLabel, streamHealthPill) check failed: %v", err)
		}

		// R. Desktop Modal Geometric Bounds & Height Consistency Assertion (1280x800)
		modalGeometryCheckJS := `
			(function() {
				const modals = [
					{ id: 'settingsModal', open: () => document.getElementById('settingsBtn').click(), close: () => closeSettings() },
					{ id: 'historyModal', open: () => document.getElementById('historyBtn').click(), close: () => closeHistory() },
					{ id: 'logsModal', open: () => document.getElementById('logsBtn').click(), close: () => closeLogsModal() },
					{ id: 'pickerModal', open: () => document.getElementById('browseDestBtn').click(), close: () => closePicker() }
				];

				const heights = [];
				const vpWidth = window.innerWidth;

				for (const m of modals) {
					m.open();
					const overlay = document.getElementById(m.id);
					if (!overlay || !overlay.classList.contains('open')) {
						return JSON.stringify({ ok: false, error: m.id + ' failed to open' });
					}

					const dialog = overlay.querySelector('.modal, .dialog');
					if (!dialog) {
						return JSON.stringify({ ok: false, error: m.id + ' dialog/modal element missing' });
					}

					const dRect = dialog.getBoundingClientRect();
					heights.push({ id: m.id, height: dRect.height });

					// Assert all buttons in footer lie strictly within [0, window.innerWidth]
					const buttons = dialog.querySelectorAll('.dialog-foot button');
					for (const btn of buttons) {
						if (btn.offsetParent === null && window.getComputedStyle(btn).display === 'none') continue;
						const bRect = btn.getBoundingClientRect();
						if (bRect.left < -0.5 || bRect.right > vpWidth + 0.5) {
							return JSON.stringify({
								ok: false,
								error: "Button '" + btn.textContent.trim() + "' in " + m.id + " footer clipped horizontally: left=" + bRect.left.toFixed(1) + ", right=" + bRect.right.toFixed(1) + ", innerWidth=" + vpWidth
							});
						}
						if (bRect.width <= 0 || bRect.height <= 0) {
							return JSON.stringify({
								ok: false,
								error: "Button '" + btn.textContent.trim() + "' in " + m.id + " has invalid dimensions: " + bRect.width + "x" + bRect.height
							});
						}
					}

					m.close();
					if (overlay.classList.contains('open')) {
						return JSON.stringify({ ok: false, error: m.id + ' failed to close' });
					}
				}

				const hValues = heights.map(h => h.height);
				const minH = Math.min(...hValues);
				const maxH = Math.max(...hValues);
				const diff = maxH - minH;
				if (diff > 2.0) {
					const details = heights.map(h => h.id + "=" + h.height.toFixed(1) + "px").join(', ');
					return JSON.stringify({
						ok: false,
						error: "Desktop modal height discrepancy " + diff.toFixed(2) + "px exceeds 2px tolerance: " + details
					});
				}

				return JSON.stringify({ ok: true, heights: heights });
			})()
		`
		var desktopModalResStr string
		desktopModalResStr, err = chromedp.Run(ctx, chromedp.Evaluate[string](modalGeometryCheckJS))
		if err != nil {
			t.Fatalf("Desktop modal geometry evaluation failed: %v", err)
		}
		var desktopModalRes struct {
			OK      bool   `json:"ok"`
			Error   string `json:"error"`
			Heights []struct {
				ID     string  `json:"id"`
				Height float64 `json:"height"`
			} `json:"heights"`
		}
		if err := json.Unmarshal([]byte(desktopModalResStr), &desktopModalRes); err != nil {
			t.Fatalf("Failed unmarshaling desktop modal geometry result: %v (raw=%s)", err, desktopModalResStr)
		}
		if !desktopModalRes.OK {
			t.Fatalf("Desktop modal geometry assertion failed: %s", desktopModalRes.Error)
		}
		t.Logf("Desktop primary modals verified consistent (heights: %v)", desktopModalRes.Heights)
	})

	// =========================================================================
	// SUBTEST 2: MOBILE VIEWPORT (390x844 - iPhone / Modern Android Phone)
	// =========================================================================
	t.Run("Mobile_Viewport_390x844", func(t *testing.T) {
		if err := chromedp.Do(ctx,
			chromedp.EmulateViewport(390, 844, chromedp.EmulateMobile, chromedp.EmulateTouch),
			chromedp.Sleep(100*time.Millisecond),
		); err != nil {
			t.Fatalf("failed to emulate mobile viewport: %v", err)
		}

		// A. Assert Zero Horizontal Overflow on Mobile Phone Form Factor
		overflowCheckJS := `(document.documentElement.scrollWidth <= window.innerWidth + 1)`
		hasNoMobileOverflow, err := chromedp.Run(ctx, chromedp.Evaluate[bool](overflowCheckJS))
		if err != nil || !hasNoMobileOverflow {
			t.Fatalf("mobile viewport (390x844) has horizontal overflow: scrollWidth > innerWidth")
		}

		// B. Test Android Hardware Back Button (window.handleAndroidBack)
		androidBackModalJS := `
			(function() {
				if (typeof window.handleAndroidBack !== 'function') return false;

				// 1. Open Settings modal and close via handleAndroidBack
				const settingsBtn = document.getElementById('settingsBtn');
				settingsBtn.click();
				const settingsModal = document.getElementById('settingsModal');
				if (!settingsModal.classList.contains('open')) return false;

				const handled1 = window.handleAndroidBack();
				if (!handled1 || settingsModal.classList.contains('open')) return false;

				// 2. Open Directory Picker modal and close via handleAndroidBack
				const browseBtn = document.getElementById('browseDestBtn');
				browseBtn.click();
				const pickerModal = document.getElementById('pickerModal');
				if (!pickerModal.classList.contains('open')) return false;

				const handled2 = window.handleAndroidBack();
				if (!handled2 || pickerModal.classList.contains('open')) return false;

				return true;
			})()
		`
		androidBackOk, err := chromedp.Run(ctx, chromedp.Evaluate[bool](androidBackModalJS))
		if err != nil || !androidBackOk {
			t.Fatalf("Mobile window.handleAndroidBack() modal dismissal test failed: %v", err)
		}

		// C. Mobile Modal Geometric Bounds & Height Consistency Assertion (390x844)
		mobileModalGeometryCheckJS := `
			(function() {
				const modals = [
					{ id: 'settingsModal', open: () => document.getElementById('settingsBtn').click(), close: () => closeSettings() },
					{ id: 'historyModal', open: () => document.getElementById('historyBtn').click(), close: () => closeHistory() },
					{ id: 'logsModal', open: () => document.getElementById('logsBtn').click(), close: () => closeLogsModal() },
					{ id: 'pickerModal', open: () => document.getElementById('browseDestBtn').click(), close: () => closePicker() }
				];

				const heights = [];
				const vpWidth = window.innerWidth;

				for (const m of modals) {
					m.open();
					const overlay = document.getElementById(m.id);
					if (!overlay || !overlay.classList.contains('open')) {
						return JSON.stringify({ ok: false, error: m.id + ' failed to open on mobile' });
					}

					const dialog = overlay.querySelector('.modal, .dialog');
					if (!dialog) {
						return JSON.stringify({ ok: false, error: m.id + ' dialog/modal element missing on mobile' });
					}

					const dRect = dialog.getBoundingClientRect();
					heights.push({ id: m.id, height: dRect.height });

					// Assert all buttons in footer lie strictly within [0, window.innerWidth] (zero clipped buttons)
					const buttons = dialog.querySelectorAll('.dialog-foot button');
					for (const btn of buttons) {
						if (btn.offsetParent === null && window.getComputedStyle(btn).display === 'none') continue;
						const bRect = btn.getBoundingClientRect();
						if (bRect.left < -0.5 || bRect.right > vpWidth + 0.5) {
							return JSON.stringify({
								ok: false,
								error: "Button '" + btn.textContent.trim() + "' in " + m.id + " footer clipped horizontally on mobile: left=" + bRect.left.toFixed(1) + ", right=" + bRect.right.toFixed(1) + ", innerWidth=" + vpWidth
							});
						}
						if (bRect.width <= 0 || bRect.height <= 0) {
							return JSON.stringify({
								ok: false,
								error: "Button '" + btn.textContent.trim() + "' in " + m.id + " has invalid dimensions: " + bRect.width + "x" + bRect.height
							});
						}
					}

					m.close();
					if (overlay.classList.contains('open')) {
						return JSON.stringify({ ok: false, error: m.id + ' failed to close on mobile' });
					}
				}

				const hValues = heights.map(h => h.height);
				const minH = Math.min(...hValues);
				const maxH = Math.max(...hValues);
				const diff = maxH - minH;
				if (diff > 2.0) {
					const details = heights.map(h => h.id + "=" + h.height.toFixed(1) + "px").join(', ');
					return JSON.stringify({
						ok: false,
						error: "Mobile modal height discrepancy " + diff.toFixed(2) + "px exceeds 2px tolerance: " + details
					});
				}

				return JSON.stringify({ ok: true, heights: heights });
			})()
		`
		var mobileModalResStr string
		mobileModalResStr, err = chromedp.Run(ctx, chromedp.Evaluate[string](mobileModalGeometryCheckJS))
		if err != nil {
			t.Fatalf("Mobile modal geometry evaluation failed: %v", err)
		}
		var mobileModalRes struct {
			OK      bool   `json:"ok"`
			Error   string `json:"error"`
			Heights []struct {
				ID     string  `json:"id"`
				Height float64 `json:"height"`
			} `json:"heights"`
		}
		if err := json.Unmarshal([]byte(mobileModalResStr), &mobileModalRes); err != nil {
			t.Fatalf("Failed unmarshaling mobile modal geometry result: %v (raw=%s)", err, mobileModalResStr)
		}
		if !mobileModalRes.OK {
			t.Fatalf("Mobile modal geometry assertion failed: %s", mobileModalRes.Error)
		}
		t.Logf("Mobile primary modals verified consistent (heights: %v)", mobileModalRes.Heights)
	})

	// =========================================================================
	// ZERO JAVASCRIPT CONSOLE ERROR ASSERTION
	// =========================================================================
	t.Run("Zero_JavaScript_Errors", func(t *testing.T) {
		// Check in-page trapped errors
		pageErrorsJS := `(window.__e2eErrors || [])`
		pageErrors, err := chromedp.Run(ctx, chromedp.Evaluate[[]string](pageErrorsJS))
		if err != nil {
			t.Fatalf("failed to retrieve trapped in-page JS errors: %v", err)
		}

		if len(pageErrors) > 0 {
			t.Fatalf("found %d unhandled JavaScript error(s) during E2E UI run: %#v", len(pageErrors), pageErrors)
		}

		// Check CDP-level exceptions
		jsMu.Lock()
		cdpErrors := make([]string, len(jsErrors))
		copy(cdpErrors, jsErrors)
		jsMu.Unlock()

		if len(cdpErrors) > 0 {
			t.Fatalf("found %d CDP runtime exception(s) during E2E UI run: %#v", len(cdpErrors), cdpErrors)
		}
		t.Log("E2E UI test completed with zero JavaScript errors across all viewports and actions!")
	})
}
