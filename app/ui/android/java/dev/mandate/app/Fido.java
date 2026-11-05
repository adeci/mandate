package dev.mandate.app;

import android.app.Activity;
import android.app.PendingIntent;
import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.hardware.usb.UsbConstants;
import android.hardware.usb.UsbDevice;
import android.hardware.usb.UsbDeviceConnection;
import android.hardware.usb.UsbEndpoint;
import android.hardware.usb.UsbInterface;
import android.hardware.usb.UsbManager;
import android.nfc.NfcAdapter;
import android.nfc.Tag;
import android.nfc.tech.IsoDep;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;

/**
 * Moves bytes to and from a FIDO2 security key, over USB (64-byte HID reports)
 * or NFC (APDUs). Native code does the CTAP2 framing; one key at a time.
 */
final class Fido {
    private static final String GRANT = "dev.mandate.app.USB";
    private static final int REPORT = 64;

    private static UsbDeviceConnection usb;
    private static UsbInterface hid;
    private static UsbEndpoint in, out;
    private static IsoDep nfc;

    /** Opens the FIDO HID interface of a key plugged in, asking the owner for leave once. */
    static boolean usb(Activity activity) throws Exception {
        UsbManager manager = activity.getSystemService(UsbManager.class);
        for (UsbDevice device : manager.getDeviceList().values()) {
            for (int i = 0; i < device.getInterfaceCount(); i++) {
                UsbInterface iface = device.getInterface(i);
                if (iface.getInterfaceClass() != UsbConstants.USB_CLASS_HID || iface.getEndpointCount() < 2) {
                    continue;
                }
                if (!manager.hasPermission(device) && !ask(activity, manager, device)) {
                    throw new Exception("mandate may not use the security key.");
                }
                UsbDeviceConnection c = manager.openDevice(device);
                if (c == null || !c.claimInterface(iface, true)) {
                    continue;
                }
                if (!fido(c, iface)) {
                    c.releaseInterface(iface);
                    c.close();
                    continue;
                }
                usb = c;
                hid = iface;
                for (int e = 0; e < iface.getEndpointCount(); e++) {
                    UsbEndpoint ep = iface.getEndpoint(e);
                    if (ep.getDirection() == UsbConstants.USB_DIR_IN) {
                        in = ep;
                    } else {
                        out = ep;
                    }
                }
                return true;
            }
        }
        return false;
    }

    /** Whether the interface's report descriptor has the FIDO usage page, 0xF1D0. */
    private static boolean fido(UsbDeviceConnection c, UsbInterface iface) {
        byte[] d = new byte[256];
        // GET_DESCRIPTOR, HID report descriptor, to the interface
        int n = c.controlTransfer(0x81, 0x06, 0x2200, iface.getId(), d, d.length, 1000);
        for (int i = 0; i + 2 < n; i++) {
            if (d[i] == 0x06 && d[i + 1] == (byte) 0xd0 && d[i + 2] == (byte) 0xf1) {
                return true;
            }
        }
        return false;
    }

    private static boolean ask(Activity activity, UsbManager manager, UsbDevice device) throws Exception {
        CountDownLatch done = new CountDownLatch(1);
        boolean[] granted = {false};
        BroadcastReceiver receiver =
                new BroadcastReceiver() {
                    @Override
                    public void onReceive(Context context, Intent intent) {
                        granted[0] = intent.getBooleanExtra(UsbManager.EXTRA_PERMISSION_GRANTED, false);
                        done.countDown();
                    }
                };
        activity.registerReceiver(receiver, new IntentFilter(GRANT), Context.RECEIVER_NOT_EXPORTED);
        Intent intent = new Intent(GRANT).setPackage(activity.getPackageName());
        manager.requestPermission(
                device, PendingIntent.getBroadcast(activity, 0, intent, PendingIntent.FLAG_MUTABLE));
        done.await();
        activity.unregisterReceiver(receiver);
        return granted[0];
    }

    static void write(byte[] report) throws Exception {
        if (usb.bulkTransfer(out, report, REPORT, 1000) != REPORT) {
            throw new Exception("The security key stopped responding. Plug it back in.");
        }
    }

    /** The next report, or null if none came within ms. */
    static byte[] read(int ms) {
        byte[] report = new byte[REPORT];
        return usb.bulkTransfer(in, report, REPORT, ms) == REPORT ? report : null;
    }

    /** Waits up to ms for a key held to the phone's NFC reader. */
    static boolean nfc(Activity activity, int ms) throws Exception {
        NfcAdapter adapter = NfcAdapter.getDefaultAdapter(activity);
        if (adapter == null || !adapter.isEnabled()) {
            return false;
        }
        CountDownLatch found = new CountDownLatch(1);
        adapter.enableReaderMode(
                activity,
                (Tag tag) -> {
                    IsoDep card = IsoDep.get(tag);
                    if (card == null) {
                        return;
                    }
                    try {
                        card.connect();
                        card.setTimeout(30000);
                        nfc = card;
                        found.countDown();
                    } catch (Exception e) {
                        // Taken away too soon; the next tap tries again.
                    }
                },
                NfcAdapter.FLAG_READER_NFC_A
                        | NfcAdapter.FLAG_READER_NFC_B
                        | NfcAdapter.FLAG_READER_SKIP_NDEF_CHECK
                        | NfcAdapter.FLAG_READER_NO_PLATFORM_SOUNDS,
                null);
        if (!found.await(ms, TimeUnit.MILLISECONDS)) {
            adapter.disableReaderMode(activity);
            return false;
        }
        return true;
    }

    static byte[] transceive(byte[] apdu) throws Exception {
        return nfc.transceive(apdu);
    }

    /** Lets go of the key. */
    static void close(Activity activity) {
        if (usb != null) {
            usb.releaseInterface(hid);
            usb.close();
            usb = null;
        }
        if (nfc != null) {
            try {
                nfc.close();
            } catch (Exception e) {
                // Already gone.
            }
            nfc = null;
            NfcAdapter.getDefaultAdapter(activity).disableReaderMode(activity);
        }
    }
}
