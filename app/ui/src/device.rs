//! The paired device as saved in the app's data dir.

#[cfg(not(target_os = "android"))]
use mandate_core::DeviceKey;
use mandate_core::{Client, Error, Join, PairLink, Trust};
use serde_json::{Value, json};
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;
use std::sync::Arc;

pub const FILE: &str = "device.json";

/// What the device calls itself in the key log: the phone's name from its
/// settings, or the computer's hostname.
pub fn name() -> String {
    #[cfg(target_os = "android")]
    return crate::android::device_name().unwrap_or_else(|| "phone".into());
    #[cfg(not(target_os = "android"))]
    {
        let host = fs::read_to_string("/proc/sys/kernel/hostname").unwrap_or_default();
        if host.trim().is_empty() { "computer".into() } else { host.trim().into() }
    }
}

/// The device key: on Android in the Keystore or on a FIDO2 security key, on
/// desktop on the FIDO2 authenticator or PKCS#11 token the owner chose.
#[cfg(target_os = "android")]
pub type Key = crate::android::Key;
#[cfg(not(target_os = "android"))]
pub type Key = crate::desktop::Key;

#[derive(Clone)]
pub struct Device {
    pub client: Client,
    pub key: Arc<Key>,
}

impl Device {
    /// The device paired before, if any.
    pub fn load(dir: &Path) -> Option<Device> {
        let v: Value = serde_json::from_slice(&fs::read(dir.join(FILE)).ok()?).ok()?;
        let trust = match v["pin"].as_str() {
            Some(pin) => Trust::Pin(unhex(pin)?.try_into().ok()?),
            None => Trust::Roots,
        };
        let (key, token) = secrets(&v)?;
        let client = Client::new(v["url"].as_str()?, trust, &token).ok()?;
        Some(Device { client, key: Arc::new(key) })
    }

    /// Pairs as the first device and saves what that made.
    pub fn pair(dir: &Path, link: &PairLink, key: Key) -> Result<Device, Error> {
        let token = mandate_core::new_token();
        let client = Client::pair(link, &key, &name(), &token)?;
        Device::save(dir, &link.url, client, key, &token)
    }

    /// The device once the owner let it in from another, saved.
    pub fn joined(
        dir: &Path,
        url: &str,
        join: Join,
        key: Key,
        token: &str,
    ) -> Result<Device, Error> {
        Device::save(dir, url, join.into_client()?, key, token)
    }

    fn save(dir: &Path, url: &str, client: Client, key: Key, token: &str) -> Result<Device, Error> {
        let mut saved = json!({ "url": url });
        if let Trust::Pin(p) = client.trust() {
            saved["pin"] = hex(&p).into();
        }
        save_secrets(&mut saved, &key, token)?;
        write(dir, &saved)?;
        #[cfg(target_os = "android")]
        crate::android::listen();
        Ok(Device { client, key: Arc::new(key) })
    }

    /// Moves the device to key once the server has accepted it in the key
    /// log: the file keeps the address and token under the new key, and the
    /// old key is removed.
    pub fn rotate(&self, dir: &Path, key: Key) -> Result<Device, Error> {
        let invalid = || Error::Invalid(format!("{FILE} is not as saved"));
        let mut saved: Value =
            serde_json::from_slice(&fs::read(dir.join(FILE))?).map_err(|_| invalid())?;
        let (_, token) = secrets(&saved).ok_or_else(invalid)?;
        save_secrets(&mut saved, &key, &token)?;
        write(dir, &saved)?;
        #[cfg(target_os = "android")]
        self.key.delete()?;
        #[cfg(not(target_os = "android"))]
        crate::desktop::forget_token(&self.key.key("").id);
        Ok(Device { client: self.client.clone(), key: Arc::new(key) })
    }
}

fn write(dir: &Path, saved: &Value) -> Result<(), Error> {
    fs::create_dir_all(dir)?;
    let mut f = OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600)
        .open(dir.join(FILE))?;
    f.write_all(saved.to_string().as_bytes())?;
    Ok(())
}

/// The key and the token from the file: the token sealed by the Keystore.
#[cfg(target_os = "android")]
fn secrets(v: &Value) -> Option<(Key, String)> {
    let key = match v.get("fido2") {
        Some(f) => {
            let point = unhex(f["public_key"].as_str()?)?.try_into().ok()?;
            let id = unhex(f["credential_id"].as_str()?)?;
            Key::Fido(crate::fido::android::Fido::new(point, id))
        }
        None => {
            let alias = v["alias"].as_str().unwrap_or(crate::android::FIRST_ALIAS);
            Key::Keystore(crate::android::Keystore::load(alias)?)
        }
    };
    Some((key, crate::android::unseal(&unhex(v["sealed"].as_str()?)?)?))
}

#[cfg(target_os = "android")]
fn save_secrets(saved: &mut Value, key: &Key, token: &str) -> Result<(), Error> {
    let obj = saved.as_object_mut().expect("device.json is an object");
    obj.remove("alias");
    obj.remove("fido2");
    match key {
        Key::Keystore(k) => saved["alias"] = k.alias().into(),
        Key::Fido(k) => {
            saved["fido2"] = json!({
                "public_key": hex(&k.public_key()),
                "credential_id": hex(k.credential_id()),
            })
        }
    }
    saved["sealed"] = hex(&crate::android::seal(token)?).into();
    Ok(())
}

#[cfg(not(target_os = "android"))]
use crate::desktop::{load_secrets as secrets, save_secrets};

pub fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

pub fn unhex(s: &str) -> Option<Vec<u8>> {
    (0..s.len()).step_by(2).map(|i| u8::from_str_radix(s.get(i..i + 2)?, 16).ok()).collect()
}
