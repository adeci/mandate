//! The four kinds of record and their encoding (records.md).

use crate::{Error, hex, invalid, jcs, sha256};
use p256::ecdsa::VerifyingKey;
use serde::de::{self, DeserializeOwned};
use serde::{Deserialize, Deserializer, Serialize, Serializer};
use std::collections::BTreeMap;
use std::fmt;
use std::str::FromStr;
use std::time::{SystemTime, UNIX_EPOCH};

/// What an envelope's record bytes decode to for its kind.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Record {
    Key(KeyEntry),
    Identity(Identity),
    Grant(Grant),
    Write(Write),
}

/// A device key.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Key {
    #[serde(rename = "type")]
    pub kind: KeyType,
    /// The SHA-256 of `public_key`, in hex.
    pub id: String,
    pub name: String,
    /// The 65-byte uncompressed P-256 point.
    #[serde(with = "b64")]
    pub public_key: Vec<u8>,
    /// For fido2 keys only.
    #[serde(default, skip_serializing_if = "Vec::is_empty", with = "b64")]
    pub credential_id: Vec<u8>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum KeyType {
    P256,
    Fido2,
}

/// An entry of the key log.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct KeyEntry {
    pub seq: u64,
    pub op: Op,
    pub key: Key,
    /// The SHA-256 of the previous entry's record bytes in hex, "" at seq 0.
    pub prev: String,
    pub at: Time,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Op {
    Add,
    Remove,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Identity {
    pub id: String,
    pub token_sha256: String,
    pub created_at: Time,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Grant {
    pub id: String,
    pub identity: String,
    pub connector: String,
    /// Empty means every chat.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub chats: Vec<String>,
    /// Seconds back from each read; 0 means all history.
    pub horizon: u64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub from: Option<Time>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub until: Option<Time>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub schedule: Option<Schedule>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub note: String,
    pub created_at: Time,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Schedule {
    /// 0 is Sunday; empty means every day.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub days: Vec<u8>,
    /// HH:MM
    pub start: String,
    pub length: u32,
    /// IANA zone.
    pub zone: String,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Write {
    pub id: String,
    pub connector: String,
    pub op: String,
    pub args: BTreeMap<String, String>,
    pub display: Vec<Field>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub note: String,
    pub created_at: Time,
    pub expires_at: Time,
}

/// A line of a write's display.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Field {
    pub label: String,
    pub value: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub param: String,
}

impl Record {
    pub fn kind(&self) -> &'static str {
        match self {
            Record::Key(_) => "key",
            Record::Identity(_) => "identity",
            Record::Grant(_) => "grant",
            Record::Write(_) => "write",
        }
    }

    /// The record bytes, if the kind's rules hold.
    pub fn encode(&self) -> Result<Vec<u8>, Error> {
        self.check()?;
        match self {
            Record::Key(r) => jcs::canonical(r),
            Record::Identity(r) => jcs::canonical(r),
            Record::Grant(r) => jcs::canonical(r),
            Record::Write(r) => jcs::canonical(r),
        }
    }

    /// Decodes record bytes of the kind. It refuses bytes that are not
    /// canonical, have unknown fields or lack a required one, and records the
    /// kind's rules refuse.
    pub fn decode(kind: &str, b: &[u8]) -> Result<Record, Error> {
        let r = match kind {
            "key" => Record::Key(strict(b)?),
            "identity" => Record::Identity(strict(b)?),
            "grant" => Record::Grant(strict(b)?),
            "write" => Record::Write(strict(b)?),
            _ => return Err(invalid(format!("unknown kind {kind:?}"))),
        };
        r.check()?;
        Ok(r)
    }

    /// The kind's rules that need no key log and no clock. A schedule's zone
    /// is not looked up, since the apps carry no tz database; the gateway,
    /// which verifies grants, does.
    fn check(&self) -> Result<(), Error> {
        match self {
            Record::Key(e) => e.key.verifying_key().map(drop),
            Record::Identity(i) => {
                require(is_id(&i.id), "identity id is not a record ID")?;
                require(is_hash(&i.token_sha256), "token_sha256 is not a SHA-256 in hex")
            }
            Record::Grant(g) => {
                require(
                    is_id(&g.id) && is_id(&g.identity),
                    "grant id or identity is not a record ID",
                )?;
                require(!g.connector.is_empty(), "grant has no connector")?;
                require(!g.chats.iter().any(String::is_empty), "grant has an empty chat ID")?;
                match (&g.schedule, g.from, g.until) {
                    (Some(s), None, None) => s.check(),
                    (None, Some(from), Some(until)) if from < until => Ok(()),
                    _ => Err(invalid("a grant has either from before until or a schedule")),
                }
            }
            Record::Write(w) => require(
                is_id(&w.id) && !w.connector.is_empty() && !w.op.is_empty(),
                "a write needs a record ID, a connector and an op",
            ),
        }
    }
}

/// strict refuses unknown fields; re-encoding refuses the rest, since an
/// optional field written empty is then dropped.
fn strict<T: Serialize + DeserializeOwned>(b: &[u8]) -> Result<T, Error> {
    let r: T = serde_json::from_slice(b).map_err(|e| invalid(format!("record: {e}")))?;
    if jcs::canonical(&r)? != b {
        return Err(invalid("record bytes are not canonical"));
    }
    Ok(r)
}

impl Schedule {
    fn check(&self) -> Result<(), Error> {
        require(
            self.days.windows(2).all(|w| w[0] < w[1]) && self.days.iter().all(|&d| d <= 6),
            "schedule days are not ascending from 0 to 6",
        )?;
        let s = self.start.as_bytes();
        let hhmm = s.len() == 5
            && s[2] == b':'
            && [0, 1, 3, 4].iter().all(|&i| s[i].is_ascii_digit())
            && (s[0], s[1]) <= (b'2', b'3')
            && s[3] <= b'5';
        require(hhmm, "schedule start is not HH:MM")?;
        require((1..=86400).contains(&self.length), "schedule length is not 1 to 86400")?;
        require(!self.zone.is_empty(), "schedule has no zone")
    }
}

impl Key {
    /// A p256 device key for its public point.
    pub fn p256(public_key: &[u8], name: &str) -> Key {
        Key {
            kind: KeyType::P256,
            id: hex(&sha256(public_key)),
            name: name.into(),
            public_key: public_key.to_vec(),
            credential_id: Vec::new(),
        }
    }

    pub(crate) fn verifying_key(&self) -> Result<VerifyingKey, Error> {
        require(
            self.credential_id.is_empty() == (self.kind == KeyType::P256),
            "a credential_id belongs to fido2 keys, and each has one",
        )?;
        // from_sec1_bytes also takes compressed points.
        require(
            self.public_key.len() == 65 && self.public_key[0] == 4,
            "public_key is not an uncompressed point",
        )?;
        let vk = VerifyingKey::from_sec1_bytes(&self.public_key)
            .map_err(|_| invalid("public_key is not a P-256 point"))?;
        require(
            hex(&sha256(&self.public_key)) == self.id,
            "key id is not the SHA-256 of its public_key",
        )?;
        Ok(vk)
    }
}

fn require(ok: bool, msg: &str) -> Result<(), Error> {
    if ok { Ok(()) } else { Err(invalid(msg)) }
}

fn is_id(s: &str) -> bool {
    s.len() == 26 && s.bytes().all(|c| c.is_ascii_uppercase() || (b'2'..=b'7').contains(&c))
}

pub(crate) fn is_hash(s: &str) -> bool {
    s.len() == 64 && s.bytes().all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
}

/// A time in whole seconds, written in RFC 3339 UTC: `2026-11-03T09:00:00Z`.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct Time(i64);

impl Time {
    pub fn from_unix(secs: i64) -> Time {
        Time(secs)
    }

    pub fn unix(self) -> i64 {
        self.0
    }

    pub fn now() -> Time {
        let d = SystemTime::now().duration_since(UNIX_EPOCH).unwrap_or_default();
        Time(i64::try_from(d.as_secs()).unwrap_or(i64::MAX))
    }
}

impl fmt::Display for Time {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        let secs = self.0.rem_euclid(86400);
        let (y, m, d) = civil(self.0.div_euclid(86400));
        write!(f, "{y:04}-{m:02}-{d:02}T{:02}:{:02}:{:02}Z", secs / 3600, secs / 60 % 60, secs % 60)
    }
}

impl FromStr for Time {
    type Err = Error;

    fn from_str(s: &str) -> Result<Time, Error> {
        let b = s.as_bytes();
        let num = |at: usize, len: usize| {
            let d = b.get(at..at + len)?;
            d.iter().try_fold(0, |n, c| c.is_ascii_digit().then(|| n * 10 + i64::from(c - b'0')))
        };
        let parse = || {
            let shape = b.len() == 20
                && [(4, b'-'), (7, b'-'), (10, b'T'), (13, b':'), (16, b':'), (19, b'Z')]
                    .iter()
                    .all(|&(i, c)| b[i] == c);
            if !shape {
                return None;
            }
            let day = days(num(0, 4)?, num(5, 2)?, num(8, 2)?);
            Some(Time(day * 86400 + num(11, 2)? * 3600 + num(14, 2)? * 60 + num(17, 2)?))
        };
        // Fields out of range, like 13 for a month, come back as another time.
        match parse() {
            Some(t) if t.to_string() == s => Ok(t),
            _ => Err(invalid(format!("time {s:?} is not RFC 3339 UTC in whole seconds"))),
        }
    }
}

// days and civil convert between days since 1970-01-01 and dates, after
// Howard Hinnant's chrono-compatible algorithms.
fn days(y: i64, m: i64, d: i64) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let (era, yoe) = (y.div_euclid(400), y.rem_euclid(400));
    let doy = (153 * (if m > 2 { m - 3 } else { m + 9 }) + 2) / 5 + d - 1;
    era * 146097 + yoe * 365 + yoe / 4 - yoe / 100 + doy - 719468
}

fn civil(days: i64) -> (i64, i64, i64) {
    let z = days + 719468;
    let (era, doe) = (z.div_euclid(146097), z.rem_euclid(146097));
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    (era * 400 + yoe + i64::from(m <= 2), m, d)
}

impl Serialize for Time {
    fn serialize<S: Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        s.collect_str(self)
    }
}

impl<'de> Deserialize<'de> for Time {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Time, D::Error> {
        String::deserialize(d)?.parse().map_err(de::Error::custom)
    }
}

/// Bytes as base64url without padding.
pub(crate) mod b64 {
    use base64::Engine;
    use base64::engine::general_purpose::URL_SAFE_NO_PAD;
    use serde::{Deserialize, Deserializer, Serializer, de};

    pub fn serialize<S: Serializer>(b: &[u8], s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(&URL_SAFE_NO_PAD.encode(b))
    }

    pub fn deserialize<'de, D: Deserializer<'de>>(d: D) -> Result<Vec<u8>, D::Error> {
        URL_SAFE_NO_PAD.decode(String::deserialize(d)?).map_err(de::Error::custom)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn decode_strict() {
        let ok = concat!(
            r#"{"args":{"text":"a<b & c"#,
            "\u{2028}",
            r#""},"connector":"signal","created_at":"2026-11-03T09:30:00Z","#,
            r#""display":[{"label":"Text","value":"\u001f"}],"expires_at":"2026-11-04T09:30:00Z","#,
            r#""id":"Z2K7Q4M3P6X5W2N7D4R3J6YB5C","op":"send"}"#,
        );
        Record::decode("write", ok.as_bytes()).expect("canonical write");
        for (name, from, to) in [
            ("html escape", "a<b", r"a\u003cb"),
            ("U+2028 escape", "\u{2028}", r"\u2028"),
            ("uppercase escape", r"\u001f", r"\u001F"),
            ("unknown field", r#""op":"send""#, r#""op":"send","x":"y""#),
            ("missing field", r#""created_at":"2026-11-03T09:30:00Z","#, ""),
            ("empty optional", r#""op":"send""#, r#""note":"","op":"send""#),
            ("null", r#""display":[{"label":"Text","value":"\u001f"}]"#, r#""display":null"#),
            (
                "unsorted keys",
                r#"{"label":"Text","value":"\u001f"}"#,
                r#"{"value":"\u001f","label":"Text"}"#,
            ),
            ("fractional second", r#"09:30:00Z","display""#, r#"09:30:00.0Z","display""#),
            ("offset", r#"09:30:00Z","display""#, r#"09:30:00+00:00","display""#),
            ("February 30", "2026-11-04T", "2026-02-30T"),
            ("trailing data", r#""op":"send"}"#, r#""op":"send"} "#),
        ] {
            let bad = ok.replacen(from, to, 1);
            assert_ne!(bad, ok, "{name}: edit does not apply");
            assert!(Record::decode("write", bad.as_bytes()).is_err(), "{name}: accepted {bad}");
        }
    }

    #[test]
    fn grant_rules() {
        let from = "2026-11-03T09:00:00Z".parse::<Time>().unwrap();
        let schedule = |days: Vec<u8>, start: &str| Schedule {
            days,
            start: start.into(),
            length: 1200,
            zone: "Europe/Berlin".into(),
        };
        let valid = Grant {
            id: "7RJ4P2W5ZK3HXQ6N5M2C4YB7DE".into(),
            identity: "QX3M7E2KQ4F5PN6ZJ7LDV2Y3AU".into(),
            connector: "signal".into(),
            chats: Vec::new(),
            horizon: 3600,
            from: Some(from),
            until: Some(Time(from.0 + 3600)),
            schedule: None,
            note: String::new(),
            created_at: from,
        };
        Record::Grant(valid.clone()).encode().expect("valid grant");
        let routine = Grant { from: None, until: None, ..valid.clone() };
        let refused = [
            (
                "window and schedule",
                Grant { schedule: Some(schedule(vec![], "07:00")), ..valid.clone() },
            ),
            ("from equals until", Grant { until: Some(from), ..valid.clone() }),
            ("no window", routine.clone()),
            (
                "days descending",
                Grant { schedule: Some(schedule(vec![2, 1], "07:00")), ..routine.clone() },
            ),
            ("start 7:00", Grant { schedule: Some(schedule(vec![], "7:00")), ..routine.clone() }),
            ("start 24:00", Grant { schedule: Some(schedule(vec![], "24:00")), ..routine.clone() }),
        ];
        for (name, g) in refused {
            assert!(Record::Grant(g).encode().is_err(), "{name}: encoded");
        }
    }
}
