package com.musakpg.directcrate

object DirectCrateBridge {
    private var isLoaded = false

    fun loadNativeLibrary() {
        if (!isLoaded) {
            System.loadLibrary("directcrate")
            isLoaded = true
        }
    }

    external fun startServer(configDir: String, defaultDest: String): Int
    external fun stopServer()
    external fun getProgressJson(): String
    external fun updateDeviceThermals(temp: Double, status: String)
}

