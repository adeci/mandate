//! FIDO2 authenticators on hidraw, as device keys (crate::fido has the CTAP2
//! part).

use crate::fido::hid::{Hid, REPORT, Reports};
use crate::fido::{self, AGAIN, Ctap, NO_CREDENTIALS, Session, key_error, random};
use mandate_core::{DeviceKey, DeviceSignature, Error, Key};
use std::fs::{self, File, OpenOptions};
use std::io::{self, Read, Write};
use std::os::fd::AsRawFd;
use std::path::{Path, PathBuf};
use std::sync::Mutex;
use std::time::Instant;

const PLUG_IN: &str = "Plug in your security key.";
const GONE: &str = "The security key stopped responding. Plug it back in.";

/// A FIDO2 authenticator found on hidraw.
#[derive(Clone)]
pub struct Authenticator {
    pub path: PathBuf,
    pub name: String,
}

/// The connected FIDO2 authenticators: hidraw devices whose report descriptor
/// has usage page 0xF1D0, named by HID_NAME in their uevent.
pub fn authenticators() -> Vec<Authenticator> {
    let Ok(dir) = fs::read_dir("/sys/class/hidraw") else { return Vec::new() };
    let mut found: Vec<Authenticator> = dir
        .flatten()
        .filter_map(|e| {
            let sys = e.path().join("device");
            let descriptor = fs::read(sys.join("report_descriptor")).ok()?;
            if !descriptor.windows(3).any(|w| w == b"\x06\xd0\xf1") {
                return None;
            }
            let uevent = fs::read_to_string(sys.join("uevent")).ok()?;
            let name = uevent.lines().find_map(|l| l.strip_prefix("HID_NAME="))?.to_owned();
            Some(Authenticator { path: Path::new("/dev").join(e.file_name()), name })
        })
        .collect();
    found.sort_by(|a, b| a.path.cmp(&b.path));
    found
}

/// A fido2 device key: a credential for rp ID `mandate` on some authenticator.
pub struct Fido {
    public_key: [u8; 65],
    credential_id: Vec<u8>,
    /// The PIN token, and the authenticator that gave it.
    session: Mutex<Option<(PathBuf, Session)>>,
}

impl Fido {
    /// Gets a PIN token from the authenticator with pin, then makes an ES256
    /// credential for rp `mandate` under `name`. The owner touches the key.
    /// Keeps a token for the first signature, which pairing makes at once.
    pub fn create(a: &Authenticator, pin: &str, name: &str) -> Result<Fido, Error> {
        let mut dev = open(&a.path)?;
        let session = fido::login(&mut dev, pin)?;
        let (credential_id, public_key) = fido::make_credential(&mut dev, &session, name)?;
        // A CTAP 2.1 key took the token's permissions with the credential.
        let session = if session.once { fido::login(&mut dev, pin)? } else { session };
        Ok(Fido { public_key, credential_id, session: Mutex::new(Some((a.path.clone(), session))) })
    }

    /// A credential made before.
    pub fn new(public_key: [u8; 65], credential_id: Vec<u8>) -> Fido {
        Fido { public_key, credential_id, session: Mutex::new(None) }
    }

    pub fn public_key(&self) -> [u8; 65] {
        self.public_key
    }

    pub fn credential_id(&self) -> &[u8] {
        &self.credential_id
    }

    /// Whether signing needs `unlock` first: no PIN token held for this process.
    pub fn locked(&self) -> bool {
        self.session.lock().unwrap().is_none()
    }

    /// Finds the connected authenticator holding the credential and gets a
    /// PIN token from it.
    pub fn unlock(&self, pin: &str) -> Result<(), Error> {
        let mut miss = None;
        for a in authenticators() {
            let mut dev = match open(&a.path) {
                Ok(dev) => dev,
                Err(e) => {
                    miss.get_or_insert(Ctap::Other(e));
                    continue;
                }
            };
            match fido::probe(&mut dev, &self.credential_id) {
                Ok(()) => {
                    let session = fido::login(&mut dev, pin)?;
                    *self.session.lock().unwrap() = Some((a.path, session));
                    return Ok(());
                }
                // Another key: say so only if nothing better went wrong.
                Err(Ctap::Status(NO_CREDENTIALS)) if miss.is_some() => {}
                Err(e) => miss = Some(e),
            }
        }
        Err(miss.map_or_else(|| key_error(PLUG_IN), Error::from))
    }

    /// Proves the owner is here, signing nothing the server sees: an
    /// assertion over random bytes, with a touch and the PIN token, which pin
    /// gets first when none is held.
    pub fn confirm(&self, pin: Option<&str>) -> Result<(), Error> {
        if let Some(pin) = pin {
            self.unlock(pin)?;
        }
        self.sign(&random::<32>()).map(drop)
    }
}

impl DeviceKey for Fido {
    fn key(&self, name: &str) -> Key {
        Key::fido2(&self.public_key, &self.credential_id, name)
    }

    /// Blocks until the owner touches the key; needs the token from `create`
    /// or `unlock`.
    fn sign(&self, challenge: &[u8; 32]) -> Result<DeviceSignature, Error> {
        let mut held = self.session.lock().unwrap();
        let (path, session) = held.as_ref().ok_or_else(|| key_error(AGAIN))?;
        let signed = open(path)
            .map_err(Ctap::Other)
            .and_then(|mut dev| fido::assert(&mut dev, session, &self.credential_id, challenge));
        if session.once || signed.as_ref().is_err_and(fido::token_spent) {
            *held = None;
        }
        signed.map_err(Error::from)
    }
}

/// A CTAPHID channel on the hidraw device at path.
fn open(path: &Path) -> Result<Hid<Raw>, Error> {
    let file = OpenOptions::new().read(true).write(true).open(path).map_err(io_error)?;
    Hid::open(Raw(file))
}

/// A hidraw device: reports written after report ID 0.
struct Raw(File);

impl Reports for Raw {
    fn write(&mut self, report: &[u8; REPORT]) -> Result<(), Error> {
        self.0.write_all(&[&[0][..], report].concat()).map_err(io_error)
    }

    fn read(&mut self, deadline: Instant) -> Result<[u8; REPORT], Error> {
        let mut report = [0; REPORT];
        loop {
            let left = deadline.saturating_duration_since(Instant::now());
            if left.is_zero() {
                return Err(key_error("The security key didn't answer."));
            }
            let mut fd = libc::pollfd { fd: self.0.as_raw_fd(), events: libc::POLLIN, revents: 0 };
            let ready = unsafe { libc::poll(&mut fd, 1, left.as_millis().max(1) as i32) };
            if ready < 0 {
                let e = io::Error::last_os_error();
                if e.kind() != io::ErrorKind::Interrupted {
                    return Err(io_error(e));
                }
            } else if ready > 0 {
                return match self.0.read(&mut report).map_err(io_error)? {
                    REPORT => Ok(report),
                    _ => Err(key_error(GONE)),
                };
            }
        }
    }
}

fn io_error(e: io::Error) -> Error {
    key_error(match e.kind() {
        io::ErrorKind::NotFound => PLUG_IN,
        io::ErrorKind::PermissionDenied => {
            "Can't open the security key. Install the FIDO udev rules (libfido2)."
        }
        _ => GONE,
    })
}
