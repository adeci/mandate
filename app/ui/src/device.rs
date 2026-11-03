//! The paired device as saved in the app's data dir, and the owner API
//! bodies the app reads.

use mandate_core::{Client, Error, PairLink, SoftwareKey, Trust};
use serde::Deserialize;
use serde_json::{Value, json};
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;
use std::sync::Arc;

const FILE: &str = "device.json";
const NAME: &str = if cfg!(target_os = "android") { "phone" } else { "desktop" };

#[derive(Clone)]
pub struct Device {
    pub client: Client,
    pub key: Arc<SoftwareKey>,
}

impl Device {
    /// The device paired before, if any.
    pub fn load(dir: &Path) -> Option<Device> {
        let v: Value = serde_json::from_slice(&fs::read(dir.join(FILE)).ok()?).ok()?;
        let field = |k: &str| v[k].as_str();
        let trust = match field("pin") {
            Some(pin) => Trust::Pin(unhex(pin)?.try_into().ok()?),
            None => Trust::Roots,
        };
        let key = SoftwareKey::from_bytes(&unhex(field("key")?)?.try_into().ok()?).ok()?;
        let client = Client::new(field("url")?, trust, field("token")?).ok()?;
        Some(Device { client, key: Arc::new(key) })
    }

    /// Pairs as the first device and saves what that made.
    pub fn pair(dir: &Path, link: &PairLink) -> Result<Device, Error> {
        let key = SoftwareKey::generate();
        let token = mandate_core::new_token();
        let client = Client::pair(link, &key, NAME, &token)?;
        let pin = match client.trust() {
            Trust::Pin(p) => Some(hex(&p)),
            Trust::Roots => None,
        };
        let saved =
            json!({"url": link.url, "pin": pin, "token": token, "key": hex(&key.to_bytes())});
        fs::create_dir_all(dir)?;
        let mut f = OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .mode(0o600)
            .open(dir.join(FILE))?;
        f.write_all(saved.to_string().as_bytes())?;
        Ok(Device { client, key: Arc::new(key) })
    }
}

pub fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

fn unhex(s: &str) -> Option<Vec<u8>> {
    (0..s.len()).step_by(2).map(|i| u8::from_str_radix(s.get(i..i + 2)?, 16).ok()).collect()
}

#[derive(Clone, Default, Deserialize)]
pub struct Now {
    #[serde(default)]
    pub requests: Vec<Request>,
    #[serde(default)]
    pub proposals: Vec<Proposal>,
    #[serde(default)]
    pub grants: Vec<OpenGrant>,
}

#[derive(Clone, Deserialize)]
pub struct Request {
    pub id: String,
    pub identity_name: String,
    pub connector: String,
    #[serde(default)]
    pub terms: Vec<Term>,
    #[serde(default)]
    pub reason: String,
}

#[derive(Clone, Deserialize)]
pub struct Term {
    #[serde(default)]
    pub chats: Option<Vec<Chat>>,
}

#[derive(Clone, Deserialize)]
pub struct Chat {
    pub id: String,
    pub name: String,
}

impl Request {
    /// The name the request gave a chat ID, for showing beside it.
    pub fn chat_name(&self, id: &str) -> Option<&str> {
        self.terms
            .iter()
            .flat_map(|t| t.chats.iter().flatten())
            .find(|c| c.id == id)
            .map(|c| c.name.as_str())
    }
}

#[derive(Clone, Deserialize)]
pub struct Proposal {
    pub id: String,
    pub identity_name: String,
    pub connector: String,
    pub op: String,
}

#[derive(Clone, Deserialize)]
pub struct OpenGrant {
    pub identity_name: String,
    pub connector: String,
    #[serde(default)]
    pub until: Option<String>,
}

#[derive(Clone, Deserialize)]
pub struct Exchange {
    pub id: String,
    pub kind: String,
    pub name: String,
}
