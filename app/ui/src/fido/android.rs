//! A FIDO2 security key on Android, over USB or NFC (Fido.java moves the
//! bytes). An NFC key loses its PIN token when it leaves the reader, so every
//! signature logs in afresh: `unlock` keeps the PIN for the next one only.

use super::hid::{Hid, REPORT, Reports};
use super::nfc::{Card, Nfc};
use super::{AGAIN, Transport, key_error, odd, random};
use crate::android::{activity, java};
use jni::objects::{JByteArray, JValue};
use jni::{jni_sig, jni_str};
use mandate_core::{DeviceKey, DeviceSignature, Error, Key};
use std::sync::Mutex;
use std::time::Instant;

/// How long to wait for a key held to the phone.
const TAP_MS: i32 = 30_000;

/// A fido2 device key: a credential for rp ID `mandate` on a security key.
pub struct Fido {
    public_key: [u8; 65],
    credential_id: Vec<u8>,
    pin: Mutex<Option<String>>,
}

impl Fido {
    /// Makes an ES256 credential for rp `mandate` under name on the key the
    /// owner plugs in or holds to the phone, then touches. Keeps the PIN for
    /// the first signature, which pairing makes at once.
    pub fn create(pin: &str, name: &str) -> Result<Fido, Error> {
        let (credential_id, public_key) = with_key(|dev| {
            let session = super::login(dev, pin)?;
            super::make_credential(dev, &session, name)
        })?;
        Ok(Fido { public_key, credential_id, pin: Mutex::new(Some(pin.into())) })
    }

    /// A credential made before.
    pub fn new(public_key: [u8; 65], credential_id: Vec<u8>) -> Fido {
        Fido { public_key, credential_id, pin: Mutex::new(None) }
    }

    pub fn public_key(&self) -> [u8; 65] {
        self.public_key
    }

    pub fn credential_id(&self) -> &[u8] {
        &self.credential_id
    }

    /// Whether signing needs `unlock` first.
    pub fn locked(&self) -> bool {
        self.pin.lock().unwrap().is_none()
    }

    /// Keeps the PIN for the next signature; the key checks it then.
    pub fn unlock(&self, pin: &str) {
        *self.pin.lock().unwrap() = Some(pin.into());
    }

    /// Proves the owner is here, signing nothing the server sees.
    pub fn confirm(&self, pin: &str) -> Result<(), Error> {
        self.unlock(pin);
        self.sign(&random::<32>()).map(drop)
    }
}

impl DeviceKey for Fido {
    fn key(&self, name: &str) -> Key {
        Key::fido2(&self.public_key, &self.credential_id, name)
    }

    /// Blocks until the owner plugs in or holds the key to the phone and
    /// touches it; needs the PIN from `create` or `unlock`.
    fn sign(&self, challenge: &[u8; 32]) -> Result<DeviceSignature, Error> {
        let pin = self.pin.lock().unwrap().take().ok_or_else(|| key_error(AGAIN))?;
        with_key(|dev| {
            let session = super::login(dev, &pin)?;
            super::assert(dev, &session, &self.credential_id, challenge).map_err(Error::from)
        })
    }
}

/// Runs f on the key plugged in, or else on one held to the phone.
fn with_key<T>(f: impl FnOnce(&mut dyn Transport) -> Result<T, Error>) -> Result<T, Error> {
    let done = if fido_call(jni_str!("usb"))? {
        Hid::open(Usb).and_then(|mut dev| f(&mut dev))
    } else if fido_wait()? {
        Nfc::open(Tag).and_then(|mut dev| f(&mut dev))
    } else {
        Err(key_error("Plug in your security key, or hold it to the back of the phone."))
    };
    let _ = java(|env, c| {
        let a = activity(env);
        let sig = jni_sig!("(Landroid/app/Activity;)V");
        env.call_static_method(&c.fido, jni_str!("close"), sig, &[(&a).into()])?.v()
    });
    done
}

fn fido_call(name: &jni::strings::JNIStr) -> Result<bool, Error> {
    java(|env, c| {
        let a = activity(env);
        let sig = jni_sig!("(Landroid/app/Activity;)Z");
        env.call_static_method(&c.fido, name, sig, &[(&a).into()])?.z()
    })
}

fn fido_wait() -> Result<bool, Error> {
    java(|env, c| {
        let a = activity(env);
        let sig = jni_sig!("(Landroid/app/Activity;I)Z");
        env.call_static_method(&c.fido, jni_str!("nfc"), sig, &[(&a).into(), JValue::Int(TAP_MS)])?
            .z()
    })
}

/// The key's FIDO HID interface, opened by `Fido.usb`.
struct Usb;

impl Reports for Usb {
    fn write(&mut self, report: &[u8; REPORT]) -> Result<(), Error> {
        java(|env, c| {
            let r = env.byte_array_from_slice(report)?;
            env.call_static_method(&c.fido, jni_str!("write"), jni_sig!("([B)V"), &[(&r).into()])?
                .v()
        })
    }

    fn read(&mut self, deadline: Instant) -> Result<[u8; REPORT], Error> {
        loop {
            let left = deadline.saturating_duration_since(Instant::now());
            if left.is_zero() {
                return Err(key_error("The security key didn't answer."));
            }
            let ms = left.as_millis().clamp(1, 1000) as i32;
            let report = java(|env, c| {
                let sig = jni_sig!("(I)[B");
                let r = env
                    .call_static_method(&c.fido, jni_str!("read"), sig, &[JValue::Int(ms)])?
                    .l()?;
                if r.is_null() {
                    return Ok(None);
                }
                let r = JByteArray::cast_local(env, r)?;
                env.convert_byte_array(r).map(Some)
            })?;
            if let Some(r) = report {
                return r.try_into().map_err(|_| odd());
            }
        }
    }
}

/// The key on the NFC reader, connected by `Fido.nfc`.
struct Tag;

impl Card for Tag {
    fn transceive(&mut self, apdu: &[u8]) -> Result<Vec<u8>, Error> {
        java(|env, c| {
            let a = env.byte_array_from_slice(apdu)?;
            let sig = jni_sig!("([B)[B");
            let r = env
                .call_static_method(&c.fido, jni_str!("transceive"), sig, &[(&a).into()])?
                .l()?;
            let r = JByteArray::cast_local(env, r)?;
            env.convert_byte_array(r)
        })
        .map_err(|_| {
            key_error("The security key left the phone too soon. Hold it still and try again.")
        })
    }
}
