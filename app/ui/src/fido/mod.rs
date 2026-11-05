//! FIDO2 security keys as device keys: the CTAP2 commands mandate needs, over
//! any transport. `hid` frames them for USB (hidraw on the desktop, Android's
//! USB host), `nfc` for NFC. A credential is for rp ID `mandate`; records.md's
//! `fido2` type.

#[cfg(target_os = "android")]
pub mod android;
pub mod hid;
#[cfg(any(target_os = "android", test))]
pub mod nfc;

use aes::cipher::{BlockDecryptMut, BlockEncryptMut, KeyIvInit};
use hmac::{Hmac, KeyInit, Mac};
use mandate_core::{DeviceSignature, Error, der_signature};
use p256::{NonZeroScalar, PublicKey};
use sha2::{Digest, Sha256};

pub const RP: &str = "mandate";

const MAKE_CREDENTIAL: u8 = 0x01;
const GET_ASSERTION: u8 = 0x02;
const GET_INFO: u8 = 0x04;
const CLIENT_PIN: u8 = 0x06;

pub const NO_CREDENTIALS: u8 = 0x2e;
const PIN_INVALID: u8 = 0x31;
const PIN_AUTH_INVALID: u8 = 0x33;
const PUAT_REQUIRED: u8 = 0x36;
const PIN_TOKEN_EXPIRED: u8 = 0x38;

pub const AGAIN: &str = "Your security key wants its PIN again.";
const NO_PIN: &str =
    "Your security key has no PIN. Set one first, e.g. with `ykman fido access change-pin`.";

/// Sends one CTAP2 command and answers its CBOR, or the authenticator's
/// status: a USB or NFC connection to one authenticator.
pub trait Transport {
    fn call(&mut self, command: u8, request: &[u8]) -> Result<Vec<u8>, Error>;
}

/// Runs a CTAP2 command: its status byte and CBOR answer.
fn cbor(dev: &mut dyn Transport, command: u8, params: Option<Cbor>) -> Result<Cbor, Ctap> {
    let mut msg = Vec::new();
    if let Some(p) = params {
        p.encode(&mut msg);
    }
    match dev.call(command, &msg)?.split_first() {
        Some((0, [])) => Ok(Cbor::Map(Vec::new())),
        Some((0, body)) => Ok(decode(body)?.0),
        Some((code, _)) => Err(Ctap::Status(*code)),
        None => Err(odd().into()),
    }
}

/// A PIN token from one authenticator.
pub struct Session {
    protocol: u8,
    token: Vec<u8>,
    /// A CTAP 2.1 authenticator takes a token's permissions away once it is
    /// used, so the token serves one operation. On Android every signature
    /// logs in afresh anyway.
    #[cfg(not(target_os = "android"))]
    pub once: bool,
}

impl Session {
    /// pinUvAuthParam over msg; protocol 1 keeps half the HMAC.
    fn authenticate(&self, msg: &[u8]) -> Vec<u8> {
        hmac(&self.token, msg)[..if self.protocol == 1 { 16 } else { 32 }].to_vec()
    }
}

/// Gets a PIN token from the authenticator, with PIN protocol 2 if it has it.
pub fn login(dev: &mut dyn Transport, pin: &str) -> Result<Session, Error> {
    let info = cbor(dev, GET_INFO, None)?;
    if info.get(4).and_then(|o| o.field("clientPin")) != Some(&Cbor::Bool(true)) {
        return Err(key_error(NO_PIN));
    }
    let protocols = info.get(6).and_then(Cbor::array).unwrap_or_default();
    let protocol = if protocols.contains(&Cbor::Int(2)) { 2 } else { 1 };
    let (shared, platform) = agree(dev, protocol)?;
    let hash = sha256(pin.as_bytes());
    let request = map([
        (1, Cbor::Int(protocol.into())),
        (2, Cbor::Int(5)),
        (3, platform),
        (6, Cbor::Bytes(shared.encrypt(&hash[..16]))),
    ]);
    let r = match cbor(dev, CLIENT_PIN, Some(request)) {
        Err(Ctap::Status(PIN_INVALID)) => return Err(wrong_pin(dev, protocol)),
        r => r?,
    };
    let token = shared.decrypt(r.get(2).and_then(Cbor::bytes).ok_or_else(odd)?)?;
    #[cfg(not(target_os = "android"))]
    let once = info
        .get(1)
        .and_then(Cbor::array)
        .is_some_and(|v| v.iter().any(|v| matches!(v, Cbor::Text(t) if t == "FIDO_2_1")));
    Ok(Session {
        protocol,
        token,
        #[cfg(not(target_os = "android"))]
        once,
    })
}

/// Makes an ES256 credential for rp `mandate` under name; the owner touches
/// the key. Answers its ID and public point.
pub fn make_credential(
    dev: &mut dyn Transport,
    session: &Session,
    name: &str,
) -> Result<(Vec<u8>, [u8; 65]), Error> {
    let hash = random::<32>();
    let rp = tmap([("id", text(RP)), ("name", text(RP))]);
    let user = tmap([
        ("id", Cbor::Bytes(random::<16>().to_vec())),
        ("name", text(name)),
        ("displayName", text(name)),
    ]);
    let es256 = tmap([("alg", Cbor::Int(-7)), ("type", text("public-key"))]);
    let r = cbor(
        dev,
        MAKE_CREDENTIAL,
        Some(map([
            (1, Cbor::Bytes(hash.to_vec())),
            (2, rp),
            (3, user),
            (4, Cbor::Array(vec![es256])),
            (8, Cbor::Bytes(session.authenticate(&hash))),
            (9, Cbor::Int(session.protocol.into())),
        ])),
    )?;
    attested(r.get(2).and_then(Cbor::bytes).ok_or_else(odd)?)
}

/// Whether the authenticator holds the credential: an assertion without
/// touch or PIN fails with NO_CREDENTIALS on any other.
#[cfg(not(target_os = "android"))]
pub fn probe(dev: &mut dyn Transport, credential_id: &[u8]) -> Result<(), Ctap> {
    cbor(
        dev,
        GET_ASSERTION,
        Some(map([
            (1, text(RP)),
            (2, Cbor::Bytes(vec![0; 32])),
            (3, allow(credential_id)),
            (5, tmap([("up", Cbor::Bool(false))])),
        ])),
    )
    .map(drop)
}

/// Signs challenge with the credential, with a touch and the PIN token.
pub fn assert(
    dev: &mut dyn Transport,
    session: &Session,
    credential_id: &[u8],
    challenge: &[u8; 32],
) -> Result<DeviceSignature, Ctap> {
    let r = cbor(
        dev,
        GET_ASSERTION,
        Some(map([
            (1, text(RP)),
            (2, Cbor::Bytes(challenge.to_vec())),
            (3, allow(credential_id)),
            (5, tmap([("up", Cbor::Bool(true))])),
            (6, Cbor::Bytes(session.authenticate(challenge))),
            (7, Cbor::Int(session.protocol.into())),
        ])),
    )?;
    let data = r.get(2).and_then(Cbor::bytes).ok_or_else(odd)?;
    let der = r.get(3).and_then(Cbor::bytes).ok_or_else(odd)?;
    // rp ID hash, flags with UP and UV, sign count
    if data.len() < 37 || data[..32] != sha256(RP.as_bytes()) || data[32] & 0x05 != 0x05 {
        return Err(odd().into());
    }
    Ok(DeviceSignature { sig: der_signature(der)?, authenticator_data: data.to_vec() })
}

/// Whether a failed assertion means the token is spent.
#[cfg(not(target_os = "android"))]
pub fn token_spent(e: &Ctap) -> bool {
    matches!(e, Ctap::Status(PIN_AUTH_INVALID | PUAT_REQUIRED | PIN_TOKEN_EXPIRED))
}

fn allow(credential_id: &[u8]) -> Cbor {
    let id = Cbor::Bytes(credential_id.to_vec());
    Cbor::Array(vec![tmap([("id", id), ("type", text("public-key"))])])
}

fn wrong_pin(dev: &mut dyn Transport, protocol: u8) -> Error {
    let retries = map([(1, Cbor::Int(protocol.into())), (2, Cbor::Int(1))]);
    let left = cbor(dev, CLIENT_PIN, Some(retries)).ok().and_then(|r| r.get(3).and_then(Cbor::int));
    match left {
        Some(1) => key_error("Wrong PIN. 1 try left."),
        Some(n) => Error::Key(format!("Wrong PIN. {n} tries left.")),
        None => key_error("Wrong PIN."),
    }
}

/// ECDH with the authenticator's key agreement key: the shared key and the
/// platform's COSE_Key to send along.
fn agree(dev: &mut dyn Transport, protocol: u8) -> Result<(Shared, Cbor), Error> {
    let r = cbor(dev, CLIENT_PIN, Some(map([(1, Cbor::Int(protocol.into())), (2, Cbor::Int(2))])))?;
    let peer = cose_point(r.get(1).ok_or_else(odd)?)?;
    let peer = PublicKey::from_sec1_bytes(&peer).map_err(|_| odd())?;
    let secret = loop {
        if let Ok(s) = NonZeroScalar::try_from(&random::<32>()[..]) {
            break s;
        }
    };
    let shared = PublicKey::from_affine((peer.to_projective() * *secret).to_affine())
        .map_err(|_| odd())?
        .to_sec1_bytes();
    let z = &shared[1..33];
    let key = if protocol == 1 { sha256(z) } else { hkdf(z, b"CTAP2 AES key") };
    let own = PublicKey::from_secret_scalar(&secret).to_sec1_bytes();
    let cose = map([
        (1, Cbor::Int(2)),
        (3, Cbor::Int(-25)),
        (-1, Cbor::Int(1)),
        (-2, Cbor::Bytes(own[1..33].to_vec())),
        (-3, Cbor::Bytes(own[33..65].to_vec())),
    ]);
    Ok((Shared { protocol, key }, cose))
}

/// 0x04||x||y from a COSE_Key for a P-256 point.
fn cose_point(key: &Cbor) -> Result<[u8; 65], Error> {
    let coord = move |label| key.get(label).and_then(Cbor::bytes).filter(|b| b.len() == 32);
    match (key.get(1).and_then(Cbor::int), key.get(-1).and_then(Cbor::int), coord(-2), coord(-3)) {
        (Some(2), Some(1), Some(x), Some(y)) => {
            let mut p = [4; 65];
            p[1..33].copy_from_slice(x);
            p[33..].copy_from_slice(y);
            Ok(p)
        }
        _ => Err(odd()),
    }
}

/// The credential ID and public point in makeCredential's authenticator data:
/// rp ID hash, flags, sign count, AAGUID, ID length, ID, COSE_Key.
fn attested(data: &[u8]) -> Result<(Vec<u8>, [u8; 65]), Error> {
    if data.len() < 55 || data[32] & 0x40 == 0 {
        return Err(odd());
    }
    let end = 55 + usize::from(u16::from_be_bytes([data[53], data[54]]));
    let id = data.get(55..end).ok_or_else(odd)?;
    let (key, _) = decode(&data[end..])?;
    if key.get(3).and_then(Cbor::int) != Some(-7) {
        return Err(odd());
    }
    Ok((id.to_vec(), cose_point(&key)?))
}

/// A PIN protocol's AES key, agreed with the authenticator.
struct Shared {
    protocol: u8,
    key: [u8; 32],
}

impl Shared {
    /// AES-256-CBC: protocol 1 with a zero IV, protocol 2 with a random IV in
    /// front.
    fn encrypt(&self, data: &[u8]) -> Vec<u8> {
        let iv = if self.protocol == 1 { [0; 16] } else { random() };
        let mut out = data.to_vec();
        let mut aes = cbc::Encryptor::<aes::Aes256>::new(&self.key.into(), &iv.into());
        for block in out.as_chunks_mut::<16>().0 {
            aes.encrypt_block_mut(aes::Block::from_mut_slice(block));
        }
        if self.protocol == 1 { out } else { [&iv[..], &out[..]].concat() }
    }

    fn decrypt(&self, data: &[u8]) -> Result<Vec<u8>, Error> {
        let zero = [0; 16];
        let (iv, data) = if self.protocol == 1 {
            (&zero[..], data)
        } else {
            data.split_at_checked(16).ok_or_else(odd)?
        };
        if data.len() % 16 != 0 {
            return Err(odd());
        }
        let mut out = data.to_vec();
        let mut aes = cbc::Decryptor::<aes::Aes256>::new_from_slices(&self.key, iv)
            .expect("a 32-byte key and a 16-byte IV");
        for block in out.as_chunks_mut::<16>().0 {
            aes.decrypt_block_mut(aes::Block::from_mut_slice(block));
        }
        Ok(out)
    }
}

/// An error from the authenticator: a CTAP2 status, or anything else.
pub enum Ctap {
    Status(u8),
    Other(Error),
}

impl From<Error> for Ctap {
    fn from(e: Error) -> Ctap {
        Ctap::Other(e)
    }
}

impl From<Ctap> for Error {
    fn from(c: Ctap) -> Error {
        match c {
            Ctap::Status(code) => status(code),
            Ctap::Other(e) => e,
        }
    }
}

/// A plain message for a CTAP2 status.
fn status(code: u8) -> Error {
    key_error(match code {
        // OPERATION_DENIED, KEEPALIVE_CANCEL, USER_ACTION_TIMEOUT
        0x27 | 0x2d | 0x2f => "No touch, so nothing was signed.",
        NO_CREDENTIALS => "Wrong security key. Use the one this device was set up with.",
        PIN_INVALID => "Wrong PIN.",
        0x32 => "Your security key's PIN is blocked. Reset the key.",
        0x34 => "Too many wrong PINs. Take the key away and try again.",
        0x35 => NO_PIN,
        0x37 => "Your security key wants a new PIN, e.g. with `ykman fido access change-pin`.",
        PIN_AUTH_INVALID | PUAT_REQUIRED | PIN_TOKEN_EXPIRED => AGAIN,
        _ => return unknown(code),
    })
}

pub fn unknown(code: u8) -> Error {
    Error::Key(format!("The security key answered error 0x{code:02x}."))
}

pub fn odd() -> Error {
    key_error("The security key gave an answer the app doesn't understand.")
}

pub fn key_error(msg: &str) -> Error {
    Error::Key(msg.into())
}

/// A CBOR value, as far as CTAP2 uses them.
#[derive(Debug, PartialEq)]
pub enum Cbor {
    Int(i64),
    Bytes(Vec<u8>),
    Text(String),
    Array(Vec<Cbor>),
    Map(Vec<(Cbor, Cbor)>),
    Bool(bool),
}

impl Cbor {
    /// Encodes in CTAP2 canonical form.
    fn encode(&self, out: &mut Vec<u8>) {
        match self {
            Cbor::Int(i) if *i >= 0 => head(out, 0, *i as u64),
            Cbor::Int(i) => head(out, 1, !*i as u64),
            Cbor::Bytes(b) => {
                head(out, 2, b.len() as u64);
                out.extend_from_slice(b);
            }
            Cbor::Text(t) => {
                head(out, 3, t.len() as u64);
                out.extend_from_slice(t.as_bytes());
            }
            Cbor::Array(a) => {
                head(out, 4, a.len() as u64);
                for v in a {
                    v.encode(out);
                }
            }
            Cbor::Map(m) => {
                let mut pairs: Vec<(Vec<u8>, &Cbor)> = m
                    .iter()
                    .map(|(k, v)| {
                        let mut key = Vec::new();
                        k.encode(&mut key);
                        (key, v)
                    })
                    .collect();
                // Major type first, then the shorter key, then bytewise.
                pairs.sort_by(|(a, _), (b, _)| {
                    (a[0] >> 5, a.len(), a).cmp(&(b[0] >> 5, b.len(), b))
                });
                head(out, 5, m.len() as u64);
                for (key, v) in pairs {
                    out.extend(key);
                    v.encode(out);
                }
            }
            Cbor::Bool(b) => out.push(if *b { 0xf5 } else { 0xf4 }),
        }
    }

    fn get(&self, key: i64) -> Option<&Cbor> {
        self.find(|k| *k == Cbor::Int(key))
    }

    fn field(&self, name: &str) -> Option<&Cbor> {
        self.find(|k| matches!(k, Cbor::Text(t) if t == name))
    }

    fn find(&self, key: impl Fn(&Cbor) -> bool) -> Option<&Cbor> {
        match self {
            Cbor::Map(m) => m.iter().find(|(k, _)| key(k)).map(|(_, v)| v),
            _ => None,
        }
    }

    fn int(&self) -> Option<i64> {
        if let Cbor::Int(i) = self { Some(*i) } else { None }
    }

    fn bytes(&self) -> Option<&[u8]> {
        if let Cbor::Bytes(b) = self { Some(b) } else { None }
    }

    fn array(&self) -> Option<&[Cbor]> {
        if let Cbor::Array(a) = self { Some(a) } else { None }
    }
}

/// A CBOR head: major type and argument, in the shortest form.
fn head(out: &mut Vec<u8>, major: u8, n: u64) {
    let major = major << 5;
    let len: usize = match n {
        0..24 => return out.push(major | n as u8),
        24..0x100 => 1,
        0x100..0x1_0000 => 2,
        0x1_0000..0x1_0000_0000 => 4,
        _ => 8,
    };
    out.push(major | (24 + len.ilog2() as u8));
    out.extend_from_slice(&n.to_be_bytes()[8 - len..]);
}

/// Decodes one CBOR item from the front of data, and how many bytes it took.
fn decode(data: &[u8]) -> Result<(Cbor, usize), Error> {
    let mut r = Reader { data, pos: 0 };
    let item = r.item(0)?;
    Ok((item, r.pos))
}

struct Reader<'a> {
    data: &'a [u8],
    pos: usize,
}

impl<'a> Reader<'a> {
    fn take(&mut self, n: u64) -> Result<&'a [u8], Error> {
        let data = self.data;
        let end = usize::try_from(n)
            .ok()
            .and_then(|n| self.pos.checked_add(n))
            .filter(|&end| end <= data.len())
            .ok_or_else(odd)?;
        let taken = &data[self.pos..end];
        self.pos = end;
        Ok(taken)
    }

    fn item(&mut self, depth: u8) -> Result<Cbor, Error> {
        if depth > 16 {
            return Err(odd());
        }
        let first = self.take(1)?[0];
        let (major, info) = (first >> 5, first & 0x1f);
        let n = match info {
            0..24 => u64::from(info),
            24..28 => self.take(1 << (info - 24))?.iter().fold(0u64, |n, &b| n << 8 | u64::from(b)),
            _ => return Err(odd()),
        };
        match major {
            0 => i64::try_from(n).map(Cbor::Int).map_err(|_| odd()),
            1 => i64::try_from(n).map(|n| Cbor::Int(-1 - n)).map_err(|_| odd()),
            2 => Ok(Cbor::Bytes(self.take(n)?.to_vec())),
            3 => String::from_utf8(self.take(n)?.to_vec()).map(Cbor::Text).map_err(|_| odd()),
            4 => (0..n).map(|_| self.item(depth + 1)).collect::<Result<_, _>>().map(Cbor::Array),
            5 => (0..n)
                .map(|_| -> Result<_, Error> { Ok((self.item(depth + 1)?, self.item(depth + 1)?)) })
                .collect::<Result<_, _>>()
                .map(Cbor::Map),
            7 if info == 20 || info == 21 => Ok(Cbor::Bool(info == 21)),
            // Tags, floats and the rest CTAP2 doesn't send.
            _ => Err(odd()),
        }
    }
}

fn map<const N: usize>(pairs: [(i64, Cbor); N]) -> Cbor {
    Cbor::Map(pairs.into_iter().map(|(k, v)| (Cbor::Int(k), v)).collect())
}

fn tmap<const N: usize>(pairs: [(&str, Cbor); N]) -> Cbor {
    Cbor::Map(pairs.into_iter().map(|(k, v)| (text(k), v)).collect())
}

fn text(s: &str) -> Cbor {
    Cbor::Text(s.into())
}

pub fn random<const N: usize>() -> [u8; N] {
    let mut b = [0; N];
    getrandom::fill(&mut b).expect("the OS gives randomness");
    b
}

fn sha256(b: &[u8]) -> [u8; 32] {
    Sha256::digest(b).into()
}

fn hmac(key: &[u8], msg: &[u8]) -> [u8; 32] {
    let mut mac = <Hmac<Sha256> as KeyInit>::new_from_slice(key).expect("HMAC takes any key");
    mac.update(msg);
    mac.finalize().into_bytes().into()
}

/// One block of HKDF-SHA-256 with a zero salt, as PIN protocol 2 derives keys.
fn hkdf(ikm: &[u8], info: &[u8]) -> [u8; 32] {
    let prk = hmac(&[0; 32], ikm);
    hmac(&prk, &[info, &[1][..]].concat())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cbor_is_canonical_and_round_trips() {
        let m = Cbor::Map(vec![
            (text("a"), Cbor::Bool(true)),
            (Cbor::Int(-1), Cbor::Bool(false)),
            (Cbor::Int(24), Cbor::Bytes(vec![1])),
        ]);
        let mut b = Vec::new();
        m.encode(&mut b);
        // 24 takes two bytes yet sorts before -1: integers by major type first.
        assert_eq!(b, [0xa3, 0x18, 0x18, 0x41, 0x01, 0x20, 0xf4, 0x61, b'a', 0xf5]);
        let (back, used) = decode(&b).unwrap();
        assert_eq!(used, b.len());
        let Cbor::Map(pairs) = back else { panic!("not a map") };
        assert_eq!(pairs[0], (Cbor::Int(24), Cbor::Bytes(vec![1])));
        assert_eq!(pairs[1], (Cbor::Int(-1), Cbor::Bool(false)));
        assert_eq!(pairs[2], (text("a"), Cbor::Bool(true)));
    }

    #[test]
    fn attested_credential() {
        let cose = map([
            (1, Cbor::Int(2)),
            (3, Cbor::Int(-7)),
            (-1, Cbor::Int(1)),
            (-2, Cbor::Bytes(vec![1; 32])),
            (-3, Cbor::Bytes(vec![2; 32])),
        ]);
        let mut data = vec![0; 32];
        data.push(0x45);
        data.extend([0; 4 + 16]);
        data.extend([0, 2, 7, 8]);
        cose.encode(&mut data);
        let (id, point) = attested(&data).unwrap();
        let mut want = [4; 65];
        want[1..33].fill(1);
        want[33..].fill(2);
        assert_eq!((id, point), (vec![7, 8], want));
    }

    #[test]
    fn hkdf_matches_rfc_5869() {
        // Test case 3: an empty salt is 32 zero bytes; empty info; the first block.
        let okm: String = hkdf(&[0x0b; 22], b"").iter().map(|b| format!("{b:02x}")).collect();
        assert_eq!(okm, "8da4e775a563c18f715f802a063c5a31b8a11f5c5ee1879ec3454e5f3c738d2d");
    }

    #[test]
    fn pin_protocols_round_trip() {
        for protocol in [1, 2] {
            let shared = Shared { protocol, key: [9; 32] };
            let sealed = shared.encrypt(&[7; 32]);
            assert_eq!(sealed.len(), if protocol == 1 { 32 } else { 48 });
            assert_eq!(shared.decrypt(&sealed).unwrap(), [7; 32]);
        }
    }
}
