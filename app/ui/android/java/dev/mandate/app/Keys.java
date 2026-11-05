package dev.mandate.app;

import android.app.Activity;
import android.app.KeyguardManager;
import android.hardware.biometrics.BiometricManager;
import android.hardware.biometrics.BiometricManager.Authenticators;
import android.hardware.biometrics.BiometricPrompt;
import android.os.CancellationSignal;
import android.security.keystore.KeyInfo;
import android.security.keystore.KeyGenParameterSpec;
import android.security.keystore.KeyProperties;
import android.security.keystore.StrongBoxUnavailableException;
import java.security.KeyFactory;
import java.security.KeyPairGenerator;
import java.security.KeyStore;
import java.security.PrivateKey;
import java.security.Signature;
import java.security.spec.ECGenParameterSpec;
import java.util.Arrays;
import java.util.concurrent.CountDownLatch;
import javax.crypto.Cipher;
import javax.crypto.KeyGenerator;
import javax.crypto.SecretKey;
import javax.crypto.spec.GCMParameterSpec;

/**
 * Device keys, which sign only through a BiometricPrompt for that one
 * signature, and the key that seals the device token. All live in the
 * Android Keystore. Each device key has its own alias, so a new one can be
 * made while the old one still signs.
 */
final class Keys {
    private static final String TOKEN = "token";
    private static final int IV = 12;
    /** Methods: a strong biometric or the screen lock, or the screen lock alone. */
    static final int BIOMETRIC = 1, SCREEN_LOCK = 2;

    /** A new device key under alias, in StrongBox if the phone has one; answers its public key as X.509. */
    static byte[] generate(String alias, int method) throws Exception {
        // A 0-second validity: each signature needs its own authentication,
        // and unlocking the phone authorizes none. The screen lock signs as
        // well, so a newly enrolled finger is no reason to drop the key.
        int auth =
                method == SCREEN_LOCK
                        ? KeyProperties.AUTH_DEVICE_CREDENTIAL
                        : KeyProperties.AUTH_BIOMETRIC_STRONG | KeyProperties.AUTH_DEVICE_CREDENTIAL;
        KeyGenParameterSpec.Builder spec =
                new KeyGenParameterSpec.Builder(alias, KeyProperties.PURPOSE_SIGN)
                        .setAlgorithmParameterSpec(new ECGenParameterSpec("secp256r1"))
                        .setDigests(KeyProperties.DIGEST_SHA256)
                        .setUserAuthenticationRequired(true)
                        .setUserAuthenticationParameters(0, auth)
                        .setInvalidatedByBiometricEnrollment(false);
        KeyPairGenerator gen = KeyPairGenerator.getInstance(KeyProperties.KEY_ALGORITHM_EC, "AndroidKeyStore");
        try {
            gen.initialize(spec.setIsStrongBoxBacked(true).build());
            return gen.generateKeyPair().getPublic().getEncoded();
        } catch (StrongBoxUnavailableException e) {
            gen.initialize(spec.setIsStrongBoxBacked(false).build());
            return gen.generateKeyPair().getPublic().getEncoded();
        }
    }

    /** The device key's public key as X.509. */
    static byte[] publicKey(String alias) throws Exception {
        return store().getCertificate(alias).getPublicKey().getEncoded();
    }

    static void delete(String alias) throws Exception {
        store().deleteEntry(alias);
    }

    /** The method the key at alias was made with. */
    static int method(String alias) throws Exception {
        PrivateKey key = (PrivateKey) store().getKey(alias, null);
        KeyInfo info = KeyFactory.getInstance(key.getAlgorithm(), "AndroidKeyStore").getKeySpec(key, KeyInfo.class);
        return (info.getUserAuthenticationType() & KeyProperties.AUTH_BIOMETRIC_STRONG) != 0 ? BIOMETRIC : SCREEN_LOCK;
    }

    /** Bit 1: a strong biometric is enrolled; bit 2: a secure screen lock is set. */
    static int available(Activity activity) {
        int biometric =
                activity.getSystemService(BiometricManager.class).canAuthenticate(Authenticators.BIOMETRIC_STRONG)
                                == BiometricManager.BIOMETRIC_SUCCESS
                        ? 1
                        : 0;
        int lock = activity.getSystemService(KeyguardManager.class).isDeviceSecure() ? 2 : 0;
        return biometric | lock;
    }

    /**
     * SHA256withECDSA over msg, as DER, once the owner authenticates for it in
     * BiometricPrompt with the key's method.
     */
    static byte[] sign(Activity activity, String alias, byte[] msg) throws Exception {
        Signature s = Signature.getInstance("SHA256withECDSA");
        s.initSign((PrivateKey) store().getKey(alias, null));
        prompt(activity, "Sign with mandate", method(alias), new BiometricPrompt.CryptoObject(s));
        s.update(msg);
        return s.sign();
    }

    /** Asks the owner to authenticate as the key at alias would, signing nothing. */
    static void authenticate(Activity activity, String alias) throws Exception {
        prompt(activity, "Unlock mandate", method(alias), null);
    }

    /** Shows BiometricPrompt and waits for the owner; a cancel or failure throws its message. */
    private static void prompt(Activity activity, String title, int method, BiometricPrompt.CryptoObject crypto)
            throws Exception {
        CountDownLatch done = new CountDownLatch(1);
        String[] error = {null};
        BiometricPrompt.AuthenticationCallback callback =
                new BiometricPrompt.AuthenticationCallback() {
                    @Override
                    public void onAuthenticationSucceeded(BiometricPrompt.AuthenticationResult r) {
                        done.countDown();
                    }

                    @Override
                    public void onAuthenticationError(int code, CharSequence message) {
                        error[0] = String.valueOf(message);
                        done.countDown();
                    }
                };
        int allowed =
                method == SCREEN_LOCK
                        ? Authenticators.DEVICE_CREDENTIAL
                        : Authenticators.BIOMETRIC_STRONG | Authenticators.DEVICE_CREDENTIAL;
        activity.runOnUiThread(
                () -> {
                    BiometricPrompt p =
                            new BiometricPrompt.Builder(activity)
                                    .setTitle(title)
                                    .setAllowedAuthenticators(allowed)
                                    .build();
                    CancellationSignal cancel = new CancellationSignal();
                    if (crypto == null) {
                        p.authenticate(cancel, activity.getMainExecutor(), callback);
                    } else {
                        p.authenticate(crypto, cancel, activity.getMainExecutor(), callback);
                    }
                });
        done.await();
        if (error[0] != null) {
            throw new Exception(error[0]);
        }
    }

    /** The token sealed under a new AES-GCM key: IV then ciphertext. */
    static byte[] seal(byte[] token) throws Exception {
        KeyGenerator gen = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, "AndroidKeyStore");
        gen.init(
                new KeyGenParameterSpec.Builder(
                                TOKEN, KeyProperties.PURPOSE_ENCRYPT | KeyProperties.PURPOSE_DECRYPT)
                        .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                        .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                        .build());
        Cipher c = Cipher.getInstance("AES/GCM/NoPadding");
        c.init(Cipher.ENCRYPT_MODE, gen.generateKey());
        byte[] iv = c.getIV(), sealed = c.doFinal(token);
        byte[] out = Arrays.copyOf(iv, IV + sealed.length);
        System.arraycopy(sealed, 0, out, IV, sealed.length);
        return out;
    }

    static byte[] unseal(byte[] sealed) throws Exception {
        Cipher c = Cipher.getInstance("AES/GCM/NoPadding");
        SecretKey key = (SecretKey) store().getKey(TOKEN, null);
        c.init(Cipher.DECRYPT_MODE, key, new GCMParameterSpec(128, sealed, 0, IV));
        return c.doFinal(sealed, IV, sealed.length - IV);
    }

    private static KeyStore store() throws Exception {
        KeyStore ks = KeyStore.getInstance("AndroidKeyStore");
        ks.load(null);
        return ks;
    }
}
