//! CTAPHID: CTAP2 over USB HID, in 64-byte reports on a channel.

use super::{Transport, key_error, odd, random, unknown};
use mandate_core::Error;
use std::time::{Duration, Instant};

pub const REPORT: usize = 64;

const INIT: u8 = 0x86;
const CBOR: u8 = 0x90;
const ERROR: u8 = 0xbf;

/// How long a command may take: the owner has this long to touch the key.
const TIMEOUT: Duration = Duration::from_secs(35);

/// Moves 64-byte HID reports to and from one authenticator.
pub trait Reports {
    fn write(&mut self, report: &[u8; REPORT]) -> Result<(), Error>;
    /// The next report, or an error once the deadline passes.
    fn read(&mut self, deadline: Instant) -> Result<[u8; REPORT], Error>;
}

/// A CTAPHID channel on an authenticator.
pub struct Hid<R> {
    reports: R,
    cid: [u8; 4],
}

impl<R: Reports> Hid<R> {
    /// Gets a channel on the authenticator.
    pub fn open(reports: R) -> Result<Hid<R>, Error> {
        let mut hid = Hid { reports, cid: [0xff; 4] };
        let nonce = random::<8>();
        hid.send(INIT, &nonce)?;
        let deadline = Instant::now() + Duration::from_secs(3);
        loop {
            // Other programs' answers on the broadcast channel carry their nonces.
            let r = hid.recv(INIT, deadline)?;
            if r.len() >= 17 && r[..8] == nonce {
                if r[16] & 0x04 == 0 {
                    return Err(key_error("This key is U2F only. Use a FIDO2 key."));
                }
                hid.cid.copy_from_slice(&r[8..12]);
                return Ok(hid);
            }
        }
    }

    /// Sends a message as an init packet and continuation packets.
    fn send(&mut self, cmd: u8, data: &[u8]) -> Result<(), Error> {
        for report in packets(self.cid, cmd, data) {
            self.reports.write(&report)?;
        }
        Ok(())
    }

    /// Reads the answer to cmd on this channel, past keepalives.
    fn recv(&mut self, cmd: u8, deadline: Instant) -> Result<Vec<u8>, Error> {
        let mut message = Message::new(self.cid, cmd);
        loop {
            if let Some(data) = message.take(&self.reports.read(deadline)?)? {
                return Ok(data);
            }
        }
    }
}

impl<R: Reports> Transport for Hid<R> {
    fn call(&mut self, command: u8, request: &[u8]) -> Result<Vec<u8>, Error> {
        self.send(CBOR, &[&[command][..], request].concat())?;
        self.recv(CBOR, Instant::now() + TIMEOUT)
    }
}

/// A message as reports: an init packet (channel, command, length, data)
/// and continuation packets (channel, sequence number, data).
fn packets(cid: [u8; 4], cmd: u8, data: &[u8]) -> Vec<[u8; REPORT]> {
    let mut report = [0; REPORT];
    report[..4].copy_from_slice(&cid);
    report[4] = cmd;
    report[5..7].copy_from_slice(&(data.len() as u16).to_be_bytes());
    let (first, rest) = data.split_at(data.len().min(REPORT - 7));
    report[7..7 + first.len()].copy_from_slice(first);
    let mut out = vec![report];
    for (seq, chunk) in rest.chunks(REPORT - 5).enumerate() {
        report[4..].fill(0);
        report[4] = seq as u8;
        report[5..5 + chunk.len()].copy_from_slice(chunk);
        out.push(report);
    }
    out
}

/// An answer being put together from reports.
struct Message {
    cid: [u8; 4],
    cmd: u8,
    len: usize,
    data: Option<Vec<u8>>,
    seq: u8,
}

impl Message {
    fn new(cid: [u8; 4], cmd: u8) -> Message {
        Message { cid, cmd, len: 0, data: None, seq: 0 }
    }

    /// Takes one report; answers the whole message once it is complete.
    /// Reports for other channels and keepalives are skipped.
    fn take(&mut self, p: &[u8; REPORT]) -> Result<Option<Vec<u8>>, Error> {
        if p[..4] != self.cid {
            return Ok(None);
        }
        match &mut self.data {
            None => match p[4] {
                c if c == self.cmd => {
                    self.len = usize::from(u16::from_be_bytes([p[5], p[6]]));
                    self.data = Some(p[7..].to_vec());
                }
                ERROR if p[7] == 0x06 => {
                    return Err(key_error("The security key is busy with another app."));
                }
                ERROR => return Err(unknown(p[7])),
                // Keepalives, e.g. while the key waits for a touch.
                _ => return Ok(None),
            },
            Some(data) => {
                if p[4] != self.seq {
                    return Err(odd());
                }
                data.extend_from_slice(&p[5..]);
                self.seq += 1;
            }
        }
        let data = self.data.as_mut().expect("set above");
        if data.len() < self.len {
            return Ok(None);
        }
        data.truncate(self.len);
        Ok(self.data.take())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A command's packets, and its answer with a keepalive before it (while
    /// the key waits for a touch) and another channel's packet in between.
    #[test]
    fn frames_a_command_and_reassembles_its_answer() {
        let cid = [0x7a, 0x2f, 0x0c, 0x11];
        let request = [0x04];
        let sent = packets(cid, CBOR, &request);
        assert_eq!(sent.len(), 1);
        assert_eq!(sent[0][..8], [0x7a, 0x2f, 0x0c, 0x11, 0x90, 0x00, 0x01, 0x04]);
        assert!(sent[0][8..].iter().all(|&b| b == 0));

        let answer: Vec<u8> = (0..150).map(|i| i as u8).collect();
        let mut reports = packets(cid, CBOR, &answer);
        assert_eq!(reports.len(), 3); // 57 + 59 + 34
        let mut keepalive = [0; REPORT];
        keepalive[..4].copy_from_slice(&cid);
        keepalive[4] = 0xbb;
        keepalive[5..8].copy_from_slice(&[0, 1, 2]);
        let mut other = reports[1];
        other[..4].copy_from_slice(&[1, 2, 3, 4]);
        reports.insert(0, keepalive);
        reports.insert(2, other);

        let mut m = Message::new(cid, CBOR);
        let mut got = None;
        for r in &reports {
            if let Some(d) = m.take(r).unwrap() {
                got = Some(d);
            }
        }
        assert_eq!(got.unwrap(), answer);
    }

    #[test]
    fn rejects_out_of_order_continuations() {
        let cid = [1; 4];
        let reports = packets(cid, CBOR, &[7; 200]);
        let mut m = Message::new(cid, CBOR);
        assert!(m.take(&reports[0]).unwrap().is_none());
        assert!(m.take(&reports[2]).is_err());
    }

    #[test]
    fn reports_a_busy_key() {
        let cid = [1; 4];
        let mut p = [0; REPORT];
        p[..4].copy_from_slice(&cid);
        p[4] = ERROR;
        p[5..8].copy_from_slice(&[0, 1, 0x06]);
        assert!(Message::new(cid, CBOR).take(&p).is_err());
    }

    /// Reports a CanoKey (QEMU's canokey device) exchanged with python-fido2:
    /// getInfo; getKeyAgreement and getPinToken; makeCredential; the PIN
    /// token again; getAssertion over 32 bytes of 0x11.
    const CANOKEY: &str = include_str!("canokey.txt");

    /// The recorded messages in order, checking that this module frames each
    /// request into the very reports python-fido2 sent: (command, request
    /// or answer).
    fn recorded() -> Vec<(bool, Vec<u8>)> {
        let mut out = Vec::new();
        let mut open: Option<(bool, Message, Vec<[u8; REPORT]>)> = None;
        for line in CANOKEY.lines() {
            let to_key = match line.split_once(' ') {
                Some((">", _)) => true,
                Some(("<", _)) => false,
                _ => continue,
            };
            let hex = &line[2..];
            let report: [u8; REPORT] = (0..hex.len())
                .step_by(2)
                .map(|i| u8::from_str_radix(&hex[i..i + 2], 16).unwrap())
                .collect::<Vec<_>>()
                .try_into()
                .unwrap();
            let cid: [u8; 4] = report[..4].try_into().unwrap();
            let (_, message, reports) =
                open.get_or_insert_with(|| (to_key, Message::new(cid, CBOR), Vec::new()));
            reports.push(report);
            if let Some(data) = message.take(&report).unwrap() {
                if to_key {
                    assert_eq!(packets(cid, CBOR, &data), *reports);
                }
                out.push((to_key, data));
                open = None;
            }
        }
        out
    }

    #[test]
    fn reads_a_canokey() {
        use super::super::{Cbor, attested, decode};
        use p256::ecdsa::signature::Verifier;
        use p256::ecdsa::{Signature, VerifyingKey};

        let messages = recorded();
        let answer = |command: u8, nth: usize| -> Cbor {
            let i = messages
                .iter()
                .enumerate()
                .filter(|(_, (to_key, m))| *to_key && m[0] == command)
                .nth(nth)
                .unwrap()
                .0;
            let (to_key, a) = &messages[i + 1];
            assert!(!to_key);
            assert_eq!(a[0], 0, "status OK");
            decode(&a[1..]).unwrap().0
        };
        let info = answer(0x04, 0);
        assert_eq!(info.get(4).and_then(|o| o.field("clientPin")), Some(&Cbor::Bool(true)));
        let made = answer(0x01, 0);
        let (id, point) = attested(made.get(2).and_then(Cbor::bytes).unwrap()).unwrap();
        assert!(!id.is_empty());
        let asserted = answer(0x02, 0);
        let data = asserted.get(2).and_then(Cbor::bytes).unwrap();
        assert_eq!(data[32] & 0x05, 0x05, "UP and UV");
        let sig = Signature::from_der(asserted.get(3).and_then(Cbor::bytes).unwrap()).unwrap();
        let key = VerifyingKey::from_sec1_bytes(&point).unwrap();
        key.verify(&[data, &[0x11; 32][..]].concat(), &sig).unwrap();
    }
}
