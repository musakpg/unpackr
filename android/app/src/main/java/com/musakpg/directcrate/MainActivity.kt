package com.musakpg.directcrate

import android.Manifest
import android.annotation.SuppressLint
import android.app.DownloadManager
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Intent
import android.content.IntentFilter
import android.content.pm.PackageManager
import android.content.res.Configuration
import android.graphics.Color
import android.net.Uri
import android.os.BatteryManager
import android.os.Build
import android.os.Bundle
import android.os.Environment
import android.os.Handler
import android.os.Looper
import android.os.PowerManager
import android.os.storage.StorageManager
import android.provider.DocumentsContract
import android.provider.Settings
import android.view.ViewGroup
import android.webkit.JavascriptInterface
import android.webkit.WebChromeClient
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.FrameLayout
import androidx.activity.OnBackPressedCallback
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity
import androidx.core.content.ContextCompat
import androidx.core.view.WindowInsetsControllerCompat
import org.json.JSONObject
import java.io.File
import java.io.PrintWriter
import java.io.StringWriter
import java.net.HttpURLConnection
import java.net.URL
import kotlin.concurrent.thread

class MainActivity : AppCompatActivity() {

    private lateinit var rootLayout: FrameLayout
    private lateinit var webView: WebView
    private val handler = Handler(Looper.getMainLooper())
    private var isPageLoaded = false
    private var isCurrentlyDark = true

    private val requestNotificationPermissionLauncher =
        registerForActivityResult(ActivityResultContracts.RequestPermission()) { _ ->
            // Notification permission handled
        }

    private val requestStoragePermissionLauncher =
        registerForActivityResult(ActivityResultContracts.RequestMultiplePermissions()) { _ ->
            // Storage permission handled
        }

    private val pickDirectoryLauncher =
        registerForActivityResult(ActivityResultContracts.OpenDocumentTree()) { uri ->
            if (uri != null) {
                val resolvedPath = resolveDocTreeUri(uri)
                handler.post {
                    val script = "if(window.onAndroidDirectoryPicked){window.onAndroidDirectoryPicked(${JSONObject.quote(resolvedPath)});}else{const d=document.getElementById('dest');if(d)d.value=${JSONObject.quote(resolvedPath)};}"
                    webView.evaluateJavascript(script, null)
                }
            }
        }

    private fun resolveDocTreeUri(uri: Uri): String {
        try {
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.LOLLIPOP) {
                val docId = DocumentsContract.getTreeDocumentId(uri)
                val parts = docId.split(":")
                if (parts.size >= 2) {
                    val type = parts[0]
                    val subPath = parts[1]
                    if (type.equals("primary", ignoreCase = true)) {
                        val root = Environment.getExternalStorageDirectory().absolutePath
                        return if (subPath.isNotEmpty()) "$root/$subPath" else root
                    } else {
                        return "/storage/$type/$subPath"
                    }
                }
            }
        } catch (e: Exception) {
            e.printStackTrace()
        }
        return uri.path ?: uri.toString()
    }

    /**
     * Comprehensive JavaScript interface exposed to WebView as window.AndroidBridge.
     */
    inner class AndroidBridgeInterface {
        @JavascriptInterface
        fun readClipboard(): String {
            var result = ""
            val latch = java.util.concurrent.CountDownLatch(1)
            runOnUiThread {
                try {
                    val clipboard = getSystemService(CLIPBOARD_SERVICE) as? ClipboardManager
                    if (clipboard != null && clipboard.hasPrimaryClip()) {
                        val clip = clipboard.primaryClip
                        if (clip != null && clip.itemCount > 0) {
                            result = clip.getItemAt(0)?.coerceToText(this@MainActivity)?.toString() ?: ""
                        }
                    }
                } catch (e: Exception) {
                    e.printStackTrace()
                } finally {
                    latch.countDown()
                }
            }
            try {
                latch.await(500, java.util.concurrent.TimeUnit.MILLISECONDS)
            } catch (ignored: Exception) {}
            return result
        }

        @JavascriptInterface
        fun writeClipboard(text: String): Boolean {
            var result = false
            val latch = java.util.concurrent.CountDownLatch(1)
            runOnUiThread {
                try {
                    val clipboard = getSystemService(CLIPBOARD_SERVICE) as? ClipboardManager
                    if (clipboard != null) {
                        val clip = ClipData.newPlainText("DirectCrate", text)
                        clipboard.setPrimaryClip(clip)
                        result = true
                    }
                } catch (e: Exception) {
                    e.printStackTrace()
                } finally {
                    latch.countDown()
                }
            }
            try {
                latch.await(500, java.util.concurrent.TimeUnit.MILLISECONDS)
            } catch (ignored: Exception) {}
            return result
        }

        @JavascriptInterface
        fun openFolder(path: String): Boolean {
            var launched = false
            val latch = java.util.concurrent.CountDownLatch(1)
            handler.post {
                try {
                    if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
                        try {
                            val sm = getSystemService(STORAGE_SERVICE) as? StorageManager
                            val intent = sm?.primaryStorageVolume?.createOpenDocumentTreeIntent()
                            if (intent != null) {
                                intent.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
                                startActivity(intent)
                                launched = true
                            }
                        } catch (ignored: Exception) {}
                    }
                    if (!launched) {
                        try {
                            val dlIntent = Intent(DownloadManager.ACTION_VIEW_DOWNLOADS).apply {
                                flags = Intent.FLAG_ACTIVITY_NEW_TASK
                            }
                            startActivity(dlIntent)
                            launched = true
                        } catch (ignored: Exception) {}
                    }
                    if (!launched) {
                        try {
                            val filesIntent = Intent(Intent.ACTION_VIEW).apply {
                                setDataAndType(Uri.parse("content://media/external/file"), "vnd.android.cursor.dir/*")
                                flags = Intent.FLAG_ACTIVITY_NEW_TASK
                            }
                            startActivity(filesIntent)
                            launched = true
                        } catch (ignored: Exception) {}
                    }
                } catch (e: Exception) {
                    e.printStackTrace()
                } finally {
                    latch.countDown()
                }
            }
            try {
                latch.await(500, java.util.concurrent.TimeUnit.MILLISECONDS)
            } catch (ignored: Exception) {}
            return launched
        }

        @JavascriptInterface
        fun pickDirectory() {
            handler.post {
                try {
                    pickDirectoryLauncher.launch(null)
                } catch (e: Exception) {
                    e.printStackTrace()
                }
            }
        }

        @JavascriptInterface
        fun getDeviceThermals(): String {
            var tempC = 0.0
            var thermalStatus = "normal"
            try {
                val batteryIntent = registerReceiver(null, IntentFilter(Intent.ACTION_BATTERY_CHANGED))
                val rawTemp = batteryIntent?.getIntExtra(BatteryManager.EXTRA_TEMPERATURE, 0) ?: 0
                if (rawTemp > 0) {
                    tempC = rawTemp / 10.0
                }
            } catch (ignored: Exception) {}

            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
                try {
                    val powerManager = getSystemService(POWER_SERVICE) as? PowerManager
                    when (powerManager?.currentThermalStatus) {
                        PowerManager.THERMAL_STATUS_NONE,
                        PowerManager.THERMAL_STATUS_LIGHT -> thermalStatus = "normal"
                        PowerManager.THERMAL_STATUS_MODERATE -> thermalStatus = "warm"
                        PowerManager.THERMAL_STATUS_SEVERE,
                        PowerManager.THERMAL_STATUS_CRITICAL,
                        PowerManager.THERMAL_STATUS_EMERGENCY,
                        PowerManager.THERMAL_STATUS_SHUTDOWN -> thermalStatus = "hot"
                    }
                } catch (ignored: Exception) {}
            } else {
                if (tempC >= 45.0) {
                    thermalStatus = "hot"
                } else if (tempC >= 41.0) {
                    thermalStatus = "warm"
                } else {
                    thermalStatus = "normal"
                }
            }

            return JSONObject().apply {
                put("temp_c", tempC)
                put("thermal_status", thermalStatus)
            }.toString()
        }

        @JavascriptInterface
        fun hasStoragePermission(): Boolean {
            return if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                Environment.isExternalStorageManager()
            } else {
                ContextCompat.checkSelfPermission(this@MainActivity, Manifest.permission.WRITE_EXTERNAL_STORAGE) == PackageManager.PERMISSION_GRANTED
            }
        }

        @JavascriptInterface
        fun requestStoragePermission() {
            handler.post {
                launchStoragePermissionFlow()
            }
        }

        @JavascriptInterface
        fun updateNotificationProgress(
            filename: String,
            percent: Int,
            speedText: String,
            etaText: String,
            isActive: Boolean
        ) {
            handler.post {
                try {
                    DirectCrateService.getInstance()?.updateNotificationProgress(
                        filename = filename,
                        percent = percent,
                        speedText = speedText,
                        etaText = etaText,
                        isActive = isActive
                    )
                } catch (e: Exception) {
                    e.printStackTrace()
                }
            }
        }

        @JavascriptInterface
        fun openExternalUrl(url: String): Boolean {
            if (!url.startsWith("http://", ignoreCase = true) && !url.startsWith("https://", ignoreCase = true)) {
                return false
            }
            return try {
                val intent = Intent(Intent.ACTION_VIEW, Uri.parse(url)).apply {
                    addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
                }
                startActivity(intent)
                true
            } catch (e: Exception) {
                e.printStackTrace()
                false
            }
        }

        @JavascriptInterface
        fun notifyThermalWarning(tempC: Double, message: String) {
            handler.post {
                try {
                    DirectCrateService.getInstance()?.showThermalWarningNotification(tempC, message)
                } catch (e: Exception) {
                    e.printStackTrace()
                }
            }
        }

        @JavascriptInterface
        fun onThemeChanged(isDark: Boolean) {
            handler.post {
                applySystemBarsTheme(isDark)
            }
        }

        @JavascriptInterface
        fun retryConnection() {
            handler.post {
                initializeBackendAndLoad()
            }
        }
    }

    /**
     * Legacy JavaScript interface exposed to WebView as window.AndroidTheme.
     */
    inner class AndroidThemeInterface {
        @JavascriptInterface
        fun openExternalUrl(url: String): Boolean {
            if (!url.startsWith("http://", ignoreCase = true) && !url.startsWith("https://", ignoreCase = true)) {
                return false
            }
            return try {
                val intent = Intent(Intent.ACTION_VIEW, Uri.parse(url)).apply {
                    addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
                }
                startActivity(intent)
                true
            } catch (e: Exception) {
                e.printStackTrace()
                false
            }
        }

        @JavascriptInterface
        fun notifyThermalWarning(tempC: Double, message: String) {
            handler.post {
                try {
                    DirectCrateService.getInstance()?.showThermalWarningNotification(tempC, message)
                } catch (e: Exception) {
                    e.printStackTrace()
                }
            }
        }

        @JavascriptInterface
        fun onThemeChanged(isDark: Boolean) {
            handler.post {
                applySystemBarsTheme(isDark)
            }
        }

        @JavascriptInterface
        fun retryConnection() {
            handler.post {
                initializeBackendAndLoad()
            }
        }
    }

    /**
     * Dynamically updates status bar, navigation bar, and window insets controller
     * appearance based on whether dark or light mode is active.
     */
    fun applySystemBarsTheme(isDark: Boolean) {
        if (isFinishing || isDestroyed) return
        isCurrentlyDark = isDark
        val bgColor = if (isDark) Color.parseColor("#09090B") else Color.parseColor("#F8FAFC")

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.LOLLIPOP) {
            window.statusBarColor = bgColor
            window.navigationBarColor = bgColor
        }

        val insetsController = WindowInsetsControllerCompat(window, window.decorView)
        insetsController.isAppearanceLightStatusBars = !isDark
        insetsController.isAppearanceLightNavigationBars = !isDark

        if (::rootLayout.isInitialized) {
            rootLayout.setBackgroundColor(bgColor)
        }
        if (::webView.isInitialized) {
            webView.setBackgroundColor(bgColor)
        }
    }

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        DirectCrateService.isUiVisible = true
        DirectCrateService.cancelIdleStop()

        // Determine initial system dark/light preference before WebView finishes loading
        val currentNightMode = resources.configuration.uiMode and Configuration.UI_MODE_NIGHT_MASK
        val isInitialDark = currentNightMode == Configuration.UI_MODE_NIGHT_YES
        isCurrentlyDark = isInitialDark
        val initialBgColor = if (isInitialDark) Color.parseColor("#09090B") else Color.parseColor("#F8FAFC")

        // Initialize root container and WebView
        rootLayout = FrameLayout(this).apply {
            setBackgroundColor(initialBgColor)
            layoutParams = ViewGroup.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT,
                ViewGroup.LayoutParams.MATCH_PARENT
            )
        }

        webView = WebView(this).apply {
            setBackgroundColor(initialBgColor)
            layoutParams = ViewGroup.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT,
                ViewGroup.LayoutParams.MATCH_PARENT
            )
            settings.apply {
                javaScriptEnabled = true
                domStorageEnabled = true
                databaseEnabled = true
                useWideViewPort = true
                loadWithOverviewMode = true
                cacheMode = WebSettings.LOAD_DEFAULT
            }

            // Bind JavaScript interfaces for bridge and dynamic theme syncing
            addJavascriptInterface(AndroidBridgeInterface(), "AndroidBridge")
            addJavascriptInterface(AndroidThemeInterface(), "AndroidTheme")

            webViewClient = object : WebViewClient() {
                override fun onPageFinished(view: WebView?, url: String?) {
                    super.onPageFinished(view, url)
                    isPageLoaded = true
                }
            }
            webChromeClient = WebChromeClient()
        }

        rootLayout.addView(webView)
        setContentView(rootLayout)

        // Apply system bar styling for initial appearance
        applySystemBarsTheme(isInitialDark)

        // Handle Back button navigation
        onBackPressedDispatcher.addCallback(this, object : OnBackPressedCallback(true) {
            override fun handleOnBackPressed() {
                webView.evaluateJavascript("(function(){ if (window.handleAndroidBack && window.handleAndroidBack()) { return true; } return false; })()") { result ->
                    val handledInWeb = result == "true" || result == "\"true\""
                    if (!handledInWeb) {
                        if (DirectCrateService.isServerRunning) {
                            moveTaskToBack(true)
                        } else {
                            if (webView.canGoBack()) {
                                webView.goBack()
                            } else {
                                finish()
                            }
                        }
                    }
                }
            }
        })

        checkNotificationPermission()
        initializeBackendAndLoad()
    }

    private fun checkNotificationPermission() {
        // Notification permission for Android 13+
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            if (ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS)
                != PackageManager.PERMISSION_GRANTED) {
                requestNotificationPermissionLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
            }
        }
    }

    private fun launchStoragePermissionFlow() {
        // All Files Access for Android 11+
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
            try {
                val intent = Intent(Settings.ACTION_MANAGE_APP_ALL_FILES_ACCESS_PERMISSION).apply {
                    data = Uri.parse("package:$packageName")
                }
                startActivity(intent)
            } catch (e: Exception) {
                val intent = Intent(Settings.ACTION_MANAGE_ALL_FILES_ACCESS_PERMISSION)
                startActivity(intent)
            }
        } else {
            // Android 10 and below
            val perms = arrayOf(
                Manifest.permission.READ_EXTERNAL_STORAGE,
                Manifest.permission.WRITE_EXTERNAL_STORAGE
            )
            requestStoragePermissionLauncher.launch(perms)
        }
    }

    /**
     * Helper to retrieve app config and default extraction paths.
     */
    private fun getStorageDirs(): Pair<String, String> {
        val configDir = File(filesDir, "directcrate_config").apply { mkdirs() }.absolutePath
        val defaultDest = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R && Environment.isExternalStorageManager()) {
            val publicDownloads = Environment.getExternalStoragePublicDirectory(Environment.DIRECTORY_DOWNLOADS)
            File(publicDownloads, "DirectCrate").apply { mkdirs() }.absolutePath
        } else if (Build.VERSION.SDK_INT < Build.VERSION_CODES.R) {
            val publicDownloads = Environment.getExternalStoragePublicDirectory(Environment.DIRECTORY_DOWNLOADS)
            File(publicDownloads, "DirectCrate").apply { mkdirs() }.absolutePath
        } else {
            getExternalFilesDir(Environment.DIRECTORY_DOWNLOADS)?.apply { mkdirs() }?.absolutePath
                ?: File(filesDir, "downloads").apply { mkdirs() }.absolutePath
        }
        return Pair(configDir, defaultDest)
    }

    private fun initializeBackendAndLoad() {
        thread(name = "DirectCrateInit", isDaemon = true) {
            try {
                // Ensure native libdirectcrate.so loads cleanly on a background thread
                DirectCrateBridge.loadNativeLibrary()

                // Start Go native HTTP server directly on background thread
                val (configDir, defaultDest) = getStorageDirs()
                val boundPort = DirectCrateBridge.startServer(configDir, defaultDest)
                if (boundPort > 0) {
                    DirectCrateService.activeServerPort = boundPort
                    DirectCrateService.activeServerUrl = "http://127.0.0.1:$boundPort"
                    DirectCrateService.isServerRunning = true
                }

                // Start the background foreground service safely (catching any ForegroundServiceStartNotAllowedException)
                try {
                    startDirectCrateService()
                } catch (serviceErr: Throwable) {
                    serviceErr.printStackTrace()
                }

                // Poll HTTP server and load UI
                pollAndLoadServer(DirectCrateService.activeServerPort)
            } catch (t: Throwable) {
                renderErrorPage("Failed to initialize DirectCrate engine", t, canRetry = true)
            }
        }
    }

    private fun startDirectCrateService() {
        try {
            val serviceIntent = Intent(this, DirectCrateService::class.java)
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                startForegroundService(serviceIntent)
            } else {
                startService(serviceIntent)
            }
        } catch (e: Exception) {
            e.printStackTrace()
        }
    }

    private fun renderErrorPage(title: String, t: Throwable, canRetry: Boolean = true) {
        val stackTrace = StringWriter().also { t.printStackTrace(PrintWriter(it)) }.toString()
        val escaped = stackTrace.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")
        val bgColor = if (isCurrentlyDark) "#09090B" else "#F8FAFC"
        val textColor = if (isCurrentlyDark) "#FAFAFA" else "#09090B"
        val surfaceColor = if (isCurrentlyDark) "#18181B" else "#FFFFFF"
        val borderColor = if (isCurrentlyDark) "#27272A" else "#E2E8F0"
        val mutedColor = if (isCurrentlyDark) "#A1A1AA" else "#64748B"
        val codeColor = if (isCurrentlyDark) "#E4E4E7" else "#18181B"

        val retryBtnHtml = if (canRetry) {
            """<button type="button" onclick="if(window.AndroidTheme && window.AndroidTheme.retryConnection){window.AndroidTheme.retryConnection();}else{location.reload();}">Retry Connection</button>"""
        } else ""

        val html = """
            <!DOCTYPE html>
            <html lang="en">
            <head>
                <meta name="viewport" content="width=device-width, initial-scale=1.0, viewport-fit=cover">
                <style>
                    body {
                        background: $bgColor;
                        color: $textColor;
                        font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, monospace;
                        padding: 24px;
                        margin: 0;
                        box-sizing: border-box;
                    }
                    h2 { color: #EF4444; margin-top: 0; font-size: 1.15rem; }
                    p { color: $mutedColor; font-size: 0.85rem; margin-bottom: 16px; }
                    button {
                        background: #10B981;
                        color: #FAFAFA;
                        border: none;
                        border-radius: 8px;
                        padding: 10px 20px;
                        font-size: 0.9rem;
                        font-weight: 600;
                        cursor: pointer;
                        margin-bottom: 20px;
                        display: inline-block;
                    }
                    button:active {
                        opacity: 0.85;
                    }
                    pre {
                        background: $surfaceColor;
                        border: 1px solid $borderColor;
                        border-radius: 8px;
                        padding: 16px;
                        font-size: 0.78rem;
                        line-height: 1.45;
                        color: $codeColor;
                        overflow-x: auto;
                        white-space: pre-wrap;
                        word-break: break-all;
                    }
                </style>
            </head>
            <body>
                <h2>DirectCrate Startup Error</h2>
                <p>${title}</p>
                $retryBtnHtml
                <pre>$escaped</pre>
            </body>
            </html>
        """.trimIndent()

        handler.post {
            if (!isDestroyed && !isFinishing) {
                webView.loadDataWithBaseURL(null, html, "text/html", "utf-8", null)
            }
        }
    }

    private fun pollAndLoadServer(boundPort: Int) {
        if (isPageLoaded) return

        var ready = false
        var attempts = 0
        val targetPort = if (boundPort > 0) boundPort else DirectCrateService.activeServerPort
        val targetUrl = "http://127.0.0.1:$targetPort"
        val startTime = System.currentTimeMillis()
        var reinvokedServer = false

        while (!ready && (System.currentTimeMillis() - startTime) < 15000) {
            // Check if service encountered an internal startup failure
            DirectCrateService.startupError?.let { err ->
                renderErrorPage("Go backend server failed to start", err, canRetry = true)
                return
            }

            attempts++
            // If 3 seconds elapsed and still not ready, trigger startServer once again as a fail-safe
            if (!reinvokedServer && (System.currentTimeMillis() - startTime) >= 3000) {
                reinvokedServer = true
                try {
                    val (configDir, defaultDest) = getStorageDirs()
                    val p = DirectCrateBridge.startServer(configDir, defaultDest)
                    if (p > 0) {
                        DirectCrateService.activeServerPort = p
                        DirectCrateService.activeServerUrl = "http://127.0.0.1:$p"
                    }
                } catch (e: Exception) {
                    e.printStackTrace()
                }
            }

            try {
                val conn = (URL("$targetUrl/api/status").openConnection() as HttpURLConnection).apply {
                    connectTimeout = 500
                    readTimeout = 500
                    requestMethod = "GET"
                }
                if (conn.responseCode == 200) {
                    ready = true
                }
                conn.disconnect()
            } catch (ignored: Exception) {
                try {
                    Thread.sleep(150)
                } catch (e: InterruptedException) {
                    break
                }
            }
        }

        handler.post {
            if (!isDestroyed && !isFinishing) {
                if (ready) {
                    webView.loadUrl(DirectCrateService.activeServerUrl)
                } else {
                    val err = DirectCrateService.startupError
                        ?: IllegalStateException("DirectCrate HTTP server at $targetUrl did not respond within 15 seconds.")
                    renderErrorPage("Server timeout", err, canRetry = true)
                }
            }
        }
    }

    override fun onResume() {
        super.onResume()
        DirectCrateService.isUiVisible = true
        DirectCrateService.cancelIdleStop()
        if (!DirectCrateService.isServerRunning) {
            initializeBackendAndLoad()
        }
    }

    override fun onPause() {
        DirectCrateService.isUiVisible = false
        super.onPause()
    }

    override fun onDestroy() {
        DirectCrateService.isUiVisible = false
        if (!isChangingConfigurations) {
            DirectCrateService.scheduleIdleStop(2500L)
        }
        if (::webView.isInitialized) {
            webView.destroy()
        }
        super.onDestroy()
    }
}
