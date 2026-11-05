//! CTAP2 over NFC: ISO 7816-4 APDUs to the FIDO applet (CTAP 2.1, 11.3).

use super::{Transport, key_error, odd};
use mandate_core::Error;

/// The FIDO applet.
const AID: [u8; 8] = [0xa0, 0x00, 0x00, 0x06, 0x47, 0x2f, 0x00, 0x01];
/// Short APDUs carry at most this much data; longer messages are chained.
const CHUNK: usize = 255;

/// Sends one APDU to the key on the reader and answers its response,
/// status word last.
pub trait Card {
    fn transceive(&mut self, apdu: &[u8]) -> Result<Vec<u8>, Error>;
}

pub struct Nfc<C> {
    card: C,
}

impl<C: Card> Nfc<C> {
    /// Selects the FIDO applet on the key.
    pub fn open(card: C) -> Result<Nfc<C>, Error> {
        let mut nfc = Nfc { card };
        let select = [&[0x00, 0xa4, 0x04, 0x00, AID.len() as u8][..], &AID, &[0x00]].concat();
        let version = nfc.exchange(&select)?;
        if version != b"FIDO_2_0" && version != b"U2F_V2" {
            return Err(odd());
        }
        if version == b"U2F_V2" {
            return Err(key_error("This key is U2F only. Use a FIDO2 key."));
        }
        Ok(nfc)
    }

    /// Sends an APDU and answers its data, past `61xx` (more data waits)
    /// and `9100` (the key is still working, e.g. waiting for a touch).
    fn exchange(&mut self, apdu: &[u8]) -> Result<Vec<u8>, Error> {
        let mut data = Vec::new();
        let mut next = apdu.to_vec();
        loop {
            let r = self.card.transceive(&next)?;
            let (body, sw) = r.split_at_checked(r.len().wrapping_sub(2)).ok_or_else(odd)?;
            match (sw[0], sw[1]) {
                (0x90, 0x00) => {
                    data.extend_from_slice(body);
                    return Ok(data);
                }
                (0x61, n) => {
                    data.extend_from_slice(body);
                    next = vec![0x00, 0xc0, 0x00, 0x00, n];
                }
                // NFCCTAP_GETRESPONSE
                (0x91, 0x00) => next = vec![0x80, 0x11, 0x00, 0x00, 0x00],
                (0x6a, 0x82) => {
                    return Err(key_error("This card has no FIDO2 security key on it."));
                }
                (a, b) => {
                    return Err(Error::Key(format!("The security key answered {a:02x}{b:02x}.")));
                }
            }
        }
    }
}

impl<C: Card> Transport for Nfc<C> {
    fn call(&mut self, command: u8, request: &[u8]) -> Result<Vec<u8>, Error> {
        let message = [&[command][..], request].concat();
        let apdus = apdus(&message);
        let (last, chained) = apdus.split_last().expect("one APDU at least");
        for apdu in chained {
            self.exchange(apdu)?;
        }
        self.exchange(last)
    }
}

/// NFCCTAP_MSG as short APDUs, chained (CLA bit 0x10) when longer than one.
fn apdus(message: &[u8]) -> Vec<Vec<u8>> {
    let chunks: Vec<&[u8]> = message.chunks(CHUNK).collect();
    let n = chunks.len();
    chunks
        .into_iter()
        .enumerate()
        .map(|(i, chunk)| {
            let cla = if i + 1 < n { 0x90 } else { 0x80 };
            let mut apdu = vec![cla, 0x10, 0x00, 0x00, chunk.len() as u8];
            apdu.extend_from_slice(chunk);
            if i + 1 == n {
                apdu.push(0x00);
            }
            apdu
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::VecDeque;

    /// A card that checks each APDU it gets and answers from a script.
    struct Script(VecDeque<(Vec<u8>, Vec<u8>)>);

    impl Card for Script {
        fn transceive(&mut self, apdu: &[u8]) -> Result<Vec<u8>, Error> {
            let (want, answer) = self.0.pop_front().expect("an APDU the script expects");
            assert_eq!(apdu, &want[..]);
            Ok(answer)
        }
    }

    fn select() -> (Vec<u8>, Vec<u8>) {
        let apdu = hex("00a4040008a0000006472f000100");
        (apdu, [&b"FIDO_2_0"[..], &[0x90, 0x00]].concat())
    }

    fn hex(s: &str) -> Vec<u8> {
        (0..s.len()).step_by(2).map(|i| u8::from_str_radix(&s[i..i + 2], 16).unwrap()).collect()
    }

    #[test]
    fn selects_the_applet_and_sends_a_short_message() {
        let script = Script(VecDeque::from([
            select(),
            // authenticatorGetInfo; the answer is the status byte, CBOR, 9000.
            (hex("80100000010400"), hex("00a19000")),
        ]));
        let mut nfc = Nfc::open(script).unwrap();
        assert_eq!(nfc.call(0x04, &[]).unwrap(), hex("00a1"));
    }

    #[test]
    fn chains_long_messages_and_long_answers_past_keepalives() {
        let request = vec![0xab; 300];
        let mut first = vec![0x90, 0x10, 0x00, 0x00, 0xff, 0x02];
        first.extend(&request[..254]);
        let mut last = vec![0x80, 0x10, 0x00, 0x00, 46];
        last.extend(&request[254..]);
        last.push(0x00);
        let script = Script(VecDeque::from([
            select(),
            (first, hex("9000")),
            // still waiting for a touch, then the answer in two parts
            (last, hex("019100")),
            (hex("8011000000"), hex("00a16104")),
            (hex("00c0000004"), hex("010203049000")),
        ]));
        let mut nfc = Nfc::open(script).unwrap();
        assert_eq!(nfc.call(0x02, &request).unwrap(), hex("00a101020304"));
    }

    #[test]
    fn refuses_a_card_without_the_applet() {
        let script = Script(VecDeque::from([(select().0, hex("6a82"))]));
        assert!(Nfc::open(script).is_err());
    }
}
