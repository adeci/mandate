package dev.mandate.app;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import java.io.File;

/**
 * Starts the stream service, once the app is paired, when the phone boots or
 * after an update, which ends the app's processes. Both broadcasts may start
 * a foreground service from the background, and a specialUse one also from
 * BOOT_COMPLETED.
 */
public final class Boot extends BroadcastReceiver {
    @Override
    public void onReceive(Context context, Intent intent) {
        String action = intent.getAction();
        boolean start =
                Intent.ACTION_BOOT_COMPLETED.equals(action)
                        || Intent.ACTION_MY_PACKAGE_REPLACED.equals(action);
        if (start && new File(context.getFilesDir(), "device.json").exists()) {
            context.startForegroundService(new Intent(context, Stream.class));
        }
    }
}
