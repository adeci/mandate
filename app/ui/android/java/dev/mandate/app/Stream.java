package dev.mandate.app;

import android.Manifest;
import android.app.Activity;
import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.content.SharedPreferences;
import android.content.pm.PackageManager;
import android.net.Uri;
import android.os.IBinder;
import android.os.PowerManager;
import android.provider.Settings;
import android.service.notification.StatusBarNotification;
import java.util.Arrays;

/**
 * Holds the owner stream in a process of its own, so that it outlives the
 * activity, and posts a notification for each waiting item.
 */
public final class Stream extends Service {
    static final String ITEM = "item";
    private static final String CONNECTED = "connected", WAITING = "waiting";

    static {
        System.loadLibrary("mandate_app");
    }

    /** Holds the stream of the device paired in dir, calling show and hide; never returns. */
    private native void run(String dir);

    /**
     * Starts the service, asking first for leave to post notifications, then
     * once for leave to run in the background.
     */
    static void start(Activity activity) {
        activity.runOnUiThread(
                () -> {
                    String post = Manifest.permission.POST_NOTIFICATIONS;
                    activity.startForegroundService(new Intent(activity, Stream.class));
                    if (activity.checkSelfPermission(post) != PackageManager.PERMISSION_GRANTED) {
                        // Main asks for the background once this is answered.
                        activity.requestPermissions(new String[] {post}, 0);
                    } else {
                        askOnce(activity);
                    }
                });
    }

    /**
     * Asks once to leave the stream out of battery optimisation, which some
     * phones use to end it; Settings asks again while it applies.
     */
    static void askOnce(Activity activity) {
        String asked = "asked-background";
        SharedPreferences prefs = activity.getSharedPreferences("stream", MODE_PRIVATE);
        if (!background(activity) && !prefs.getBoolean(asked, false)) {
            prefs.edit().putBoolean(asked, true).apply();
            keep(activity);
        }
    }

    /** Whether battery optimisation leaves the app alone. */
    private static boolean background(Context context) {
        return context.getSystemService(PowerManager.class).isIgnoringBatteryOptimizations(context.getPackageName());
    }

    /** Whether Android lets what waits reach the owner: notifications on, no battery optimisation. */
    static boolean kept(Context context) {
        return context.getSystemService(NotificationManager.class).areNotificationsEnabled() && background(context);
    }

    /** Asks for what kept lacks: the app's notification settings first, then leave from battery optimisation. */
    static void keep(Activity activity) {
        activity.runOnUiThread(
                () -> {
                    String pkg = activity.getPackageName();
                    Intent ask =
                            activity.getSystemService(NotificationManager.class).areNotificationsEnabled()
                                    ? new Intent(
                                            Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS,
                                            Uri.parse("package:" + pkg))
                                    : new Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS)
                                            .putExtra(Settings.EXTRA_APP_PACKAGE, pkg);
                    activity.startActivity(ask);
                });
    }

    @Override
    public void onCreate() {
        NotificationManager nm = getSystemService(NotificationManager.class);
        nm.createNotificationChannel(
                new NotificationChannel(CONNECTED, "Connection", NotificationManager.IMPORTANCE_MIN));
        nm.createNotificationChannel(
                new NotificationChannel(WAITING, "Waiting", NotificationManager.IMPORTANCE_HIGH));
        String dir = getFilesDir().getPath();
        new Thread(() -> run(dir), "stream").start();
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int id) {
        // A tap opens the app as its launcher icon does: Main is singleTask,
        // so a running app comes to the front as it was.
        Intent open = getPackageManager().getLaunchIntentForPackage(getPackageName());
        Notification connected =
                new Notification.Builder(this, CONNECTED)
                        .setSmallIcon(R.drawable.notification)
                        .setContentTitle("mandate is connected")
                        .setContentIntent(
                                PendingIntent.getActivity(this, 0, open, PendingIntent.FLAG_IMMUTABLE))
                        .build();
        startForeground(1, connected);
        return START_STICKY;
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }

    /** Notifies of the waiting item id; text names only its identity, connector and kind. */
    void show(String id, String text) {
        Intent open = new Intent(this, Main.class).putExtra(ITEM, id).setIdentifier(id);
        Notification n =
                new Notification.Builder(this, WAITING)
                        .setSmallIcon(R.drawable.notification)
                        .setContentTitle(text)
                        .setVisibility(Notification.VISIBILITY_PUBLIC)
                        .setContentIntent(
                                PendingIntent.getActivity(this, 0, open, PendingIntent.FLAG_IMMUTABLE))
                        .setAutoCancel(true)
                        .build();
        getSystemService(NotificationManager.class).notify(id, 0, n);
    }

    void hide(String id) {
        getSystemService(NotificationManager.class).cancel(id, 0);
    }

    /** The items with a notification up, also those posted before this process started. */
    String[] shown() {
        return Arrays.stream(getSystemService(NotificationManager.class).getActiveNotifications())
                .filter(n -> n.getTag() != null)
                .map(StatusBarNotification::getTag)
                .toArray(String[]::new);
    }
}
