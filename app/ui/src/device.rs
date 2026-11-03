//! The paired device as saved in the app's data dir.

use mandate_core::{Client, Error, PairLink, Trust};
use serde_json::{Value, json};
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;
use std::sync::Arc;

pub const FILE: &str = "device.json";
const NAME: &str = if cfg!(target_os = "android") { "phone" } else { "desktop" };

/// The device key: in the Android Keystore, or on desktop, until it has a
/// keystore, in the file.
#[cfg(target_os = "android")]
pub type Key = crate::android::Keystore;
#[cfg(not(target_os = "android"))]
pub type Key = mandate_core::SoftwareKey;

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
    pub fn pair(dir: &Path, link: &PairLink) -> Result<Device, Error> {
        let token = mandate_core::new_token();
        let (key, mut saved) = new_secrets(&token)?;
        let client = Client::pair(link, &key, NAME, &token)?;
        saved["url"] = link.url.clone().into();
        if let Trust::Pin(p) = client.trust() {
            saved["pin"] = hex(&p).into();
        }
        fs::create_dir_all(dir)?;
        let mut f = OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .mode(0o600)
            .open(dir.join(FILE))?;
        f.write_all(saved.to_string().as_bytes())?;
        #[cfg(target_os = "android")]
        crate::android::listen();
        Ok(Device { client, key: Arc::new(key) })
    }
}

/// The key and the token from the file: the token sealed by the Keystore.
#[cfg(target_os = "android")]
fn secrets(v: &Value) -> Option<(Key, String)> {
    Some((Key::load()?, crate::android::unseal(&unhex(v["sealed"].as_str()?)?)?))
}

#[cfg(not(target_os = "android"))]
fn secrets(v: &Value) -> Option<(Key, String)> {
    let key = Key::from_bytes(&unhex(v["key"].as_str()?)?.try_into().ok()?).ok()?;
    Some((key, v["token"].as_str()?.into()))
}

/// A new device key, and what the file keeps of it and of the token.
#[cfg(target_os = "android")]
fn new_secrets(token: &str) -> Result<(Key, Value), Error> {
    Ok((Key::generate()?, json!({"sealed": hex(&crate::android::seal(token)?)})))
}

#[cfg(not(target_os = "android"))]
fn new_secrets(token: &str) -> Result<(Key, Value), Error> {
    let key = Key::generate();
    let saved = json!({"key": hex(&key.to_bytes()), "token": token});
    Ok((key, saved))
}

pub fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

fn unhex(s: &str) -> Option<Vec<u8>> {
    (0..s.len()).step_by(2).map(|i| u8::from_str_radix(s.get(i..i + 2)?, 16).ok()).collect()
}
