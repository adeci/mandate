//! Envelopes, their challenge, and device keys that sign it (records.md).

use crate::record::{Key, KeyType, Record, Time, b64};
use crate::{Error, invalid, random, sha256};
use p256::ecdsa::signature::Verifier;
use p256::ecdsa::{Signature, VerifyingKey};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

/// Record bytes and a device's signature over them. A challenge from the
/// server comes without `key` and `sig`.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Envelope {
    pub kind: String,
    #[serde(with = "b64")]
    pub record: Vec<u8>,
    #[serde(with = "b64")]
    pub nonce: Vec<u8>,
    pub issued_at: Time,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub key: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty", with = "b64")]
    pub authenticator_data: Vec<u8>,
    #[serde(default, skip_serializing_if = "Vec::is_empty", with = "b64")]
    pub sig: Vec<u8>,
}

/// A device's signing key: a platform keystore's p256 key or a FIDO2
/// credential. Implementations ask the owner to authenticate inside `sign`.
pub trait DeviceKey {
    /// The key as the key log holds it, under the device's name.
    fn key(&self, name: &str) -> Key;
    /// Signs an envelope's challenge as records.md says for the key's type.
    fn sign(&self, challenge: &[u8; 32]) -> Result<DeviceSignature, Error>;
}

/// A device key's answer to a challenge.
pub struct DeviceSignature {
    /// r||s.
    pub sig: [u8; 64],
    /// For a fido2 key, the authenticator data it signed before the
    /// challenge; empty for a p256 key.
    pub authenticator_data: Vec<u8>,
}

/// r||s from an ASN.1 DER ECDSA-P256 signature, as keystores answer.
pub fn der_signature(der: &[u8]) -> Result<[u8; 64], Error> {
    let sig =
        Signature::from_der(der).map_err(|_| Error::Key("not a DER P-256 signature".into()))?;
    Ok(sig.to_bytes().into())
}

/// A p256 device key held in memory, for the tests.
#[cfg(test)]
pub struct SoftwareKey(p256::ecdsa::SigningKey);

#[cfg(test)]
impl SoftwareKey {
    pub fn generate() -> SoftwareKey {
        loop {
            if let Ok(k) = p256::ecdsa::SigningKey::from_slice(&random::<32>()) {
                return SoftwareKey(k);
            }
        }
    }

    /// The key with this private scalar.
    pub fn from_bytes(d: &[u8; 32]) -> Result<SoftwareKey, Error> {
        p256::ecdsa::SigningKey::from_slice(d)
            .map(SoftwareKey)
            .map_err(|_| Error::Key("not a P-256 private scalar".into()))
    }

    pub fn public_key(&self) -> [u8; 65] {
        let p = self.0.verifying_key().to_sec1_point(false);
        p.as_bytes().try_into().expect("an uncompressed point is 65 bytes")
    }

    /// ECDSA-P256 with SHA-256 over msg, as r||s; RFC 6979, so the same
    /// message gets the same signature.
    pub fn sign_msg(&self, msg: &[u8]) -> [u8; 64] {
        use p256::ecdsa::signature::Signer;
        let sig: Signature = self.0.sign(msg);
        sig.to_bytes().into()
    }
}

#[cfg(test)]
impl DeviceKey for SoftwareKey {
    fn key(&self, name: &str) -> Key {
        Key::p256(&self.public_key(), name)
    }

    fn sign(&self, challenge: &[u8; 32]) -> Result<DeviceSignature, Error> {
        Ok(DeviceSignature { sig: self.sign_msg(challenge), authenticator_data: Vec::new() })
    }
}

/// H hashes each part as its 8-byte big-endian length, then the part.
pub(crate) fn h(parts: &[&[u8]]) -> [u8; 32] {
    let mut d = Sha256::new();
    for p in parts {
        d.update((p.len() as u64).to_be_bytes());
        d.update(p);
    }
    d.finalize().into()
}

impl Envelope {
    /// The record as an unsigned envelope issued at now, with a fresh nonce.
    pub fn new(r: &Record, now: Time) -> Result<Envelope, Error> {
        Ok(Envelope {
            kind: r.kind().into(),
            record: r.encode()?,
            nonce: random::<16>().to_vec(),
            issued_at: now,
            key: String::new(),
            authenticator_data: Vec::new(),
            sig: Vec::new(),
        })
    }

    /// What the device signs; for a fido2 key, the clientDataHash.
    pub fn challenge(&self) -> [u8; 32] {
        let issued_at = self.issued_at.to_string();
        h(&[
            b"mandate-record-v1",
            self.kind.as_bytes(),
            &self.record,
            &self.nonce,
            issued_at.as_bytes(),
        ])
    }

    /// The record, decoded strictly, to show the owner before signing.
    pub fn record(&self) -> Result<Record, Error> {
        Record::decode(&self.kind, &self.record)
    }

    /// Signs the envelope with a device key.
    pub fn sign(&mut self, key: &dyn DeviceKey) -> Result<(), Error> {
        let signed = key.sign(&self.challenge())?;
        self.sig = signed.sig.to_vec();
        self.authenticator_data = signed.authenticator_data;
        self.key = key.key("").id;
        Ok(())
    }

    pub(crate) fn verify_sig(&self, k: &Key, vk: &VerifyingKey) -> Result<(), Error> {
        const UP: u8 = 0x01;
        const UV: u8 = 0x04;
        let sig = Signature::from_slice(&self.sig).map_err(|_| Error::BadSignature)?;
        let challenge = self.challenge();
        let ad = &self.authenticator_data;
        let verified = match k.kind {
            KeyType::P256 if ad.is_empty() => vk.verify(&challenge, &sig),
            KeyType::P256 => return Err(invalid("authenticator_data with a p256 key")),
            // rp ID hash, flags, sign count
            KeyType::Fido2 if ad.len() < 37 || ad[..32] != sha256(b"mandate") => {
                return Err(invalid("authenticator_data is not for rp ID mandate"));
            }
            KeyType::Fido2 if ad[32] & (UP | UV) != UP | UV => {
                return Err(invalid("authenticator_data lacks user presence or verification"));
            }
            KeyType::Fido2 => vk.verify(&[ad.as_slice(), &challenge].concat(), &sig),
        };
        verified.map_err(|_| Error::BadSignature)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn der_signature_is_r_s() {
        let key = SoftwareKey::generate();
        let raw = key.sign_msg(b"msg");
        let der = Signature::from_slice(&raw).unwrap().to_der();
        assert_eq!(der_signature(der.as_bytes()).unwrap(), raw);
        assert!(der_signature(&raw).is_err());
    }
}
