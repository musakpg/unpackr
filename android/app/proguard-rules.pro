# Keep DirectCrateBridge JNI bindings
-keep class com.musakpg.directcrate.DirectCrateBridge { *; }
-keepclassmembers class com.musakpg.directcrate.DirectCrateBridge {
    public static <methods>;
}

# Keep JavaScript interface methods for WebView communication
-keepclassmembers class * {
    @android.webkit.JavascriptInterface <methods>;
}
-keep class com.musakpg.directcrate.MainActivity$AndroidThemeInterface { *; }
