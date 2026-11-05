//! PKCS#11 tokens: a P-256 key pair on a token the owner's module serves
//! (ykcs11, tpm2-pkcs11, OpenSC, SoftHSM2…) as a p256 device key.

use cryptoki::context::{CInitializeArgs, CInitializeFlags, Pkcs11 as Context};
use cryptoki::error::{Error as CkError, RvError};
use cryptoki::mechanism::Mechanism;
use cryptoki::object::{Attribute, AttributeType, KeyType, ObjectClass, ObjectHandle};
use cryptoki::session::{Session, UserType};
use cryptoki::types::AuthPin;
use mandate_core::{DeviceKey, DeviceSignature, Error, Key};
use sha2::{Digest, Sha256};
use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::{LazyLock, Mutex};

/// The prime256v1 OID in DER, a P-256 key's CKA_EC_PARAMS.
const P256: [u8; 10] = [0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07];

/// One initialized context per module for the process: C_Initialize fails
/// the second time.
static CONTEXTS: LazyLock<Mutex<HashMap<PathBuf, Context>>> = LazyLock::new(Default::default);

/// A token in one of the module's slots.
#[derive(Clone)]
pub struct Token {
    pub serial: String,
    pub label: String,
}

/// A P-256 key pair on a token.
#[derive(Clone)]
pub struct Found {
    /// CKA_ID.
    pub id: Vec<u8>,
    pub label: String,
    pub public_key: [u8; 65],
}

/// The tokens present in the module's slots (initialized ones only).
pub fn tokens(module: &Path) -> Result<Vec<Token>, Error> {
    let ctx = context(module)?;
    let slots = ctx.get_slots_with_initialized_token().map_err(error)?;
    slots
        .into_iter()
        .map(|slot| -> Result<Token, Error> {
            let info = ctx.get_token_info(slot).map_err(error)?;
            Ok(Token { serial: info.serial_number().into(), label: info.label().into() })
        })
        .collect()
}

/// A logged-in user session on one token. The session holds the module's
/// shared context.
pub struct Login {
    session: Session,
    /// For keys that want the PIN again before each signature (CKA_ALWAYS_AUTHENTICATE).
    pin: AuthPin,
}

impl Login {
    /// Logs in to the token with this serial as the user with pin.
    pub fn open(module: &Path, serial: &str, pin: &str) -> Result<Login, Error> {
        let ctx = context(module)?;
        let slots = ctx.get_slots_with_initialized_token().map_err(error)?;
        let (slot, info) = slots
            .into_iter()
            .find_map(|s| {
                let info = ctx.get_token_info(s).ok()?;
                (info.serial_number() == serial).then_some((s, info))
            })
            .ok_or_else(|| Error::Key("The token isn't there. Plug it in.".into()))?;
        let session = if info.write_protected() {
            ctx.open_ro_session(slot)
        } else {
            ctx.open_rw_session(slot)
        }
        .map_err(error)?;
        let pin = AuthPin::from(pin);
        match session.login(UserType::User, Some(&pin)) {
            // Another session of this process on the token logged in already.
            Ok(()) | Err(CkError::Pkcs11(RvError::UserAlreadyLoggedIn, _)) => {}
            Err(e) => return Err(error(e)),
        }
        Ok(Login { session, pin })
    }

    /// The token's P-256 key pairs: private signing keys with a public key
    /// of the same CKA_ID.
    pub fn keys(&self) -> Result<Vec<Found>, Error> {
        let s = &self.session;
        let private = [
            Attribute::Class(ObjectClass::PRIVATE_KEY),
            Attribute::KeyType(KeyType::EC),
            Attribute::EcParams(P256.into()),
            Attribute::Sign(true),
        ];
        let mut found = Vec::new();
        for key in s.find_objects(&private).map_err(error)? {
            let (mut id, mut label) = (Vec::new(), String::new());
            for a in
                s.get_attributes(key, &[AttributeType::Id, AttributeType::Label]).map_err(error)?
            {
                match a {
                    Attribute::Id(v) => id = v,
                    Attribute::Label(v) => label = String::from_utf8_lossy(&v).into_owned(),
                    _ => {}
                }
            }
            if id.is_empty() {
                continue;
            }
            let public = [
                Attribute::Class(ObjectClass::PUBLIC_KEY),
                Attribute::KeyType(KeyType::EC),
                Attribute::Id(id.clone()),
            ];
            let mut public_key = None;
            for p in s.find_objects(&public).map_err(error)? {
                public_key = public_key.or(self.point(p)?);
            }
            if let Some(public_key) = public_key {
                found.push(Found { id, label, public_key });
            }
        }
        Ok(found)
    }

    /// Generates a P-256 key pair on the token, under a random CKA_ID.
    pub fn create(&self, label: &str) -> Result<Found, Error> {
        let mut id = vec![0; 16];
        getrandom::fill(&mut id).map_err(|e| Error::Key(e.to_string()))?;
        let public = [
            Attribute::Token(true),
            Attribute::Id(id.clone()),
            Attribute::Label(label.into()),
            Attribute::EcParams(P256.into()),
            Attribute::Verify(true),
        ];
        let private = [
            Attribute::Token(true),
            Attribute::Id(id.clone()),
            Attribute::Label(label.into()),
            Attribute::Private(true),
            Attribute::Sensitive(true),
            Attribute::Extractable(false),
            Attribute::Sign(true),
        ];
        let (p, _) = self
            .session
            .generate_key_pair(&Mechanism::EccKeyPairGen, &public, &private)
            .map_err(error)?;
        let public_key = self
            .point(p)?
            .ok_or_else(|| Error::Key("The token made a key with no P-256 point.".into()))?;
        Ok(Found { id, label: label.into(), public_key })
    }

    /// The point of the public key p, if it is a P-256 one.
    fn point(&self, p: ObjectHandle) -> Result<Option<[u8; 65]>, Error> {
        Ok(match &self.session.get_attributes(p, &[AttributeType::EcPoint]).map_err(error)?[..] {
            [Attribute::EcPoint(v)] => ec_point(v),
            _ => None,
        })
    }

    /// ECDSA over digest with the private key with this CKA_ID; None if the
    /// token no longer has it.
    fn sign(&self, id: &[u8], digest: &[u8]) -> cryptoki::error::Result<Option<Vec<u8>>> {
        let s = &self.session;
        let template = [
            Attribute::Class(ObjectClass::PRIVATE_KEY),
            Attribute::KeyType(KeyType::EC),
            Attribute::Id(id.into()),
        ];
        let Some(&key) = s.find_objects(&template)?.first() else {
            return Ok(None);
        };
        let always = matches!(
            s.get_attributes(key, &[AttributeType::AlwaysAuthenticate])?[..],
            [Attribute::AlwaysAuthenticate(true)]
        );
        if !always {
            return s.sign(&Mechanism::Ecdsa, key, digest).map(Some);
        }
        // The PIN goes between C_SignInit and the signature, so in parts.
        s.sign_init(&Mechanism::Ecdsa, key)?;
        if let Err(e) = s.login(UserType::ContextSpecific, Some(&self.pin)) {
            // C_SignFinal ends the operation, whatever it answers.
            let _ = s.sign_final();
            return Err(e);
        }
        s.sign_update(digest)?;
        s.sign_final().map(Some)
    }
}

/// A p256 device key on a PKCS#11 token.
pub struct Pkcs11 {
    module: PathBuf,
    serial: String,
    id: Vec<u8>,
    public_key: [u8; 65],
    login: Mutex<Option<Login>>,
}

impl Pkcs11 {
    /// The key, already logged in (from the chooser).
    pub fn new(module: &Path, serial: &str, found: &Found, login: Option<Login>) -> Pkcs11 {
        Pkcs11 {
            module: module.into(),
            serial: serial.into(),
            id: found.id.clone(),
            public_key: found.public_key,
            login: Mutex::new(login),
        }
    }

    pub fn module(&self) -> &Path {
        &self.module
    }

    pub fn serial(&self) -> &str {
        &self.serial
    }

    pub fn id(&self) -> &[u8] {
        &self.id
    }

    pub fn public_key(&self) -> [u8; 65] {
        self.public_key
    }

    /// Whether signing needs `unlock` first: no session logged in for this
    /// process.
    pub fn locked(&self) -> bool {
        // A signature waiting for a touch holds the lock, and a session.
        self.login.try_lock().is_ok_and(|login| login.is_none())
    }

    /// Logs in with pin and keeps the session for the process's life.
    pub fn unlock(&self, pin: &str) -> Result<(), Error> {
        let login = Login::open(&self.module, &self.serial, pin)?;
        *self.login.lock().unwrap() = Some(login);
        Ok(())
    }

    /// Checks pin on the token. The session stays logged in, so it logs out
    /// first: logged in, the token would take any PIN without checking it.
    /// A wrong PIN leaves the key locked.
    pub fn check_pin(&self, pin: &str) -> Result<(), Error> {
        let mut held = self.login.lock().unwrap();
        if let Some(login) = held.take() {
            let _ = login.session.logout();
        }
        *held = Some(Login::open(&self.module, &self.serial, pin)?);
        Ok(())
    }
}

impl DeviceKey for Pkcs11 {
    fn key(&self, name: &str) -> Key {
        Key::p256(&self.public_key, name)
    }

    /// ECDSA over SHA-256(challenge) with CKM_ECDSA, which every module has;
    /// any touch or PIN per signature is the token's own policy.
    fn sign(&self, challenge: &[u8; 32]) -> Result<DeviceSignature, Error> {
        let again = || Error::Key("Your token wants its PIN again.".into());
        let mut held = self.login.lock().unwrap();
        let login = held.as_ref().ok_or_else(again)?;
        let sig = match login.sign(&self.id, &Sha256::digest(challenge)) {
            Ok(Some(sig)) => sig,
            Ok(None) => return Err(Error::Key("The token no longer has this key.".into())),
            Err(CkError::Pkcs11(
                RvError::SessionHandleInvalid
                | RvError::SessionClosed
                | RvError::UserNotLoggedIn
                | RvError::DeviceRemoved
                | RvError::TokenNotPresent,
                _,
            )) => {
                *held = None;
                return Err(again());
            }
            Err(e) => return Err(error(e)),
        };
        // CKM_ECDSA answers r||s, not DER.
        let sig: [u8; 64] = sig.as_slice().try_into().map_err(|_| {
            Error::Key(format!("The token answered a {}-byte signature.", sig.len()))
        })?;
        Ok(DeviceSignature { sig, authenticator_data: Vec::new() })
    }
}

/// The module's context, loaded and initialized on first use.
fn context(module: &Path) -> Result<Context, Error> {
    let mut contexts = CONTEXTS.lock().unwrap();
    if let Some(ctx) = contexts.get(module) {
        return Ok(ctx.clone());
    }
    let load = |e: CkError| {
        let reason = match e {
            // dlopen's own words, without cryptoki's "libloading error (…)".
            CkError::LibraryLoading(e) => e.to_string(),
            e => e.to_string(),
        };
        Error::Key(format!("Can't load the PKCS#11 module at {}: {reason}.", module.display()))
    };
    let ctx = Context::new(module).map_err(load)?;
    ctx.initialize(CInitializeArgs::new(CInitializeFlags::OS_LOCKING_OK)).map_err(load)?;
    contexts.insert(module.into(), ctx.clone());
    Ok(ctx)
}

/// A module's error as the owner reads it.
fn error(e: CkError) -> Error {
    Error::Key(match e {
        CkError::Pkcs11(RvError::PinIncorrect, _) => "Wrong PIN.".into(),
        CkError::Pkcs11(RvError::PinLocked, _) => "The token's PIN is locked.".into(),
        CkError::Pkcs11(RvError::TokenNotPresent | RvError::DeviceRemoved, _) => {
            "The token isn't there. Plug it in.".into()
        }
        e => e.to_string(),
    })
}

/// The uncompressed point in a CKA_EC_POINT: a DER OCTET STRING around it,
/// or the bare point as some modules give it.
fn ec_point(v: &[u8]) -> Option<[u8; 65]> {
    let point = match v {
        [0x04, 0x41, rest @ ..] if rest.len() == 65 => rest,
        _ => v,
    };
    point.try_into().ok().filter(|p: &[u8; 65]| p[0] == 0x04)
}

#[cfg(test)]
mod tests {
    use super::ec_point;

    #[test]
    fn ec_points() {
        let mut point = [7; 65];
        point[0] = 0x04;
        let der = [&[0x04, 0x41][..], &point].concat();
        assert_eq!(ec_point(&der), Some(point));
        assert_eq!(ec_point(&point), Some(point));
        point[1] = 0x41;
        assert_eq!(ec_point(&point), Some(point));
        assert_eq!(ec_point(&der[..66]), None);
        assert_eq!(ec_point(&[0x02; 33]), None);
    }
}
