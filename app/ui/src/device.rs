//! The paired device as saved in the app's data dir.

use mandate_core::{Client, DeviceKey, Error, Join, PairLink, Trust};
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

/// A device key: on Android in the Keystore or on a FIDO2 security key, on
/// desktop on a FIDO2 authenticator or a PKCS#11 token.
#[cfg(target_os = "android")]
pub type Key = crate::android::Key;
#[cfg(not(target_os = "android"))]
pub type Key = crate::desktop::Key;

/// A paired device. Each way it approves is a key of its own in the key
/// log, and the one the owner ticked signs.
#[derive(Clone)]
pub struct Device {
    pub client: Client,
    /// The key that signs.
    pub key: Arc<Key>,
    /// Every key of the device, the one that signs among them.
    pub keys: Vec<Arc<Key>>,
}

/// A key's ID in the key log.
pub fn id(key: &Key) -> String {
    key.key("").id
}

impl Device {
    /// The device paired before, if any.
    pub fn load(dir: &Path) -> Option<Device> {
        let v: Value = serde_json::from_slice(&fs::read(dir.join(FILE)).ok()?).ok()?;
        let trust = match v["pin"].as_str() {
            Some(pin) => Trust::Pin(unhex(pin)?.try_into().ok()?),
            None => Trust::Roots,
        };
        // A file from before several ways kept its one key at the top.
        let keys: Vec<Arc<Key>> = match v.get("keys").and_then(Value::as_array) {
            Some(keys) => keys.iter().filter_map(Key::load).map(Arc::new).collect(),
            #[cfg(target_os = "android")]
            None => vec![Arc::new(Key::load(&v)?)],
            #[cfg(not(target_os = "android"))]
            None => vec![Arc::new(Key::load(&v["key"])?)],
        };
        let active = |k: &&Arc<Key>| v["active"].as_str() == Some(id(k).as_str());
        let key = keys.iter().find(active).or(keys.first())?.clone();
        #[cfg(target_os = "android")]
        let token = crate::android::unseal(&unhex(v["sealed"].as_str()?)?)?;
        #[cfg(not(target_os = "android"))]
        let token = {
            let first = id(&keys[0]);
            crate::desktop::load_token(&v, v["token_id"].as_str().unwrap_or(&first))?
        };
        let client = Client::new(v["url"].as_str()?, trust, &token).ok()?;
        Some(Device { client, key, keys })
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
        #[cfg(target_os = "android")]
        {
            saved["sealed"] = hex(&crate::android::seal(token)?).into();
        }
        #[cfg(not(target_os = "android"))]
        {
            let first = id(&key);
            crate::desktop::save_token(&mut saved, &first, token);
            saved["token_id"] = first.into();
        }
        let key = Arc::new(key);
        let device = Device { client, key: key.clone(), keys: vec![key] };
        device.store(dir, saved)?;
        #[cfg(target_os = "android")]
        crate::android::listen();
        Ok(device)
    }

    /// Adds key as another way to approve, once the key log has it.
    pub fn add(&self, dir: &Path, key: Key) -> Result<Device, Error> {
        let mut keys = self.keys.clone();
        keys.push(Arc::new(key));
        self.change(dir, keys, self.key.clone())
    }

    /// Signs with the key of that ID from now on.
    pub fn activate(&self, dir: &Path, key_id: &str) -> Result<Device, Error> {
        let key = self.keys.iter().find(|k| id(k) == key_id).ok_or_else(unknown)?;
        self.change(dir, self.keys.clone(), key.clone())
    }

    /// Forgets a key that does not sign, once the key log retired it; on
    /// Android, the Keystore key goes too.
    pub fn forget(&self, dir: &Path, key_id: &str) -> Result<Device, Error> {
        if id(&self.key) == key_id {
            return Err(Error::Invalid("The key that signs stays.".into()));
        }
        let (gone, keys): (Vec<_>, Vec<_>) =
            self.keys.iter().cloned().partition(|k| id(k) == key_id);
        let device = self.change(dir, keys, self.key.clone())?;
        #[cfg(target_os = "android")]
        for k in gone {
            k.delete()?;
        }
        #[cfg(not(target_os = "android"))]
        drop(gone);
        Ok(device)
    }

    /// The device with keys, key signing, once the file says so.
    fn change(&self, dir: &Path, keys: Vec<Arc<Key>>, key: Arc<Key>) -> Result<Device, Error> {
        let invalid = || Error::Invalid(format!("{FILE} is not as saved"));
        let saved: Value =
            serde_json::from_slice(&fs::read(dir.join(FILE))?).map_err(|_| invalid())?;
        let device = Device { client: self.client.clone(), key, keys };
        device.store(dir, saved)?;
        Ok(device)
    }

    /// Writes the keys and which one signs into saved, and saved to the file.
    fn store(&self, dir: &Path, mut saved: Value) -> Result<(), Error> {
        if let Some(obj) = saved.as_object_mut() {
            for old in ["alias", "fido2", "key"] {
                obj.remove(old);
            }
        }
        // A file from before several ways kept the token under its one key,
        // which comes first.
        #[cfg(not(target_os = "android"))]
        if saved["token_id"].is_null() {
            saved["token_id"] = id(&self.keys[0]).into();
        }
        saved["keys"] = self.keys.iter().map(|k| k.saved()).collect();
        saved["active"] = id(&self.key).into();
        write(dir, &saved)
    }
}

fn unknown() -> Error {
    Error::Invalid("This device has no such key.".into())
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

pub fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

pub fn unhex(s: &str) -> Option<Vec<u8>> {
    (0..s.len()).step_by(2).map(|i| u8::from_str_radix(s.get(i..i + 2)?, 16).ok()).collect()
}
