package dev.mandate.app;

import android.app.NativeActivity;
import android.content.ActivityNotFoundException;
import android.content.ClipData;
import android.content.ClipboardManager;
import android.content.Intent;
import android.content.res.Configuration;
import android.graphics.Insets;
import android.os.Bundle;
import android.provider.MediaStore;
import android.text.format.DateFormat;
import android.view.WindowInsets;
import android.view.WindowInsetsController;

/**
 * The app's one activity: NativeActivity, plus the intents it was opened with,
 * and the clipboard and camera for native code.
 */
public final class Main extends NativeActivity {
    static {
        // Loaded here first so that its JNI_OnLoad sees the app's classes.
        System.loadLibrary("mandate_app");
    }

    /** A mandate://pair link, or the waiting item a notification is about. */
    private static native void opened(String link, String item);

    /** The system bars and cutout at the top and bottom, in logical px, and whether the soft keyboard shows. */
    private static native void insets(float top, float bottom, boolean keyboard);

    /** Whether the phone's clock reads 24-hour. */
    private static native void clock(boolean h24);

    @Override
    protected void onCreate(Bundle saved) {
        open(getIntent());
        super.onCreate(saved);
        bars(getResources().getConfiguration());
        getWindow()
                .getDecorView()
                .setOnApplyWindowInsetsListener(
                        (view, all) -> {
                            // The soft keyboard, while it shows, covers the bottom too.
                            int bars = WindowInsets.Type.systemBars() | WindowInsets.Type.displayCutout();
                            Insets i = Insets.max(all.getInsets(bars), all.getInsets(WindowInsets.Type.ime()));
                            float density = getResources().getDisplayMetrics().density;
                            insets(i.top / density, i.bottom / density, all.isVisible(WindowInsets.Type.ime()));
                            return view.onApplyWindowInsets(all);
                        });
    }

    @Override
    protected void onNewIntent(Intent intent) {
        super.onNewIntent(intent);
        setIntent(intent);
        open(intent);
    }

    /** Also after Settings, where the owner may have switched 12/24-hour time. */
    @Override
    protected void onResume() {
        super.onResume();
        clock(DateFormat.is24HourFormat(this));
    }

    @Override
    public void onConfigurationChanged(Configuration config) {
        super.onConfigurationChanged(config);
        bars(config);
    }

    /** Again on return from a prompt or another app, which may have set the bars their way. */
    @Override
    public void onWindowFocusChanged(boolean focused) {
        super.onWindowFocusChanged(focused);
        if (focused) {
            bars(getResources().getConfiguration());
        }
    }

    /** Only Stream.start asks. Its notification, posted before the answer, did not show. */
    @Override
    public void onRequestPermissionsResult(int code, String[] permissions, int[] results) {
        startForegroundService(new Intent(this, Stream.class));
    }

    private static void open(Intent intent) {
        opened(intent.getDataString(), intent.getStringExtra(Stream.ITEM));
    }

    /** The clipboard's text, or null. */
    String clipboard() {
        ClipData clip = getSystemService(ClipboardManager.class).getPrimaryClip();
        return clip == null || clip.getItemCount() == 0 ? null : clip.getItemAt(0).coerceToText(this).toString();
    }

    void copy(String text) {
        getSystemService(ClipboardManager.class).setPrimaryClip(ClipData.newPlainText("mandate", text));
    }

    /** Back at the app's root: to the background, as Android 12+ does for launcher activities. */
    void leave() {
        runOnUiThread(() -> moveTaskToBack(true));
    }

    /** Opens the camera app, whose QR reading opens the setup page's mandate://pair link. */
    void scan() {
        try {
            startActivity(new Intent(MediaStore.INTENT_ACTION_STILL_IMAGE_CAMERA));
        } catch (ActivityNotFoundException e) {
            // No camera app: pasting the link stays.
        }
    }

    /** System bar icons dark in light mode and light in dark mode, as the app is. */
    private void bars(Configuration config) {
        int night = config.uiMode & Configuration.UI_MODE_NIGHT_MASK;
        int light =
                WindowInsetsController.APPEARANCE_LIGHT_STATUS_BARS
                        | WindowInsetsController.APPEARANCE_LIGHT_NAVIGATION_BARS;
        getWindow()
                .getInsetsController()
                .setSystemBarsAppearance(night == Configuration.UI_MODE_NIGHT_YES ? 0 : light, light);
    }
}
