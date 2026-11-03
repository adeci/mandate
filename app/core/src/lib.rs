//! What the mandate apps need from docs/spec: signed records (records.md), the
//! code exchange as the new party (agent-api.md) and a client for the owner API
//! (owner-api.md).

mod client;
mod envelope;
mod exchange;
mod http;
mod jcs;
mod log;
mod record;

pub use client::{Client, Event, Events, Join, JoinStatus, PairLink, Signed};
pub use envelope::{DeviceKey, Envelope, SoftwareKey};
pub use exchange::{Payload, new_token};
pub use http::Trust;
pub use log::Log;
pub use record::{
    Field, Grant, Identity, Key, KeyEntry, KeyType, Op, Record, Schedule, Time, Write,
};

use sha2::{Digest, Sha256};
use std::fmt;

#[derive(Debug)]
pub enum Error {
    /// Bytes that do not hold to the specs: a record, an envelope, a link or a
    /// server's answer.
    Invalid(String),
    /// The key log does not trust the signing key.
    Untrusted,
    /// The signature does not verify.
    BadSignature,
    /// The other side of a code exchange derived another key: another code, or
    /// another TLS key.
    Confirm,
    /// The device key did not sign, e.g. because the owner cancelled.
    Key(String),
    /// The owner API refused.
    Api { status: u16, code: String, message: String, attempts_left: Option<u32> },
    /// Network or TLS.
    Io(std::io::Error),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        match self {
            Error::Invalid(msg) => f.write_str(msg),
            Error::Untrusted => f.write_str("key is not trusted"),
            Error::BadSignature => f.write_str("signature does not verify"),
            Error::Confirm => f.write_str("code exchange: confirmation failed"),
            Error::Key(msg) => write!(f, "device key: {msg}"),
            Error::Api { status, message, .. } => write!(f, "{status}: {message}"),
            Error::Io(e) => e.fmt(f),
        }
    }
}

impl std::error::Error for Error {}

impl From<std::io::Error> for Error {
    fn from(e: std::io::Error) -> Error {
        Error::Io(e)
    }
}

pub(crate) fn invalid(msg: impl Into<String>) -> Error {
    Error::Invalid(msg.into())
}

pub(crate) fn sha256(b: &[u8]) -> [u8; 32] {
    Sha256::digest(b).into()
}

pub(crate) fn hex(b: &[u8]) -> String {
    const DIGITS: &[u8; 16] = b"0123456789abcdef";
    b.iter()
        .flat_map(|&c| [DIGITS[usize::from(c >> 4)], DIGITS[usize::from(c & 15)]])
        .map(char::from)
        .collect()
}

/// unhex takes lowercase hex only, as the specs write it.
pub(crate) fn unhex(s: &str) -> Option<Vec<u8>> {
    let digit = |c: u8| match c {
        b'0'..=b'9' => Some(c - b'0'),
        b'a'..=b'f' => Some(c - b'a' + 10),
        _ => None,
    };
    if !s.len().is_multiple_of(2) {
        return None;
    }
    s.as_bytes().chunks(2).map(|p| Some(digit(p[0])? << 4 | digit(p[1])?)).collect()
}

pub(crate) fn random<const N: usize>() -> [u8; N] {
    let mut b = [0; N];
    getrandom::fill(&mut b).expect("the system has no randomness");
    b
}

/// The text of docs/spec/NAME, which the tests take their vectors from, so
/// that the spec, the Go code and this crate are held to the same bytes.
#[cfg(test)]
pub(crate) fn spec(name: &str) -> String {
    let path = format!("{}/../../docs/spec/{name}", env!("CARGO_MANIFEST_DIR"));
    std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("{path}: {e}"))
}
