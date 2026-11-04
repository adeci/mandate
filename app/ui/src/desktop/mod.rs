//! The Linux desktop: the device key on a FIDO2 authenticator or a PKCS#11
//! token, the device token in the Secret Service, one window per data dir,
//! and `--background`, which notifies of what waits.

pub mod fido;
pub mod pkcs11;

use crate::device::{Device, hex, unhex};
use mandate_core::{DeviceKey, DeviceSignature, Error};
use serde_json::{Value, json};
use std::collections::HashMap;
use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Duration;

/// The app's ID: its .desktop file, icon and window class.
pub const APP_ID: &str = "dev.mandate.app";

/// The device key the owner chose when this computer paired or joined.
pub struct Key {
    kind: Kind,
    /// What the owner knows it by: the authenticator's or the token's name.
    pub name: String,
}

enum Kind {
    Fido(fido::Fido),
    Pkcs11(pkcs11::Pkcs11),
}

impl Key {
    pub fn fido(key: fido::Fido, name: &str) -> Key {
        Key { kind: Kind::Fido(key), name: name.into() }
    }

    pub fn pkcs11(key: pkcs11::Pkcs11, token: &str) -> Key {
        Key { kind: Kind::Pkcs11(key), name: token.into() }
    }

    /// The key as device.json keeps it.
    fn load(v: &Value) -> Option<Key> {
        let point = unhex(v["public_key"].as_str()?)?.try_into().ok()?;
        let name = v["name"].as_str()?;
        match v["type"].as_str()? {
            "fido2" => {
                let id = unhex(v["credential_id"].as_str()?)?;
                Some(Key::fido(fido::Fido::new(point, id), name))
            }
            "pkcs11" => {
                let found = pkcs11::Found {
                    id: unhex(v["id"].as_str()?)?,
                    label: String::new(),
                    public_key: point,
                };
                let module = Path::new(v["module"].as_str()?);
                Some(Key::pkcs11(
                    pkcs11::Pkcs11::new(module, v["token"].as_str()?, &found, None),
                    name,
                ))
            }
            _ => None,
        }
    }

    fn saved(&self) -> Value {
        match &self.kind {
            Kind::Fido(k) => json!({
                "type": "fido2",
                "name": self.name,
                "public_key": hex(&k.public_key()),
                "credential_id": hex(k.credential_id()),
            }),
            Kind::Pkcs11(k) => json!({
                "type": "pkcs11",
                "name": self.name,
                "public_key": hex(&k.public_key()),
                "module": k.module(),
                "token": k.serial(),
                "id": hex(k.id()),
            }),
        }
    }

    /// Whether signing needs the PIN first.
    pub fn locked(&self) -> bool {
        match &self.kind {
            Kind::Fido(k) => k.locked(),
            Kind::Pkcs11(k) => k.locked(),
        }
    }

    pub fn unlock(&self, pin: &str) -> Result<(), Error> {
        match &self.kind {
            Kind::Fido(k) => k.unlock(pin),
            Kind::Pkcs11(k) => k.unlock(pin),
        }
    }

    /// Whether every signature waits on a touch.
    pub fn touch(&self) -> bool {
        matches!(self.kind, Kind::Fido(_))
    }

    /// What Settings shows of it.
    pub fn rows(&self) -> Vec<(&'static str, String)> {
        match &self.kind {
            Kind::Fido(_) => {
                vec![("Type", "FIDO2 security key".into()), ("Name", self.name.clone())]
            }
            Kind::Pkcs11(k) => vec![
                ("Type", "PKCS#11 token".into()),
                ("Token", self.name.clone()),
                ("Module", k.module().display().to_string()),
            ],
        }
    }
}

impl DeviceKey for Key {
    fn key(&self, name: &str) -> mandate_core::Key {
        match &self.kind {
            Kind::Fido(k) => k.key(name),
            Kind::Pkcs11(k) => k.key(name),
        }
    }

    fn sign(&self, challenge: &[u8; 32]) -> Result<DeviceSignature, Error> {
        match &self.kind {
            Kind::Fido(k) => k.sign(challenge),
            Kind::Pkcs11(k) => k.sign(challenge),
        }
    }
}

/// Keeps the key in saved and the device token in the Secret Service, or
/// in saved, which is written 0600, where no Secret Service runs.
pub fn save_secrets(saved: &mut Value, key: &Key, token: &str) -> Result<(), Error> {
    saved["key"] = key.saved();
    let id = key.key("").id;
    let stored = futures::executor::block_on(async {
        let keyring = oo7::Keyring::new().await?;
        keyring.unlock().await?;
        keyring.create_item(TOKEN_LABEL, &vec![(TOKEN_ATTRIBUTE, id.as_str())], token, true).await
    });
    if stored.is_err() {
        saved["token"] = token.into();
    }
    Ok(())
}

/// The key and the device token that save_secrets kept.
pub fn load_secrets(saved: &Value) -> Option<(Key, String)> {
    let key = Key::load(&saved["key"])?;
    if let Some(token) = saved["token"].as_str() {
        return Some((key, token.into()));
    }
    let id = key.key("").id;
    let token = futures::executor::block_on(async {
        let keyring = oo7::Keyring::new().await.ok()?;
        keyring.unlock().await.ok()?;
        let items = keyring.search_items(&vec![(TOKEN_ATTRIBUTE, id.as_str())]).await.ok()?;
        let item = items.first()?;
        item.unlock().await.ok()?;
        String::from_utf8(item.secret().await.ok()?.to_vec()).ok()
    })?;
    Some((key, token))
}

const TOKEN_LABEL: &str = "mandate device token";
/// The device key's ID, so that each data dir finds its own token.
const TOKEN_ATTRIBUTE: &str = "mandate-device-key";

/// What a launch asks of the app: a pairing link, a waiting item, or just
/// to show.
pub enum Opened {
    Link(String),
    Item(String),
    Show,
}

impl Opened {
    /// From the command line: a `mandate://` link, or `--item ID`.
    pub fn from_args(args: &[String]) -> Opened {
        match args {
            [flag, id, ..] if flag == "--item" => Opened::Item(id.clone()),
            [link, ..] if link.starts_with("mandate:") => Opened::Link(link.clone()),
            _ => Opened::Show,
        }
    }

    fn line(&self) -> String {
        match self {
            Opened::Link(l) => format!("link {l}\n"),
            Opened::Item(id) => format!("item {id}\n"),
            Opened::Show => "show\n".into(),
        }
    }

    fn parse(line: &str) -> Opened {
        match line.trim_end().split_once(' ') {
            Some(("link", l)) => Opened::Link(l.into()),
            Some(("item", id)) => Opened::Item(id.into()),
            _ => Opened::Show,
        }
    }
}

/// The socket of the window open on dir.
fn socket(dir: &Path) -> PathBuf {
    dir.join("app.sock")
}

/// Hands opened to the app already open on dir; false if none is.
pub fn forward(dir: &Path, opened: &Opened) -> bool {
    UnixStream::connect(socket(dir)).and_then(|mut s| s.write_all(opened.line().as_bytes())).is_ok()
}

/// Takes what later launches forward, for as long as the process runs.
pub fn listen(dir: &Path) -> futures::channel::mpsc::UnboundedReceiver<Opened> {
    let (tx, rx) = futures::channel::mpsc::unbounded();
    let path = socket(dir);
    let _ = std::fs::create_dir_all(dir);
    // Nothing answered forward, so a socket left here is stale.
    let _ = std::fs::remove_file(&path);
    match UnixListener::bind(&path) {
        Ok(listener) => {
            std::thread::spawn(move || {
                for stream in listener.incoming().flatten() {
                    let mut line = String::new();
                    if BufReader::new(stream).read_line(&mut line).is_ok() {
                        let _ = tx.unbounded_send(Opened::parse(&line));
                    }
                }
            });
        }
        Err(e) => eprintln!("mandate: {}: {e}", path.display()),
    }
    rx
}

/// `mandate-app --background`: holds the owner stream and posts a
/// notification for each waiting item, naming only its identity, connector
/// and kind; a click opens the app on the item. One runs per data dir.
pub fn background(dir: &Path) -> Result<(), Box<dyn std::error::Error>> {
    std::fs::create_dir_all(dir)?;
    let lock = std::fs::File::create(dir.join("background.lock"))?;
    if lock.try_lock().is_err() {
        return Ok(()); // the autostart entry and the user unit both start one
    }
    let bus = zbus::blocking::Connection::session()?;
    let notifications = notifications(&bus)?;
    // The notification IDs of the items shown, which a click names.
    let shown: Arc<Mutex<HashMap<String, u32>>> = Arc::default();
    let clicks = (notifications.receive_signal("ActionInvoked")?, shown.clone());
    std::thread::spawn(move || {
        let (signals, shown) = clicks;
        for signal in signals {
            let Ok((id, _)) = signal.body().deserialize::<(u32, String)>() else { continue };
            let item =
                shown.lock().unwrap().iter().find(|(_, n)| **n == id).map(|(i, _)| i.clone());
            if let Some(item) = item {
                let exe = std::env::current_exe().unwrap_or_else(|_| "mandate-app".into());
                let _ = std::process::Command::new(exe).args(["--item", &item]).spawn();
            }
        }
    });
    loop {
        // Loaded on each connect, in case the app has paired anew.
        if let Some(device) = Device::load(dir) {
            match device.client.events() {
                Ok(events) => {
                    notify(&notifications, &device, &shown);
                    for event in events {
                        if event.is_err() {
                            break;
                        }
                        notify(&notifications, &device, &shown);
                    }
                }
                Err(e) => eprintln!("mandate: stream: {e}"),
            }
        }
        std::thread::sleep(Duration::from_secs(5));
    }
}

fn notifications(bus: &zbus::blocking::Connection) -> zbus::Result<zbus::blocking::Proxy<'static>> {
    zbus::blocking::Proxy::new(
        bus,
        "org.freedesktop.Notifications",
        "/org/freedesktop/Notifications",
        "org.freedesktop.Notifications",
    )
}

/// Shows what newly waits and closes what no longer does.
fn notify(proxy: &zbus::blocking::Proxy, device: &Device, shown: &Mutex<HashMap<String, u32>>) {
    let waiting = match crate::waiting(&device.client) {
        Ok(w) => w,
        Err(e) => return eprintln!("mandate: {e}"),
    };
    let mut shown = shown.lock().unwrap();
    shown.retain(|item, id| {
        let still = waiting.iter().any(|(w, _)| w == item);
        if !still {
            let _ = proxy.call::<_, _, ()>("CloseNotification", &(*id,));
        }
        still
    });
    for (item, text) in waiting {
        if shown.contains_key(&item) {
            continue;
        }
        let hints = HashMap::from([
            ("desktop-entry", zbus::zvariant::Value::from(APP_ID)),
            ("urgency", zbus::zvariant::Value::U8(1)),
        ]);
        let body =
            ("mandate", 0u32, APP_ID, text.as_str(), "", vec!["default", "Open"], hints, -1i32);
        match proxy.call::<_, _, u32>("Notify", &body) {
            Ok(id) => {
                shown.insert(item, id);
            }
            Err(e) => eprintln!("mandate: notification: {e}"),
        }
    }
}
