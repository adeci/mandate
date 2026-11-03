package dev.mandate.app;

import android.app.NativeActivity;
import android.content.Intent;
import android.content.res.Configuration;
import android.graphics.Insets;
import android.os.Bundle;
import android.view.WindowInsets;
import android.view.WindowInsetsController;

/** The app's one activity: NativeActivity, plus the intents it was opened with. */
public final class Main extends NativeActivity {
    static {
        // Loaded here first so that its JNI_OnLoad sees the app's classes.
        System.loadLibrary("mandate_app");
    }

    /** A mandate://pair link, or the waiting item a notification is about. */
    private static native void opened(String link, String item);

    /** The system bars and cutout at the top and bottom, in logical px. */
    private static native void insets(float top, float bottom);

    @Override
    protected void onCreate(Bundle saved) {
        open(getIntent());
        super.onCreate(saved);
        bars();
        getWindow()
                .getDecorView()
                .setOnApplyWindowInsetsListener(
                        (view, all) -> {
                            int types = WindowInsets.Type.systemBars() | WindowInsets.Type.displayCutout();
                            Insets i = all.getInsets(types);
                            float density = getResources().getDisplayMetrics().density;
                            insets(i.top / density, i.bottom / density);
                            return view.onApplyWindowInsets(all);
                        });
    }

    @Override
    protected void onNewIntent(Intent intent) {
        super.onNewIntent(intent);
        setIntent(intent);
        open(intent);
    }

    @Override
    public void onConfigurationChanged(Configuration config) {
        super.onConfigurationChanged(config);
        bars();
    }

    /** Only Stream.start asks. Its notification, posted before the answer, did not show. */
    @Override
    public void onRequestPermissionsResult(int code, String[] permissions, int[] results) {
        startForegroundService(new Intent(this, Stream.class));
    }

    private static void open(Intent intent) {
        opened(intent.getDataString(), intent.getStringExtra(Stream.ITEM));
    }

    /** System bar icons dark in light mode and light in dark mode, as the app is. */
    private void bars() {
        int night = getResources().getConfiguration().uiMode & Configuration.UI_MODE_NIGHT_MASK;
        int light =
                WindowInsetsController.APPEARANCE_LIGHT_STATUS_BARS
                        | WindowInsetsController.APPEARANCE_LIGHT_NAVIGATION_BARS;
        getWindow()
                .getInsetsController()
                .setSystemBarsAppearance(night == Configuration.UI_MODE_NIGHT_YES ? 0 : light, light);
    }
}
