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
