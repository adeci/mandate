//! The key log (records.md).

use crate::envelope::Envelope;
use crate::record::{Key, KeyEntry, Op, Record};
use crate::{Error, hex, invalid, sha256};
use p256::ecdsa::VerifyingKey;

/// A verified key log and the keys it trusts.
#[derive(Clone, Debug, Default)]
pub struct Log {
    entries: Vec<Envelope>,
    trusted: Vec<(Key, VerifyingKey)>,
}

impl Log {
    /// Verifies a whole log, from seq 0.
    pub fn new(entries: impl IntoIterator<Item = Envelope>) -> Result<Log, Error> {
        let mut l = Log::default();
        for e in entries {
            l.append(e)?;
        }
        Ok(l)
    }

    /// Verifies e as the next entry and appends it.
    pub fn append(&mut self, e: Envelope) -> Result<(), Error> {
        if e.kind != "key" {
            return Err(invalid(format!("{} record in the key log", e.kind)));
        }
        let Record::Key(entry) = self.verify(&e)? else {
            unreachable!("a key envelope decodes as a key entry")
        };
        match entry.op {
            Op::Add => {
                let vk = entry.key.verifying_key()?;
                self.trusted.push((entry.key, vk));
            }
            Op::Remove => self.trusted.retain(|(k, _)| k.id != entry.key.id),
        }
        self.entries.push(e);
        Ok(())
    }

    /// The log's length and the SHA-256 of its last entry's record bytes, ""
    /// when empty: the next entry's seq and prev.
    pub fn head(&self) -> (u64, String) {
        match self.entries.last() {
            None => (0, String::new()),
            Some(e) => (self.entries.len() as u64, hex(&sha256(&e.record))),
        }
    }

    pub fn entries(&self) -> &[Envelope] {
        &self.entries
    }

    /// The keys trusted now, in the order they were added.
    pub fn trusted(&self) -> impl Iterator<Item = &Key> {
        self.trusted.iter().map(|(k, _)| k)
    }

    /// Verifies e with the keys trusted now and returns its record. A key
    /// entry must be the log's next; verify does not append it. A write's
    /// times are left to the connector, which checks them against its clock.
    pub fn verify(&self, e: &Envelope) -> Result<Record, Error> {
        let r = e.record()?;
        let (k, vk) = self.signer(&e.key, &r)?;
        if e.nonce.len() < 16 {
            return Err(invalid("nonce is shorter than 16 bytes"));
        }
        e.verify_sig(k, &vk)?;
        if let Record::Key(entry) = &r {
            self.follows(entry)?;
        }
        Ok(r)
    }

    fn signer<'a>(&'a self, id: &str, r: &'a Record) -> Result<(&'a Key, VerifyingKey), Error> {
        if self.entries.is_empty() {
            // Seq 0 is signed by the key it adds.
            return match r {
                Record::Key(e) if e.op == Op::Add && e.key.id == id => {
                    Ok((&e.key, e.key.verifying_key()?))
                }
                _ => Err(Error::Untrusted),
            };
        }
        let (k, vk) = self.trusted.iter().find(|(k, _)| k.id == id).ok_or(Error::Untrusted)?;
        Ok((k, *vk))
    }

    fn follows(&self, e: &KeyEntry) -> Result<(), Error> {
        let (n, head) = self.head();
        if e.seq != n {
            return Err(invalid(format!("seq {} where {n} is next", e.seq)));
        }
        if e.prev != head {
            return Err(invalid("prev does not name the previous entry"));
        }
        let held = self.trusted.iter().find(|(k, _)| k.id == e.key.id).map(|(k, _)| k);
        match (e.op, held) {
            (Op::Add, None) => Ok(()),
            (Op::Add, Some(_)) => Err(invalid(format!("key {} is already trusted", e.key.id))),
            (Op::Remove, Some(k)) if *k == e.key && self.trusted.len() == 1 => {
                Err(invalid("removing the last key"))
            }
            (Op::Remove, Some(k)) if *k == e.key => Ok(()),
            (Op::Remove, _) => Err(invalid(format!("key {} is not trusted as named", e.key.id))),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::envelope::h;
    use crate::record::Time;
    use crate::{DeviceKey, SoftwareKey, unhex};
    use std::collections::HashMap;

    #[derive(Default)]
    struct Vector {
        title: String,
        kind: String,
        record: String,
        nonce: Vec<u8>,
        issued_at: String,
        signer: String,
        auth_data: Vec<u8>,
        challenge: Vec<u8>,
        sig: Vec<u8>,
    }

    #[derive(Default)]
    struct TestKey {
        d: Vec<u8>,
        public_key: Vec<u8>,
        id: String,
    }

    #[derive(Default)]
    struct Spec {
        h_input: Vec<u8>,
        h: Vec<u8>,
        keys: HashMap<String, TestKey>,
        vectors: Vec<Vector>,
        envelope: String,
    }

    /// The vectors of records.md, read from the spec itself.
    fn read_spec() -> Spec {
        let text = crate::spec("records.md");
        let unhex = |s: &str| unhex(s).unwrap_or_else(|| panic!("{s:?} is not hex"));
        let mut s = Spec::default();
        let (mut in_block, mut lang, mut title, mut key) = (false, "", "", "");
        let mut v = Vector::default();
        for line in text.lines() {
            if let Some(l) = line.strip_prefix("```") {
                (in_block, lang) = (!in_block, l);
                continue;
            }
            if !in_block {
                if line.starts_with("**") {
                    title = line.trim_matches('*');
                }
                continue;
            }
            if lang == "json" {
                // The examples before the vectors are elided.
                if !title.is_empty() {
                    s.envelope += line;
                    s.envelope.push('\n');
                }
                continue;
            }
            let f: Vec<&str> = line.split_whitespace().collect();
            match f[..] {
                [] | [_, "=", ..] => {} // blank, or the challenge formula
                [.., last] if line.starts_with("H(\"a\", \"bc\") input") => s.h_input = unhex(last),
                [.., last] if line.starts_with("H(\"a\", \"bc\")") => s.h = unhex(last),
                [name, _, "d", d] => {
                    key = name;
                    s.keys.insert(name.into(), TestKey { d: unhex(d), ..TestKey::default() });
                }
                ["pub", p] => s.keys.get_mut(key).unwrap().public_key = unhex(p),
                ["id", id] => s.keys.get_mut(key).unwrap().id = id.into(),
                ["credential_id", _] => {}
                _ if line.starts_with('{') => {
                    let kind = title.split(',').next().unwrap();
                    v = Vector {
                        title: title.into(),
                        kind: kind.into(),
                        record: line.into(),
                        ..Vector::default()
                    };
                }
                ["nonce", n, "issued_at", at, "key", signer] => {
                    (v.nonce, v.issued_at, v.signer) = (unhex(n), at.into(), signer.into());
                }
                ["authenticator_data", ad] => v.auth_data = unhex(ad),
                ["challenge", c] => v.challenge = unhex(c),
                ["sig", sig] => {
                    v.sig = unhex(sig);
                    s.vectors.push(std::mem::take(&mut v));
                }
                _ => panic!("unparsed vector line {line:?}"),
            }
        }
        assert!(
            s.vectors.len() == 8 && s.keys.len() == 3 && !s.envelope.is_empty(),
            "parsed {} vectors and {} keys from records.md",
            s.vectors.len(),
            s.keys.len()
        );
        s
    }

    impl Spec {
        fn key(&self, name: &str) -> SoftwareKey {
            SoftwareKey::from_bytes(self.keys[name].d.as_slice().try_into().unwrap()).unwrap()
        }

        fn envelope(&self, v: &Vector) -> Envelope {
            Envelope {
                kind: v.kind.clone(),
                record: v.record.clone().into_bytes(),
                nonce: v.nonce.clone(),
                issued_at: v.issued_at.parse().unwrap(),
                key: self.keys[&v.signer].id.clone(),
                authenticator_data: v.auth_data.clone(),
                sig: v.sig.clone(),
            }
        }

        fn vector(&self, title: &str) -> Envelope {
            self.envelope(self.vectors.iter().find(|v| v.title == title).unwrap())
        }

        /// The spec's key log: phone, laptop and YubiKey.
        fn log(&self) -> Log {
            let key_entries = self.vectors.iter().filter(|v| v.kind == "key");
            Log::new(key_entries.map(|v| self.envelope(v))).unwrap()
        }
    }

    fn signed(e: KeyEntry, key: &SoftwareKey) -> Envelope {
        let mut e = Envelope::new(&Record::Key(e), Time::from_unix(1_793_000_000)).unwrap();
        e.sign(key).unwrap();
        e
    }

    #[test]
    fn vectors() {
        let s = read_spec();
        assert_eq!(h(&[b"a", b"bc"]).to_vec(), s.h);
        assert_eq!(sha256(&s.h_input).to_vec(), s.h);
        for (name, k) in &s.keys {
            let public_key = s.key(name).public_key();
            assert_eq!(public_key.to_vec(), k.public_key, "{name}");
            assert_eq!(hex(&sha256(&public_key)), k.id, "{name}");
        }

        let mut l = Log::default();
        for v in &s.vectors {
            let e = s.envelope(v);
            assert_eq!(e.challenge().to_vec(), v.challenge, "{}: challenge", v.title);
            let r = if v.kind == "key" {
                l.append(e.clone()).map(|()| e.record().unwrap())
            } else {
                l.verify(&e)
            };
            let r = r.unwrap_or_else(|err| panic!("{}: {err}", v.title));
            assert_eq!(r.encode().unwrap(), v.record.as_bytes(), "{}: encode", v.title);
        }

        let e0: Envelope = serde_json::from_str(&s.envelope).unwrap();
        assert_eq!(serde_json::to_string_pretty(&e0).unwrap() + "\n", s.envelope);
        Log::new([e0]).unwrap();

        // Must fail.
        let mut g = s.vector("grant, one-off");
        g.kind = "identity".into();
        assert_eq!(
            hex(&g.challenge()),
            "fa68aefd9d143f4937810198130e07ad1816c529dd278258c0c776bd4f552ef8"
        );
        let (phone, vk) = &l.trusted[0];
        assert!(matches!(g.verify_sig(phone, vk), Err(Error::BadSignature)));
        assert!(l.verify(&g).is_err(), "grant as identity verified");
        let mut g = s.vector("grant, one-off");
        g.record = String::from_utf8(g.record)
            .unwrap()
            .replacen(",\"horizon\"", ", \"horizon\"", 1)
            .into_bytes();
        assert!(
            matches!(l.verify(&g), Err(Error::Invalid(m)) if m.contains("canonical")),
            "grant with a space"
        );
    }

    #[test]
    fn log() {
        let s = read_spec();
        let (phone, laptop) = (s.key("phone"), s.key("laptop"));
        let full = s.log();
        let keys: Vec<Key> = full.trusted().cloned().collect();
        let entries = full.entries();
        assert_eq!(full.head(), (3, hex(&sha256(&entries[2].record))));
        // The next entry of l.
        let next = |l: &Log, op, key: &Key| {
            let (seq, prev) = l.head();
            let at = Time::from_unix(1_793_000_000);
            KeyEntry { seq, op, key: key.clone(), prev, at }
        };

        let empty = Log::default();
        let one = Log::new(entries[..1].to_vec()).unwrap();
        let renamed = Key { name: "Old MacBook".into(), ..keys[1].clone() };
        let first_prev = hex(&sha256(&entries[0].record));
        let laptop_removal = next(&full, Op::Remove, &keys[1]);
        let refused = [
            ("seq 0 signed by another key", &empty, next(&empty, Op::Add, &keys[0]), &laptop),
            ("seq 0 removing", &empty, next(&empty, Op::Remove, &keys[0]), &phone),
            ("adding a trusted key", &full, next(&full, Op::Add, &keys[1]), &phone),
            ("removing the last key", &one, next(&one, Op::Remove, &keys[0]), &phone),
            ("removing a renamed key", &full, next(&full, Op::Remove, &renamed), &phone),
            ("wrong prev", &full, KeyEntry { prev: first_prev, ..laptop_removal.clone() }, &phone),
            ("wrong seq", &full, KeyEntry { seq: 4, ..laptop_removal.clone() }, &phone),
        ];
        for (name, l, e, key) in refused {
            assert!(l.clone().append(signed(e, key)).is_err(), "{name}: appended");
        }

        // A removed key's grants stop verifying.
        let routine = s.vector("grant, routine");
        let mut l = full.clone();
        l.verify(&routine).unwrap();
        l.append(signed(laptop_removal, &laptop)).unwrap();
        assert!(matches!(l.verify(&routine), Err(Error::Untrusted)));
        let ids: Vec<&str> = l.trusted().map(|k| k.id.as_str()).collect();
        assert_eq!(ids, [keys[0].id.as_str(), keys[2].id.as_str()]);
    }

    #[test]
    fn fido2() {
        let s = read_spec();
        let l = s.log();
        let yubikey = s.key("yubikey");
        let approved = s.vector("write, approved with the YubiKey");
        l.verify(&approved).unwrap();
        let ad = &approved.authenticator_data;
        for (name, i, byte) in [("no user verification", 32, 0x01), ("another rp ID", 0, ad[0] ^ 1)]
        {
            let mut e = approved.clone();
            e.authenticator_data[i] = byte;
            let msg = [e.authenticator_data.as_slice(), &e.challenge()].concat();
            e.sig = yubikey.sign(&msg).unwrap().to_vec();
            let err = l.verify(&e).expect_err(name);
            assert!(!matches!(err, Error::BadSignature), "{name}: {err}");
        }
    }
}
