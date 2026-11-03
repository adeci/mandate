package dev.mandate.app;

import android.app.Activity;
import android.hardware.biometrics.BiometricManager.Authenticators;
import android.hardware.biometrics.BiometricPrompt;
import android.os.CancellationSignal;
import android.security.keystore.KeyGenParameterSpec;
import android.security.keystore.KeyProperties;
import android.security.keystore.StrongBoxUnavailableException;
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
 * The device key, which signs only through a BiometricPrompt for that one
 * signature, and the key that seals the device token. Both live in the
 * Android Keystore.
 */
final class Keys {
    private static final String DEVICE = "device", TOKEN = "token";
    private static final int IV = 12;

    /** A new device key, in StrongBox if the phone has one; answers its public key as X.509. */
    static byte[] generate() throws Exception {
        // A 0-second validity: each signature needs its own authentication,
        // and unlocking the phone authorizes none. The screen lock signs as
        // well, so a newly enrolled finger is no reason to drop the key.
        KeyGenParameterSpec.Builder spec =
                new KeyGenParameterSpec.Builder(DEVICE, KeyProperties.PURPOSE_SIGN)
                        .setAlgorithmParameterSpec(new ECGenParameterSpec("secp256r1"))
                        .setDigests(KeyProperties.DIGEST_SHA256)
                        .setUserAuthenticationRequired(true)
                        .setUserAuthenticationParameters(
                                0,
                                KeyProperties.AUTH_BIOMETRIC_STRONG
                                        | KeyProperties.AUTH_DEVICE_CREDENTIAL)
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
    static byte[] publicKey() throws Exception {
        return store().getCertificate(DEVICE).getPublicKey().getEncoded();
    }

    /**
     * SHA256withECDSA over msg, as DER, once the owner authenticates for it in
     * BiometricPrompt with a strong biometric or the screen lock.
     */
    static byte[] sign(Activity activity, byte[] msg) throws Exception {
        Signature s = Signature.getInstance("SHA256withECDSA");
        s.initSign((PrivateKey) store().getKey(DEVICE, null));
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
        activity.runOnUiThread(
                () ->
                        new BiometricPrompt.Builder(activity)
                                .setTitle("Sign with mandate")
                                .setAllowedAuthenticators(
                                        Authenticators.BIOMETRIC_STRONG
                                                | Authenticators.DEVICE_CREDENTIAL)
                                .build()
                                .authenticate(
                                        new BiometricPrompt.CryptoObject(s),
                                        new CancellationSignal(),
                                        activity.getMainExecutor(),
                                        callback));
        done.await();
        if (error[0] != null) {
            throw new Exception(error[0]);
        }
        s.update(msg);
        return s.sign();
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
