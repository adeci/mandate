//! The code exchange of agent-api.md, as the new party: CPace keyed by a
//! 6-digit code, key confirmation, and the payload sealed under the key.

use crate::record::{Key, Op, Record, is_hash};
use crate::{Error, hex, invalid, jcs, random, sha256};
use aes_gcm::Aes256Gcm;
use aes_gcm::aead::{Aead, KeyInit, Payload as Message};
use curve25519_dalek::ristretto::{CompressedRistretto, RistrettoPoint};
use curve25519_dalek::scalar::Scalar;
use curve25519_dalek::traits::IsIdentity;
use hmac::{Hmac, Mac};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256, Sha512};

/// A code as the server draws it, for the tests.
#[cfg(test)]
pub(crate) fn new_code() -> String {
    loop {
        let v = u32::from_be_bytes(random());
        // Values from 4294 * 10^6 up would favour the low codes.
        if v < 4_294_000_000 {
            return format!("{:06}", v % 1_000_000);
        }
    }
}

/// A bearer token, a device's or an agent's: 32 random bytes in hex.
pub fn new_token() -> String {
    hex(&random::<32>())
}

/// The hash of a token as the server keeps it.
pub(crate) fn token_sha256(token: &str) -> String {
    hex(&sha256(token.as_bytes()))
}

/// What the new party seals: a device's key and the hash of its token, or an
/// agent's hash alone.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Payload {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub key: Option<Key>,
    pub token_sha256: String,
}

impl Payload {
    /// Whether r lets this payload's party in: the identity with an agent's
    /// token, or the add entry for a device's key. The owner's device checks
    /// this before it signs.
    pub fn matches(&self, r: &Record) -> bool {
        match (r, &self.key) {
            (Record::Identity(i), None) => i.token_sha256 == self.token_sha256,
            (Record::Key(e), Some(k)) => e.op == Op::Add && e.key == *k,
            _ => false,
        }
    }

    fn channel(&self) -> &'static [u8] {
        match self.key {
            Some(_) => b"mandate join device",
            None => b"mandate join agent",
        }
    }
}

// CPace as in draft-irtf-cfrg-cpace-21, suite CPACE-RISTR255-SHA512.

const DSI: &[u8] = b"CPaceRistretto255";

/// lv_cat prepends each part with its LEB128 length.
fn lv_cat(parts: &[&[u8]]) -> Vec<u8> {
    let mut b = Vec::new();
    for p in parts {
        let mut n = p.len();
        while n >= 0x80 {
            b.push(n as u8 | 0x80);
            n >>= 7;
        }
        b.push(n as u8);
        b.extend_from_slice(p);
    }
    b
}

fn generator(prs: &[u8], ci: &[u8], sid: &[u8]) -> RistrettoPoint {
    // The padding fills the first hash block with DSI and PRS alone, so the
    // work done on the password does not depend on its length.
    let zpad = 127usize.saturating_sub(lv_cat(&[prs]).len() + lv_cat(&[DSI]).len());
    let h = Sha512::digest(lv_cat(&[DSI, prs, &vec![0; zpad], ci, sid]));
    RistrettoPoint::from_uniform_bytes(&h.into())
}

fn share(y: &Scalar, g: &RistrettoPoint) -> [u8; 32] {
    (y * g).compress().to_bytes()
}

/// secret is scalar_mult_vfy, failing where the draft returns the identity.
fn secret(y: &Scalar, peer: &[u8]) -> Result<[u8; 32], Error> {
    let p = CompressedRistretto::from_slice(peer).ok().and_then(|c| c.decompress());
    match p.map(|p| y * p) {
        Some(k) if !k.is_identity() => Ok(k.compress().to_bytes()),
        _ => Err(invalid("code exchange: the server's message is not a valid point")),
    }
}

/// derive_isk hashes lv_cat(DSI_ISK, sid, K) || transcript_ir(Ya, ADa, Yb,
/// ADb), which is one lv_cat over all seven parts.
fn derive_isk(sid: &[u8], k: &[u8], ya: &[u8], ada: &[u8], yb: &[u8], adb: &[u8]) -> [u8; 64] {
    Sha512::digest(lv_cat(&[b"CPaceRistretto255_ISK", sid, k, ya, ada, yb, adb])).into()
}

fn mac(isk: &[u8], label: &str, extra: &[u8]) -> Hmac<Sha256> {
    let mut m = <Hmac<Sha256> as KeyInit>::new_from_slice(isk).expect("HMAC takes any key");
    m.update(label.as_bytes());
    m.update(extra);
    m
}

fn party_confirm(isk: &[u8]) -> [u8; 32] {
    mac(isk, "mandate exchange party", &[]).finalize().into_bytes().into()
}

fn sealer(isk: &[u8]) -> Aes256Gcm {
    let key: [u8; 32] = mac(isk, "mandate exchange seal", &[]).finalize().into_bytes().into();
    Aes256Gcm::new(&key.into())
}

/// Each ISK, and so each key, seals one message, so the fixed nonce is safe.
fn seal(isk: &[u8], id: &[u8], payload: &[u8]) -> Vec<u8> {
    let msg = Message { msg: payload, aad: id };
    sealer(isk).encrypt(&[0; 12].into(), msg).expect("AES-GCM seals short messages")
}

/// The new party's answer to the server's message.
pub(crate) struct Reply {
    pub msg: [u8; 32],
    pub confirm: [u8; 32],
    pub sealed: Vec<u8>,
    isk: [u8; 64],
}

/// Runs CPace as responder on the server's message and seals the payload.
pub(crate) fn respond(id: &[u8; 16], code: &str, ya: &[u8], p: &Payload) -> Result<Reply, Error> {
    if code.len() != 6 || !code.bytes().all(|c| c.is_ascii_digit()) {
        return Err(invalid("the code is not 6 digits"));
    }
    if !is_hash(&p.token_sha256) {
        return Err(invalid("token_sha256 is not a SHA-256 in hex"));
    }
    let payload = jcs::canonical(p)?;
    let y = Scalar::from_bytes_mod_order_wide(&random());
    let k = secret(&y, ya)?;
    let yb = share(&y, &generator(code.as_bytes(), p.channel(), id));
    let isk = derive_isk(id, &k, ya, &[], &yb, &[]);
    Ok(Reply { msg: yb, confirm: party_confirm(&isk), sealed: seal(&isk, id, &payload), isk })
}

impl Reply {
    /// Checks server_confirm against the SHA-256 of the SubjectPublicKeyInfo
    /// of the TLS key the answer came over.
    pub(crate) fn check(&self, confirm: &[u8], spki_sha256: &[u8; 32]) -> Result<(), Error> {
        mac(&self.isk, "mandate exchange server", spki_sha256)
            .verify_slice(confirm)
            .map_err(|_| Error::Confirm)
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use crate::unhex;
    use std::collections::HashMap;

    /// The unlabelled code blocks of agent-api.md, which hold the vectors,
    /// as name → bytes. Hex runs on over indented lines; quoted values are
    /// text.
    fn read_vectors() -> Vec<HashMap<String, Vec<u8>>> {
        let text = crate::spec("agent-api.md");
        let (mut blocks, mut fence, mut name) = (Vec::new(), None, "");
        for line in text.lines() {
            if let Some(lang) = line.strip_prefix("```") {
                fence = match fence {
                    Some(_) => None,
                    None => Some(lang),
                };
                if fence == Some("") {
                    blocks.push(HashMap::<String, Vec<u8>>::new());
                }
                continue;
            }
            if fence != Some("") {
                continue;
            }
            let value = match line.split_once(' ') {
                Some((n, v)) if !n.is_empty() => {
                    name = n;
                    v.trim()
                }
                _ => line.trim(),
            };
            let bytes = match value.as_bytes() {
                [b'=', ..] => continue, // the formulas
                [b'"' | b'\'', inner @ .., _] => inner.to_vec(),
                _ => unhex(value).unwrap_or_else(|| panic!("{name}: {value:?} is not hex")),
            };
            blocks.last_mut().unwrap().entry(name.into()).or_default().extend(bytes);
        }
        blocks.retain(|b| !b.is_empty());
        assert_eq!(blocks.len(), 2, "vector blocks in agent-api.md");
        blocks
    }

    fn scalar(b: &[u8]) -> Scalar {
        Scalar::from_canonical_bytes(b.try_into().unwrap()).unwrap()
    }

    #[test]
    fn cpace_vector() {
        let v = &read_vectors()[0];
        let ci = [b"mandate join ".as_slice(), &v["kind"]].concat();
        let g = generator(&v["code"], &ci, &v["id"]);
        assert_eq!(g.compress().to_bytes().to_vec(), v["g"]);
        let (ya, yb) = (scalar(&v["ya"]), scalar(&v["yb"]));
        assert_eq!(share(&ya, &g).to_vec(), v["Ya"]);
        assert_eq!(share(&yb, &g).to_vec(), v["Yb"]);
        assert_eq!(secret(&yb, &v["Ya"]).unwrap().to_vec(), v["K"]);
        assert_eq!(secret(&ya, &v["Yb"]).unwrap().to_vec(), v["K"]);
        let isk = derive_isk(&v["id"], &v["K"], &v["Ya"], &[], &v["Yb"], &[]);
        assert_eq!(isk.to_vec(), v["ISK"]);
    }

    #[test]
    fn derivation_vector() {
        let v = &read_vectors()[1];
        let isk = &v["ISK"];
        let token = String::from_utf8(v["token"].clone()).unwrap();
        let payload = Payload { key: None, token_sha256: token_sha256(&token) };
        assert_eq!(jcs::canonical(&payload).unwrap(), v["payload"]);
        let server_confirm = mac(isk, "mandate exchange server", &v["spki_sha256"]).finalize();
        let seal_key = mac(isk, "mandate exchange seal", &[]).finalize();
        assert_eq!(party_confirm(isk).to_vec(), v["party_confirm"]);
        assert_eq!(server_confirm.into_bytes().to_vec(), v["server_confirm"]);
        assert_eq!(seal_key.into_bytes().to_vec(), v["seal_key"]);
        assert_eq!(seal(isk, &v["id"], &v["payload"]), v["sealed"]);
    }

    /// The server's side of one attempt, as the owner side runs it: Ya for
    /// the code, then the payload and server_confirm from the new party's
    /// answer, or Error::Confirm.
    pub(crate) struct Server {
        id: [u8; 16],
        y: Scalar,
        pub msg: [u8; 32],
    }

    impl Server {
        pub(crate) fn start(kind: &str, id: [u8; 16], code: &str) -> Server {
            let y = Scalar::from_bytes_mod_order_wide(&random());
            let ci = format!("mandate join {kind}");
            Server { id, y, msg: share(&y, &generator(code.as_bytes(), ci.as_bytes(), &id)) }
        }

        pub(crate) fn finish(
            &self,
            yb: &[u8],
            confirm: &[u8],
            sealed: &[u8],
            spki_sha256: &[u8],
        ) -> Result<(Payload, [u8; 32]), Error> {
            let k = secret(&self.y, yb)?;
            let isk = derive_isk(&self.id, &k, &self.msg, &[], yb, &[]);
            if party_confirm(&isk) != confirm {
                return Err(Error::Confirm);
            }
            let msg = Message { msg: sealed, aad: &self.id };
            let payload = sealer(&isk).decrypt(&[0; 12].into(), msg).map_err(|_| Error::Confirm)?;
            let p = serde_json::from_slice(&payload).unwrap();
            Ok((
                p,
                mac(&isk, "mandate exchange server", spki_sha256).finalize().into_bytes().into(),
            ))
        }
    }

    #[test]
    fn exchange() {
        let id = [7; 16];
        let spki = sha256(b"spki");
        let p = Payload { key: None, token_sha256: token_sha256(&crate::new_token()) };
        let code = new_code();
        let s = Server::start("agent", id, &code);
        let r = respond(&id, &code, &s.msg, &p).unwrap();
        let (got, confirm) = s.finish(&r.msg, &r.confirm, &r.sealed, &spki).unwrap();
        assert_eq!(got, p);
        r.check(&confirm, &spki).unwrap();
        assert!(matches!(r.check(&confirm, &sha256(b"other")), Err(Error::Confirm)));

        let s = Server::start("agent", id, "123456");
        let r = respond(&id, "123457", &s.msg, &p).unwrap();
        assert!(matches!(s.finish(&r.msg, &r.confirm, &r.sealed, &spki), Err(Error::Confirm)));

        // The invalid points of draft-irtf-cfrg-cpace-21's ristretto255 vectors.
        let y = scalar(
            &unhex("7cd0e075fa7955ba52c02759a6c90dbbfc10e6d40aea8d283e407d88cf538a05").unwrap(),
        );
        for bad in [
            "2b3c6b8c4f3800e7aef6864025b4ed79bd599117e427c41bd47d93d654b4a51c",
            "0000000000000000000000000000000000000000000000000000000000000000",
        ] {
            assert!(secret(&y, &unhex(bad).unwrap()).is_err(), "accepted {bad}");
        }
    }
}
