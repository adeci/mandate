//! Envelopes, their challenge, and device keys that sign it (records.md).

use crate::record::{Key, KeyType, Record, Time, b64};
use crate::{Error, hex, invalid, random, sha256};
use p256::ecdsa::signature::{Signer, Verifier};
use p256::ecdsa::{Signature, SigningKey, VerifyingKey};
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

/// A device's P-256 key. Platform keystores implement it, and ask the owner
/// to authenticate inside `sign`.
pub trait DeviceKey {
    /// The 65-byte uncompressed point.
    fn public_key(&self) -> [u8; 65];
    /// ECDSA-P256 with SHA-256 over msg, as r||s. Keystores that answer in DER
    /// convert with `der_signature`.
    fn sign(&self, msg: &[u8]) -> Result<[u8; 64], Error>;
}

/// r||s from an ASN.1 DER ECDSA-P256 signature, as keystores answer.
pub fn der_signature(der: &[u8]) -> Result<[u8; 64], Error> {
    let sig =
        Signature::from_der(der).map_err(|_| Error::Key("not a DER P-256 signature".into()))?;
    Ok(sig.to_bytes().into())
}

/// A device key held in memory, for tests and for the desktop until it has
/// a keystore.
pub struct SoftwareKey(SigningKey);

impl SoftwareKey {
    pub fn generate() -> SoftwareKey {
        loop {
            if let Ok(k) = SigningKey::from_slice(&random::<32>()) {
                return SoftwareKey(k);
            }
        }
    }

    /// The key with this private scalar.
    pub fn from_bytes(d: &[u8; 32]) -> Result<SoftwareKey, Error> {
        SigningKey::from_slice(d)
            .map(SoftwareKey)
            .map_err(|_| Error::Key("not a P-256 private scalar".into()))
    }

    pub fn to_bytes(&self) -> [u8; 32] {
        self.0.to_bytes().into()
    }
}

impl DeviceKey for SoftwareKey {
    fn public_key(&self) -> [u8; 65] {
        let p = self.0.verifying_key().to_sec1_point(false);
        p.as_bytes().try_into().expect("an uncompressed point is 65 bytes")
    }

    /// RFC 6979, so the same message gets the same signature.
    fn sign(&self, msg: &[u8]) -> Result<[u8; 64], Error> {
        let sig: Signature = self.0.sign(msg);
        Ok(sig.to_bytes().into())
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

    /// Signs the envelope with a p256 device key.
    pub fn sign(&mut self, key: &dyn DeviceKey) -> Result<(), Error> {
        self.sig = key.sign(&self.challenge())?.to_vec();
        self.key = hex(&sha256(&key.public_key()));
        self.authenticator_data.clear();
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
        let raw = key.sign(b"msg").unwrap();
        let der = Signature::from_slice(&raw).unwrap().to_der();
        assert_eq!(der_signature(der.as_bytes()).unwrap(), raw);
        assert!(der_signature(&raw).is_err());
    }
}
