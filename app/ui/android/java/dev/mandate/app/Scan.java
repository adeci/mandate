package dev.mandate.app;

import android.Manifest;
import android.app.Activity;
import android.content.pm.PackageManager;
import android.graphics.Color;
import android.graphics.ImageFormat;
import android.graphics.Matrix;
import android.graphics.SurfaceTexture;
import android.hardware.camera2.CameraAccessException;
import android.hardware.camera2.CameraCaptureSession;
import android.hardware.camera2.CameraCharacteristics;
import android.hardware.camera2.CameraDevice;
import android.hardware.camera2.CameraManager;
import android.hardware.camera2.CaptureRequest;
import android.media.Image;
import android.media.ImageReader;
import android.os.Bundle;
import android.os.Handler;
import android.os.HandlerThread;
import android.util.Size;
import android.view.Gravity;
import android.view.Surface;
import android.view.TextureView;
import android.view.ViewGroup;
import android.widget.FrameLayout;
import android.widget.TextView;
import java.nio.ByteBuffer;
import java.util.Arrays;

/**
 * The camera, reading QR codes until one is a mandate://pair link, which then
 * opens on the pairing screen. Back cancels.
 */
public final class Scan extends Activity implements TextureView.SurfaceTextureListener {
    /** Native code reads the frame's QR code: 0 none, 1 a pairing link (taken), 2 something else. */
    private static native int frame(byte[] luma, int width, int height, int stride);

    /** The owner refused the camera. */
    private static native void denied();

    private static final int NONE = 0, PAIRING = 1, OTHER = 2;

    private TextureView view;
    private TextView note;
    private HandlerThread thread;
    private Handler handler;
    private CameraDevice camera;
    private ImageReader reader;
    private Size size;
    private int sensor;

    @Override
    protected void onCreate(Bundle saved) {
        super.onCreate(saved);
        view = new TextureView(this);
        view.setSurfaceTextureListener(this);
        note = new TextView(this);
        note.setText("Point the camera at the QR code on the setup page.");
        note.setTextColor(Color.WHITE);
        note.setBackgroundColor(0xB3000000);
        note.setGravity(Gravity.CENTER);
        int pad = (int) (24 * getResources().getDisplayMetrics().density);
        note.setPadding(pad, pad, pad, pad * 3);
        FrameLayout root = new FrameLayout(this);
        root.setBackgroundColor(Color.BLACK);
        root.addView(view);
        root.addView(
                note,
                new FrameLayout.LayoutParams(
                        ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.WRAP_CONTENT, Gravity.BOTTOM));
        setContentView(root);
        if (!allowed()) {
            requestPermissions(new String[] {Manifest.permission.CAMERA}, 0);
        }
    }

    @Override
    public void onRequestPermissionsResult(int code, String[] permissions, int[] results) {
        if (allowed()) {
            open();
        } else {
            denied();
            finish();
        }
    }

    @Override
    protected void onResume() {
        super.onResume();
        thread = new HandlerThread("scan");
        thread.start();
        handler = new Handler(thread.getLooper());
        open();
    }

    @Override
    protected void onPause() {
        if (camera != null) {
            camera.close();
            camera = null;
        }
        if (reader != null) {
            reader.close();
            reader = null;
        }
        thread.quitSafely();
        super.onPause();
    }

    private boolean allowed() {
        return checkSelfPermission(Manifest.permission.CAMERA) == PackageManager.PERMISSION_GRANTED;
    }

    /** Opens the back camera once there is leave and somewhere to show it. */
    private void open() {
        if (!allowed() || !view.isAvailable() || camera != null || handler == null) {
            return;
        }
        CameraManager cameras = getSystemService(CameraManager.class);
        try {
            String id = null;
            for (String c : cameras.getCameraIdList()) {
                Integer facing = cameras.getCameraCharacteristics(c).get(CameraCharacteristics.LENS_FACING);
                if (id == null || (facing != null && facing == CameraCharacteristics.LENS_FACING_BACK)) {
                    id = c;
                }
            }
            if (id == null) {
                note.setText("This phone has no camera. Paste the link instead.");
                return;
            }
            CameraCharacteristics c = cameras.getCameraCharacteristics(id);
            sensor = c.get(CameraCharacteristics.SENSOR_ORIENTATION);
            // About 720p: enough for a QR code across the screen, quick to read.
            size = null;
            for (Size s :
                    c.get(CameraCharacteristics.SCALER_STREAM_CONFIGURATION_MAP)
                            .getOutputSizes(ImageFormat.YUV_420_888)) {
                if (s.getWidth() <= 1280 && (size == null || s.getWidth() > size.getWidth())) {
                    size = s;
                }
            }
            reader = ImageReader.newInstance(size.getWidth(), size.getHeight(), ImageFormat.YUV_420_888, 2);
            reader.setOnImageAvailableListener(this::read, handler);
            view.getSurfaceTexture().setDefaultBufferSize(size.getWidth(), size.getHeight());
            fill();
            cameras.openCamera(
                    id,
                    new CameraDevice.StateCallback() {
                        @Override
                        public void onOpened(CameraDevice opened) {
                            camera = opened;
                            preview();
                        }

                        @Override
                        public void onDisconnected(CameraDevice gone) {
                            gone.close();
                        }

                        @Override
                        public void onError(CameraDevice failed, int error) {
                            failed.close();
                        }
                    },
                    handler);
        } catch (CameraAccessException | SecurityException e) {
            note.setText("The camera is not available. Paste the link instead.");
        }
    }

    /** Streams to the screen and to the reader. */
    private void preview() {
        try {
            Surface shown = new Surface(view.getSurfaceTexture());
            CaptureRequest.Builder request = camera.createCaptureRequest(CameraDevice.TEMPLATE_PREVIEW);
            request.addTarget(shown);
            request.addTarget(reader.getSurface());
            request.set(CaptureRequest.CONTROL_AF_MODE, CaptureRequest.CONTROL_AF_MODE_CONTINUOUS_PICTURE);
            camera.createCaptureSession(
                    Arrays.asList(shown, reader.getSurface()),
                    new CameraCaptureSession.StateCallback() {
                        @Override
                        public void onConfigured(CameraCaptureSession session) {
                            try {
                                session.setRepeatingRequest(request.build(), null, handler);
                            } catch (CameraAccessException | IllegalStateException e) {
                                // The camera closed meanwhile.
                            }
                        }

                        @Override
                        public void onConfigureFailed(CameraCaptureSession session) {}
                    },
                    handler);
        } catch (CameraAccessException | IllegalStateException e) {
            // The camera closed meanwhile.
        }
    }

    /** Reads the newest frame's brightness, which is all a QR code needs. */
    private void read(ImageReader from) {
        Image image = from.acquireLatestImage();
        if (image == null) {
            return;
        }
        Image.Plane plane = image.getPlanes()[0];
        ByteBuffer buffer = plane.getBuffer();
        byte[] luma = new byte[buffer.remaining()];
        buffer.get(luma);
        int width = image.getWidth(), height = image.getHeight(), stride = plane.getRowStride();
        image.close();
        switch (frame(luma, width, height, stride)) {
            case PAIRING:
                runOnUiThread(this::finish);
                break;
            case OTHER:
                runOnUiThread(() -> note.setText("Not a mandate code."));
                break;
            default:
                break;
        }
    }

    /** Scales the preview to fill the view without stretching it. */
    private void fill() {
        int rotation = getDisplay().getRotation() * 90;
        boolean turned = (sensor - rotation + 360) % 180 != 0;
        float w = turned ? size.getHeight() : size.getWidth();
        float h = turned ? size.getWidth() : size.getHeight();
        float vw = view.getWidth(), vh = view.getHeight();
        float scale = Math.max(vw / w, vh / h);
        Matrix m = new Matrix();
        m.setScale(scale * w / vw, scale * h / vh, vw / 2, vh / 2);
        view.setTransform(m);
    }

    @Override
    public void onSurfaceTextureAvailable(SurfaceTexture texture, int width, int height) {
        open();
    }

    @Override
    public void onSurfaceTextureSizeChanged(SurfaceTexture texture, int width, int height) {
        if (size != null) {
            fill();
        }
    }

    @Override
    public boolean onSurfaceTextureDestroyed(SurfaceTexture texture) {
        return true;
    }

    @Override
    public void onSurfaceTextureUpdated(SurfaceTexture texture) {}
}
