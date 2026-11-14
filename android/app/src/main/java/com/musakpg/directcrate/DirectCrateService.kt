package com.musakpg.directcrate

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.net.wifi.WifiManager
import android.os.Build
import android.os.Environment
import android.os.Handler
import android.os.IBinder
import android.os.Looper
import android.os.PowerManager
import androidx.core.app.NotificationCompat
import java.io.File
import java.net.HttpURLConnection
import java.net.URL
import kotlin.concurrent.thread

class DirectCrateService : Service() {

    companion object {
        const val CHANNEL_ID = "directcrate_service_channel"
        const val NOTIFICATION_ID = 1001
        const val COMPLETION_NOTIFICATION_ID = 1002
        const val THERMAL_NOTIFICATION_ID = 1003
        const val ACTION_STOP_SERVICE = "com.musakpg.directcrate.ACTION_STOP_SERVICE"
        const val ACTION_ABORT_TASK = "com.musakpg.directcrate.ACTION_ABORT_TASK"

        var isServerRunning = false
            internal set
        var isServiceRunning = false
            internal set
        @Volatile
        var isExtracting = false
            internal set
        var activeServerPort = 9090
            internal set
        var activeServerUrl = "http://127.0.0.1:9090"
            internal set
        var startupError: Throwable? = null
            internal set
        @Volatile
        var isUiVisible = false

        private var instance: DirectCrateService? = null
        private val mainHandler = Handler(Looper.getMainLooper())
        private var idleStopRunnable: Runnable? = null

        fun getInstance(): DirectCrateService? = instance

        fun cancelIdleStop() {
            mainHandler.post {
                idleStopRunnable?.let {
                    mainHandler.removeCallbacks(it)
                    idleStopRunnable = null
                }
            }
        }

        fun scheduleIdleStop(delayMs: Long) {
            mainHandler.post {
                idleStopRunnable?.let { mainHandler.removeCallbacks(it) }

                val r = Runnable {
                    idleStopRunnable = null
                    if (isUiVisible) return@Runnable

                    thread(name = "DirectCrateIdleStopCheck", isDaemon = true) {
                        if (isUiVisible || isExtracting || isJobActiveOrQueued()) return@thread

                        mainHandler.post {
                            if (!isUiVisible && !isExtracting && !isJobActiveOrQueued()) {
                                instance?.stopForegroundServiceCleanly()
                            }
                        }
                    }
                }
                idleStopRunnable = r
                mainHandler.postDelayed(r, delayMs)
            }
        }

        fun isJobActiveOrQueued(): Boolean {
            val jsonStr = try {
                DirectCrateBridge.getProgressJson()
            } catch (t: Throwable) {
                ""
            }
            if (!jsonStr.isNullOrEmpty() && jsonStr != "{}") {
                try {
                    val obj = org.json.JSONObject(jsonStr)
                    return obj.optBoolean("active", false) || obj.optInt("queued_count", 0) > 0
                } catch (e: Exception) {
                }
            }
            return false
        }
    }

    private var wakeLock: PowerManager.WakeLock? = null
    private var wifiLock: WifiManager.WifiLock? = null
    private var isMonitoring = false

    override fun onCreate() {
        super.onCreate()
        instance = this
        isServiceRunning = true
        createNotificationChannel()
        acquireLocks()
    }

    fun stopForegroundServiceCleanly() {
        try {
            isMonitoring = false
            isExtracting = false
            val notificationManager = getSystemService(Context.NOTIFICATION_SERVICE) as? NotificationManager
            notificationManager?.cancel(NOTIFICATION_ID)
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.N) {
                stopForeground(STOP_FOREGROUND_REMOVE)
            } else {
                @Suppress("DEPRECATION")
                stopForeground(true)
            }
            stopSelf()
        } catch (e: Exception) {
            e.printStackTrace()
        }
    }

    override fun onTaskRemoved(rootIntent: Intent?) {
        super.onTaskRemoved(rootIntent)
        isUiVisible = false
        // When user swipes app away from Recent Apps:
        // If engine is idle, immediately shut down and remove notification.
        // If extraction is actively running, keep foreground service alive to let it finish.
        if (!isExtracting && !isJobActiveOrQueued()) {
            stopForegroundServiceCleanly()
        }
    }

    private fun acquireLocks() {
        try {
            val pm = getSystemService(Context.POWER_SERVICE) as? PowerManager
            wakeLock = pm?.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "DirectCrate:ExtractionWakeLock")?.apply {
                setReferenceCounted(false)
                acquire()
            }
        } catch (e: Exception) {
            e.printStackTrace()
        }

        try {
            val wm = applicationContext.getSystemService(Context.WIFI_SERVICE) as? WifiManager
            wifiLock = wm?.createWifiLock(WifiManager.WIFI_MODE_FULL_HIGH_PERF, "DirectCrate:ExtractionWifiLock")?.apply {
                setReferenceCounted(false)
                acquire()
            }
        } catch (e: Exception) {
            e.printStackTrace()
        }
    }

    private fun releaseLocks() {
        try {
            if (wakeLock?.isHeld == true) {
                wakeLock?.release()
            }
        } catch (e: Exception) {
            e.printStackTrace()
        }
        wakeLock = null

        try {
            if (wifiLock?.isHeld == true) {
                wifiLock?.release()
            }
        } catch (e: Exception) {
            e.printStackTrace()
        }
        wifiLock = null
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        cancelIdleStop()

        when (intent?.action) {
            ACTION_STOP_SERVICE -> {
                thread(name = "DirectCrateStopFromNotif", isDaemon = true) {
                    try {
                        val url = URL("http://127.0.0.1:$activeServerPort/cancel")
                        val conn = url.openConnection() as HttpURLConnection
                        conn.requestMethod = "POST"
                        conn.connectTimeout = 800
                        conn.readTimeout = 800
                        conn.responseCode
                        conn.disconnect()
                    } catch (ignored: Exception) {}
                }
                stopForegroundServiceCleanly()
                return START_NOT_STICKY
            }
            ACTION_ABORT_TASK -> {
                thread(name = "DirectCrateAbortFromNotif", isDaemon = true) {
                    try {
                        val url = URL("http://127.0.0.1:$activeServerPort/cancel")
                        val conn = url.openConnection() as HttpURLConnection
                        conn.requestMethod = "POST"
                        conn.connectTimeout = 800
                        conn.readTimeout = 800
                        conn.responseCode
                        conn.disconnect()
                    } catch (ignored: Exception) {}
                }
                return START_NOT_STICKY
            }
        }

        val notification = buildForegroundNotification()
        startForeground(NOTIFICATION_ID, notification)

        if (!isServerRunning) {
            isServerRunning = true
            startupError = null

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

            thread(name = "DirectCrateGoBackend", isDaemon = true) {
                try {
                    DirectCrateBridge.loadNativeLibrary()
                    val boundPort = DirectCrateBridge.startServer(configDir, defaultDest)
                    if (boundPort > 0) {
                        activeServerPort = boundPort
                        activeServerUrl = "http://127.0.0.1:$boundPort"
                    }
                } catch (t: Throwable) {
                    startupError = t
                    t.printStackTrace()
                }
            }

            startProgressMonitor()
        }

        return START_NOT_STICKY
    }

    /**
     * Updates notification with live extraction progress bar, or transitions to dismissible
     * completion notification upon task finish.
     */
    fun updateNotificationProgress(
        filename: String,
        percent: Int,
        speedText: String,
        etaText: String,
        isActive: Boolean
    ) {
        val notificationManager = getSystemService(Context.NOTIFICATION_SERVICE) as? NotificationManager ?: return

        if (isActive) {
            isExtracting = true
            cancelIdleStop()

            val openIntent = Intent(this, MainActivity::class.java).apply {
                flags = Intent.FLAG_ACTIVITY_SINGLE_TOP or Intent.FLAG_ACTIVITY_CLEAR_TOP
            }
            val pendingIntent = PendingIntent.getActivity(
                this,
                0,
                openIntent,
                PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
            )

            val abortIntent = Intent(this, DirectCrateService::class.java).apply {
                action = ACTION_ABORT_TASK
            }
            val abortPendingIntent = PendingIntent.getService(
                this,
                3,
                abortIntent,
                PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
            )

            val title = if (filename.isNotBlank()) filename else getString(R.string.service_notification_title)
            val textBuilder = StringBuilder()
            val safePercent = percent.coerceIn(0, 100)
            textBuilder.append("$safePercent%")
            if (speedText.isNotBlank()) {
                textBuilder.append(" • $speedText")
            }
            if (etaText.isNotBlank() && etaText != "--:--") {
                textBuilder.append(" • ETA $etaText")
            }

            val isIndeterminate = safePercent <= 0
            val notif = NotificationCompat.Builder(this, CHANNEL_ID)
                .setContentTitle(title)
                .setContentText(textBuilder.toString())
                .setSmallIcon(android.R.drawable.stat_sys_download)
                .setContentIntent(pendingIntent)
                .setProgress(100, safePercent, isIndeterminate)
                .setOngoing(true)
                .setOnlyAlertOnce(true)
                .addAction(android.R.drawable.ic_menu_close_clear_cancel, getString(R.string.action_abort), abortPendingIntent)
                .build()

            notificationManager.notify(NOTIFICATION_ID, notif)
        } else {
            val wasExtracting = isExtracting
            isExtracting = false

            if (wasExtracting) {
                val doneIntent = Intent(this, MainActivity::class.java).apply {
                    flags = Intent.FLAG_ACTIVITY_SINGLE_TOP or Intent.FLAG_ACTIVITY_CLEAR_TOP
                }
                val donePendingIntent = PendingIntent.getActivity(
                    this,
                    1,
                    doneIntent,
                    PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
                )
                val doneTitle = if (filename.isNotBlank()) "Extraction Complete: $filename" else "DirectCrate Extraction Complete"
                val doneNotif = NotificationCompat.Builder(this, CHANNEL_ID)
                    .setContentTitle(doneTitle)
                    .setContentText("Finished unpacking archive directly to disk.")
                    .setSmallIcon(android.R.drawable.stat_sys_download_done)
                    .setContentIntent(donePendingIntent)
                    .setProgress(0, 0, false)
                    .setOngoing(false)
                    .setAutoCancel(true)
                    .build()

                notificationManager.notify(COMPLETION_NOTIFICATION_ID, doneNotif)
            }

            if (isUiVisible) {
                notificationManager.notify(NOTIFICATION_ID, buildForegroundNotification())
            } else {
                stopForegroundServiceCleanly()
            }
        }
    }

    fun showThermalWarningNotification(tempC: Double, message: String) {
        try {
            val notificationManager = getSystemService(Context.NOTIFICATION_SERVICE) as? NotificationManager ?: return
            val openIntent = Intent(this, MainActivity::class.java).apply {
                flags = Intent.FLAG_ACTIVITY_SINGLE_TOP or Intent.FLAG_ACTIVITY_CLEAR_TOP
            }
            val pendingIntent = PendingIntent.getActivity(
                this, 0, openIntent,
                PendingIntent.FLAG_UPDATE_CURRENT or (if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) PendingIntent.FLAG_IMMUTABLE else 0)
            )
            val notif = NotificationCompat.Builder(this, CHANNEL_ID)
                .setSmallIcon(R.drawable.ic_launcher_foreground)
                .setContentTitle(String.format(java.util.Locale.US, "DirectCrate: Device Warming Up (%.1f°C)", tempC))
                .setContentText(message.ifEmpty { "High-speed decompression is warming your device. Consider setting a speed limit." })
                .setContentIntent(pendingIntent)
                .setAutoCancel(true)
                .setOnlyAlertOnce(true)
                .setPriority(NotificationCompat.PRIORITY_DEFAULT)
                .build()
            notificationManager.notify(THERMAL_NOTIFICATION_ID, notif)
        } catch (e: Exception) {
            e.printStackTrace()
        }
    }

    private fun startProgressMonitor() {
        if (isMonitoring) return
        isMonitoring = true

        thread(name = "DirectCrateProgressMonitor", isDaemon = true) {
            val notificationManager = getSystemService(Context.NOTIFICATION_SERVICE) as? NotificationManager
            var wasActive = false
            var lastFilename = ""

            while (isServerRunning && isMonitoring) {
                try {
                    Thread.sleep(1000)
                    if (!isServerRunning || !isMonitoring) break

                    val jsonStr = try {
                        DirectCrateBridge.getProgressJson()
                    } catch (t: Throwable) {
                        ""
                    }

                    if (jsonStr.isNullOrEmpty() || jsonStr == "{}") continue

                    val obj = org.json.JSONObject(jsonStr)
                    val active = obj.optBoolean("active", false)
                    val paused = obj.optBoolean("paused", false)
                    val filename = obj.optString("filename", "")
                    val percent = obj.optDouble("percent", 0.0).toInt().coerceIn(0, 100)
                    val speed = obj.optString("speed", "")
                    val status = obj.optString("status", "")
                    val queuedCount = obj.optInt("queued_count", 0)
                    val hasBusyWork = active || queuedCount > 0

                    val liveNotifications = obj.optBoolean("live_progress_notifications", true)

                    if (active) {
                        wasActive = true
                        isExtracting = true
                        if (filename.isNotEmpty()) {
                            lastFilename = filename
                        }

                        if (liveNotifications) {
                            val title = if (filename.isNotEmpty()) filename else getString(R.string.service_notification_title)
                            val contentText = if (paused) "Paused" else "$percent% • $speed"

                            val pendingIntent = PendingIntent.getActivity(
                                this,
                                0,
                                Intent(this, MainActivity::class.java),
                                PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
                            )

                            val abortIntent = Intent(this, DirectCrateService::class.java).apply {
                                action = ACTION_ABORT_TASK
                            }
                            val abortPendingIntent = PendingIntent.getService(
                                this,
                                3,
                                abortIntent,
                                PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
                            )

                            val notif = NotificationCompat.Builder(this, CHANNEL_ID)
                                .setContentTitle(title)
                                .setContentText(contentText)
                                .setSmallIcon(android.R.drawable.stat_sys_download)
                                .setContentIntent(pendingIntent)
                                .setProgress(100, percent, percent <= 0)
                                .setOngoing(true)
                                .setOnlyAlertOnce(true)
                                .addAction(android.R.drawable.ic_menu_close_clear_cancel, getString(R.string.action_abort), abortPendingIntent)
                                .build()

                            notificationManager?.notify(NOTIFICATION_ID, notif)
                        } else {
                            notificationManager?.notify(NOTIFICATION_ID, buildForegroundNotification())
                        }
                    } else if (wasActive) {
                        wasActive = false
                        isExtracting = false

                        if (status == "completed" || status.isEmpty()) {
                            val doneTitle = "DirectCrate Extraction Complete"
                            val doneText = if (lastFilename.isNotEmpty()) {
                                "Successfully unpacked '$lastFilename'"
                            } else {
                                "Extraction finished successfully."
                            }
                            val donePendingIntent = PendingIntent.getActivity(
                                this,
                                1,
                                Intent(this, MainActivity::class.java),
                                PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
                            )
                            val doneNotif = NotificationCompat.Builder(this, CHANNEL_ID)
                                .setContentTitle(doneTitle)
                                .setContentText(doneText)
                                .setSmallIcon(android.R.drawable.stat_sys_download_done)
                                .setContentIntent(donePendingIntent)
                                .setProgress(0, 0, false)
                                .setOngoing(false)
                                .setAutoCancel(true)
                                .build()

                            notificationManager?.notify(COMPLETION_NOTIFICATION_ID, doneNotif)
                        }

                        if (isUiVisible) {
                            notificationManager?.notify(NOTIFICATION_ID, buildForegroundNotification())
                        } else if (!hasBusyWork) {
                            // UI is closed and extraction finished; cleanly exit foreground service
                            mainHandler.post {
                                stopForegroundServiceCleanly()
                            }
                        }
                    }
                } catch (e: Exception) {
                    // Ignore transient parsing or JNI bridge errors
                }
            }
        }
    }

    override fun onDestroy() {
        if (instance === this) {
            instance = null
        }
        isServiceRunning = false
        isMonitoring = false
        isExtracting = false
        releaseLocks()
        super.onDestroy()
        thread(name = "DirectCrateServiceDestroy", isDaemon = true) {
            try {
                DirectCrateBridge.stopServer()
            } catch (t: Throwable) {
                t.printStackTrace()
            }
        }
        isServerRunning = false
    }

    override fun onBind(intent: Intent?): IBinder? = null

    private fun createNotificationChannel() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val channel = NotificationChannel(
                CHANNEL_ID,
                getString(R.string.channel_name),
                NotificationManager.IMPORTANCE_LOW
            ).apply {
                description = getString(R.string.channel_description)
                setShowBadge(false)
            }
            val manager = getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
            manager.createNotificationChannel(channel)
        }
    }

    private fun buildForegroundNotification(): Notification {
        val openIntent = Intent(this, MainActivity::class.java).apply {
            flags = Intent.FLAG_ACTIVITY_SINGLE_TOP or Intent.FLAG_ACTIVITY_CLEAR_TOP
        }
        val pendingIntent = PendingIntent.getActivity(
            this,
            0,
            openIntent,
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )

        val stopIntent = Intent(this, DirectCrateService::class.java).apply {
            action = ACTION_STOP_SERVICE
        }
        val stopPendingIntent = PendingIntent.getService(
            this,
            2,
            stopIntent,
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )

        return NotificationCompat.Builder(this, CHANNEL_ID)
            .setContentTitle(getString(R.string.service_notification_title))
            .setContentText(getString(R.string.service_notification_text))
            .setSmallIcon(android.R.drawable.stat_sys_download)
            .setContentIntent(pendingIntent)
            .setProgress(0, 0, false)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .addAction(android.R.drawable.ic_menu_close_clear_cancel, getString(R.string.action_stop), stopPendingIntent)
            .build()
    }
}
