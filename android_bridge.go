//go:build android && cgo

package main

/*
#include <jni.h>
#include <stdlib.h>

// Static helper functions to safely access JNI function pointers from CGO
static const char* get_utf_chars(JNIEnv *env, jstring str) {
    if (env == NULL || str == NULL) {
        return NULL;
    }
    return (*env)->GetStringUTFChars(env, str, NULL);
}

static void release_utf_chars(JNIEnv *env, jstring str, const char *chars) {
    if (env != NULL && str != NULL && chars != NULL) {
        (*env)->ReleaseStringUTFChars(env, str, chars);
    }
}

static jstring new_utf_string(JNIEnv *env, const char *chars) {
    if (env == NULL || chars == NULL) {
        return NULL;
    }
    return (*env)->NewStringUTF(env, chars);
}
*/
import "C"
import (
	"fmt"
	"net/http"
	"os"
	"sync"
	"unsafe"
)

var (
	bridgeMu      sync.Mutex
	bridgeServer  *http.Server
	bridgeBaseURL string
)

func jstringToString(env *C.JNIEnv, jstr C.jstring) string {
	cStr := C.get_utf_chars(env, jstr)
	if cStr == nil {
		return ""
	}
	defer C.release_utf_chars(env, jstr, cStr)
	return C.GoString(cStr)
}

func startServer(configDir, defaultDest string) int {
	bridgeMu.Lock()
	defer bridgeMu.Unlock()

	if bridgeServer != nil {
		return activeServerPort
	}

	if configDir != "" {
		_ = os.Setenv("HOME", configDir)
		_ = os.Setenv("XDG_CONFIG_HOME", configDir)
		setCustomConfigDir(configDir)
	}
	if defaultDest != "" {
		setCustomDefaultDest(defaultDest)
	}

	initLogging()
	currentSettings = loadSettings()
	if currentSettings.SpeedLimitMBs > 0 {
		globalSpeedLimitBytesPerSec.Store(int64(currentSettings.SpeedLimitMBs) * 1024 * 1024)
	}

	ln, activePort, err := listenWithFallback("127.0.0.1", 9090)
	if err != nil {
		return 0
	}
	activeServerPort = activePort
	bridgeBaseURL = fmt.Sprintf("http://127.0.0.1:%d", activePort)

	bridgeServer = &http.Server{
		Handler: setupRouter(),
	}

	go func() {
		_ = bridgeServer.Serve(ln)
	}()

	startTelemetrySampler()

	return activePort
}

func stopServer() {
	bridgeMu.Lock()
	defer bridgeMu.Unlock()

	if bridgeServer != nil {
		_ = bridgeServer.Close()
		bridgeServer = nil
		bridgeBaseURL = ""
		activeServerPort = 0
	}
}

// StartServer initializes and starts the embedded HTTP server with custom directories.
func StartServer(configDir, defaultDest string) int {
	return startServer(configDir, defaultDest)
}

// StopServer gracefully terminates the running embedded HTTP server.
func StopServer() {
	stopServer()
}

//export Java_com_musakpg_directcrate_DirectCrateBridge_startServer
func Java_com_musakpg_directcrate_DirectCrateBridge_startServer(env *C.JNIEnv, clazz C.jclass, jConfigDir C.jstring, jDefaultDest C.jstring) C.jint {
	configDir := jstringToString(env, jConfigDir)
	defaultDest := jstringToString(env, jDefaultDest)

	port := startServer(configDir, defaultDest)
	return C.jint(port)
}

//export Java_com_musakpg_directcrate_DirectCrateBridge_stopServer
func Java_com_musakpg_directcrate_DirectCrateBridge_stopServer(env *C.JNIEnv, clazz C.jclass) {
	stopServer()
}

//export Java_com_musakpg_directcrate_DirectCrateBridge_getProgressJson
func Java_com_musakpg_directcrate_DirectCrateBridge_getProgressJson(env *C.JNIEnv, clazz C.jclass) C.jstring {
	jsonStr := getActiveProgressSummaryJSON()
	cStr := C.CString(jsonStr)
	defer C.free(unsafe.Pointer(cStr))
	return C.new_utf_string(env, cStr)
}

//export Java_com_musakpg_directcrate_DirectCrateBridge_updateDeviceThermals
func Java_com_musakpg_directcrate_DirectCrateBridge_updateDeviceThermals(env *C.JNIEnv, clazz C.jclass, jTemp C.jdouble, jStatus C.jstring) {
	temp := float64(jTemp)
	status := jstringToString(env, jStatus)
	manager.Update(func(s *ExtractionState) {
		s.TempC = temp
		s.ThermalStatus = status
	})
}
