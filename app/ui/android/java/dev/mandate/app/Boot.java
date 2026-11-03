package dev.mandate.app;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import java.io.File;

/** Starts the stream service when the phone boots, once the app is paired. */
public final class Boot extends BroadcastReceiver {
    @Override
    public void onReceive(Context context, Intent intent) {
        if (Intent.ACTION_BOOT_COMPLETED.equals(intent.getAction())
                && new File(context.getFilesDir(), "device.json").exists()) {
            context.startForegroundService(new Intent(context, Stream.class));
        }
    }
}
