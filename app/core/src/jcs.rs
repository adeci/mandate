//! The RFC 8785 (JCS) subset records use: objects, arrays, strings and
//! integers, keys sorted and ASCII, no whitespace, no null.

use crate::{Error, invalid};
use serde::Serialize;
use serde_json::Value;

const MAX_INT: u64 = (1 << 53) - 1;

pub(crate) fn canonical<T: Serialize>(v: &T) -> Result<Vec<u8>, Error> {
    let v = serde_json::to_value(v).map_err(|e| invalid(e.to_string()))?;
    let mut b = Vec::new();
    write(&mut b, &v)?;
    Ok(b)
}

fn write(b: &mut Vec<u8>, v: &Value) -> Result<(), Error> {
    match v {
        Value::String(s) => string(b, s),
        Value::Number(n) => match n.as_i64() {
            Some(i) if i.unsigned_abs() <= MAX_INT => b.extend(i.to_string().bytes()),
            _ => {
                return Err(invalid(format!(
                    "{n} is not an integer of at most 2^53-1 in magnitude"
                )));
            }
        },
        Value::Array(a) => {
            b.push(b'[');
            for (i, e) in a.iter().enumerate() {
                if i > 0 {
                    b.push(b',');
                }
                write(b, e)?;
            }
            b.push(b']');
        }
        Value::Object(m) => {
            // Sorted here, not by the map, whose order depends on serde_json's
            // features. Byte order equals JCS's UTF-16 order only for ASCII.
            let mut keys: Vec<&String> = m.keys().collect();
            keys.sort();
            b.push(b'{');
            for (i, k) in keys.into_iter().enumerate() {
                if !k.is_ascii() {
                    return Err(invalid(format!("key {k:?} is not ASCII")));
                }
                if i > 0 {
                    b.push(b',');
                }
                string(b, k);
                b.push(b':');
                write(b, &m[k])?;
            }
            b.push(b'}');
        }
        Value::Null => return Err(invalid("null is not allowed")),
        Value::Bool(_) => return Err(invalid("booleans are not allowed")),
    }
    Ok(())
}

fn string(b: &mut Vec<u8>, s: &str) {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    b.push(b'"');
    for c in s.bytes() {
        match c {
            b'"' | b'\\' => b.extend([b'\\', c]),
            0x08 => b.extend(*b"\\b"),
            0x0c => b.extend(*b"\\f"),
            b'\n' => b.extend(*b"\\n"),
            b'\r' => b.extend(*b"\\r"),
            b'\t' => b.extend(*b"\\t"),
            ..0x20 => b.extend([
                b'\\',
                b'u',
                b'0',
                b'0',
                HEX[usize::from(c >> 4)],
                HEX[usize::from(c & 15)],
            ]),
            _ => b.push(c),
        }
    }
    b.push(b'"');
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use crate::record::strict;
    use proptest::collection::{btree_map, vec};
    use proptest::prelude::*;
    use std::fmt::Write as _;

    /// Values of the subset: any strings, integers in range, ASCII keys.
    fn value() -> impl Strategy<Value = Value> {
        let max = MAX_INT as i64;
        let leaf = prop_oneof![
            vec(any::<char>(), 0..12).prop_map(|c| Value::from(String::from_iter(c))),
            (-max..=max).prop_map(Value::from),
        ];
        leaf.prop_recursive(4, 64, 8, |inner| {
            prop_oneof![
                vec(inner.clone(), 0..8).prop_map(Value::Array),
                btree_map("[ -~]{0,8}", inner, 0..8)
                    .prop_map(|m| Value::Object(m.into_iter().collect())),
            ]
        })
    }

    /// v as JSON, spelled otherwise wherever the next choice is 0 mod 4:
    /// whitespace, escapes JCS does not use, keys out of order or repeated.
    pub(crate) fn respell(v: &Value, choices: &[u8]) -> String {
        let mut c = choices.iter().cycle();
        let mut out = String::new();
        spell(v, &mut || c.next().is_some_and(|b| b % 4 == 0), &mut out);
        out
    }

    fn spell(v: &Value, odd: &mut impl FnMut() -> bool, out: &mut String) {
        if odd() {
            out.push(' ');
        }
        match v {
            Value::String(s) => spell_string(s, odd, out),
            Value::Array(a) => {
                out.push('[');
                for (i, e) in a.iter().enumerate() {
                    if i > 0 {
                        out.push(',');
                    }
                    spell(e, odd, out);
                }
                out.push(']');
            }
            Value::Object(m) => {
                let mut keys: Vec<&String> = m.keys().collect();
                keys.sort();
                if odd() {
                    keys.reverse();
                }
                if odd() && !keys.is_empty() {
                    keys.insert(0, keys[keys.len() - 1]); // the last one wins
                }
                out.push('{');
                for (i, k) in keys.into_iter().enumerate() {
                    if i > 0 {
                        out.push(',');
                    }
                    spell_string(k, odd, out);
                    out.push(':');
                    spell(&m[k], odd, out);
                }
                out.push('}');
            }
            _ => out.push_str(&v.to_string()),
        }
        if odd() {
            out.push('\n');
        }
    }

    fn spell_string(s: &str, odd: &mut impl FnMut() -> bool, out: &mut String) {
        out.push('"');
        for c in s.chars() {
            if odd() {
                for unit in c.encode_utf16(&mut [0; 2]) {
                    let _ = write!(out, "\\u{unit:04X}");
                }
            } else {
                let mut b = Vec::new();
                string(&mut b, c.encode_utf8(&mut [0; 4]));
                out.push_str(std::str::from_utf8(&b[1..b.len() - 1]).unwrap());
            }
        }
        out.push('"');
    }

    proptest! {
        /// Canonical bytes decode to the value and are taken as they are.
        #[test]
        fn round_trip(v in value()) {
            let b = canonical(&v).unwrap();
            prop_assert_eq!(strict::<Value>(&b).unwrap(), v);
        }

        /// Any other spelling of the same value is refused.
        #[test]
        fn other_spellings_refused(v in value(), choices in vec(any::<u8>(), 1..32)) {
            let b = canonical(&v).unwrap();
            let s = respell(&v, &choices);
            prop_assert_eq!(&serde_json::from_str::<Value>(&s).unwrap(), &v, "{}", s);
            prop_assert!(s.as_bytes() == b || strict::<Value>(s.as_bytes()).is_err(), "{}", s);
        }

        /// Whatever bytes are taken re-encode to themselves.
        #[test]
        fn any_bytes(b in vec(any::<u8>(), 0..64)) {
            if let Ok(v) = strict::<Value>(&b) {
                prop_assert_eq!(canonical(&v).unwrap(), b);
            }
        }
    }

    #[test]
    fn refuses_other_integers_and_values() {
        for ok in ["0", "-1", "9007199254740991", "-9007199254740991"] {
            assert!(strict::<Value>(ok.as_bytes()).is_ok(), "{ok} refused");
        }
        let bad = [
            "1.0",
            "1e0",
            "1E0",
            "01",
            "-0",
            "+1",
            "9007199254740992",
            "-9007199254740992",
            "null",
            "true",
            r#"{"é":"x"}"#,
            r#""\ud800""#,
            "\"\u{1}\"",
            "",
        ];
        for b in bad {
            assert!(strict::<Value>(b.as_bytes()).is_err(), "{b} taken");
        }
    }
}
