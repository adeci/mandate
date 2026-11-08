//! A client for the owner API (owner-api.md), and the two ways a device gets
//! one: pairing the first device, or the code exchange for another. Calls
//! block, so the app makes them off its UI thread.

use crate::envelope::{DeviceKey, Envelope};
use crate::exchange::{self, Payload, token_sha256};
use crate::http::{MAX_LINE, Peer, Response, Server, Trust, Wait, read_line};
use crate::log::Log;
use crate::record::{KeyEntry, Op, Record, Time};
use crate::{Error, hex, invalid, unhex};
use base64::Engine;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use serde_json::json;
use std::io::BufRead;
use std::str::FromStr;
use std::time::Duration;

/// Ordinary calls, some of which wait on a connector.
const CALL: Wait = Wait::Total(Duration::from_secs(60));
/// How long a join poll waits on the server, in seconds.
const POLL: u64 = 60;
/// The stream sends a comment every 30 seconds; two missed, it is dead.
const STREAM: Wait = Wait::Idle(Duration::from_secs(65));
/// A stream event's data; the server's are far smaller.
const MAX_EVENT: usize = 64 << 10;
/// A chat's picture: a 64 px PNG is far smaller.
const MAX_PICTURE: u64 = 256 << 10;

/// A chat's picture from the owner API.
#[derive(Debug, PartialEq, Eq)]
pub enum Picture {
    /// The PNG, kept for good when it is the version the listing named.
    Png { bytes: Vec<u8>, keep: bool },
    /// The chat has none.
    None,
    /// The connector is busy fetching others; ask again after this long.
    Busy(Duration),
}

/// The link the setup page shows as text and as a QR code:
/// `mandate://pair?url=…&spki=…&secret=…`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PairLink {
    pub url: String,
    pub spki_sha256: [u8; 32],
    pub secret: Vec<u8>,
}

impl FromStr for PairLink {
    type Err = Error;

    fn from_str(s: &str) -> Result<PairLink, Error> {
        let query = s
            .strip_prefix("mandate://pair?")
            .ok_or_else(|| invalid("not a mandate://pair link"))?;
        let (mut url, mut spki_sha256, mut secret) = (None, None, None);
        for field in query.split('&') {
            let (k, v) = field.split_once('=').unwrap_or((field, ""));
            let v = unescape(v).ok_or_else(|| invalid("bad escape in the pairing link"))?;
            match k {
                "url" => url = Some(v),
                "spki" => spki_sha256 = unhex(&v).and_then(|b| b.try_into().ok()),
                "secret" => secret = URL_SAFE_NO_PAD.decode(v).ok(),
                _ => {}
            }
        }
        match (url, spki_sha256, secret) {
            (Some(url), Some(spki_sha256), Some(secret)) => {
                Ok(PairLink { url, spki_sha256, secret })
            }
            _ => Err(invalid("the pairing link lacks url, spki or secret")),
        }
    }
}

impl PairLink {
    /// Connects to the link's server before pairing, as the Pair screen
    /// opens: its key must be the link's, and the answer says whether its
    /// certificate also chains to a known root for its host. If it does,
    /// the owner has nothing to compare.
    pub fn check(&self) -> Result<bool, Error> {
        let server = Server::new(&self.url, Some(Trust::Pin(self.spki_sha256)))?;
        Ok(server.send("GET", "/owner/v1/pair", None, None, CALL)?.peer.chained)
    }
}

/// Undoes query escaping: %XX, and + for a space.
fn unescape(s: &str) -> Option<String> {
    let mut out = Vec::with_capacity(s.len());
    let mut b = s.bytes();
    while let Some(c) = b.next() {
        out.push(match c {
            b'%' => {
                u8::from_str_radix(std::str::from_utf8(&[b.next()?, b.next()?]).ok()?, 16).ok()?
            }
            b'+' => b' ',
            c => c,
        });
    }
    String::from_utf8(out).ok()
}

fn to_json(v: &impl Serialize) -> Result<Vec<u8>, Error> {
    serde_json::to_vec(v).map_err(|e| invalid(e.to_string()))
}

/// A paired device's client.
#[derive(Clone)]
pub struct Client {
    server: Server,
    token: String,
}

/// The answer to `signed` and to pairing: the record's kind and ID, the
/// key's for a key entry.
#[derive(Clone, Debug, PartialEq, Eq, Deserialize)]
pub struct Signed {
    pub kind: String,
    pub id: String,
}

impl Client {
    /// The client of a paired device: the server's URL, how its key is
    /// checked, and the device token.
    pub fn new(url: &str, trust: Trust, token: &str) -> Result<Client, Error> {
        Ok(Client { server: Server::new(url, Some(trust))?, token: token.into() })
    }

    /// The client once a first connect showed what to trust from then on.
    fn connected(mut server: Server, peer: Peer, token: String) -> Client {
        server.trust = Some(peer.trust());
        Client { server, token }
    }

    /// Pairs the first device: makes seq 0 of the key log, which adds key
    /// under the device's name and is signed by it, and registers the
    /// device token. Keep `trust()` with the URL.
    pub fn pair(
        link: &PairLink,
        key: &dyn DeviceKey,
        name: &str,
        token: &str,
    ) -> Result<Client, Error> {
        let server = Server::new(&link.url, Some(Trust::Pin(link.spki_sha256)))?;
        let now = Time::now();
        let entry =
            KeyEntry { seq: 0, op: Op::Add, key: key.key(name), prev: String::new(), at: now };
        let mut env = Envelope::new(&Record::Key(entry), now)?;
        env.sign(key)?;
        let body = json!({
            "secret": URL_SAFE_NO_PAD.encode(&link.secret),
            "token_sha256": token_sha256(token),
            "entry": env,
        });
        let res = server.send("POST", "/owner/v1/pair", None, Some(&to_json(&body)?), CALL)?;
        let peer = res.peer;
        res.json::<Signed>()?;
        Ok(Client::connected(server, peer, token.into()))
    }

    /// How this client checks the server's key.
    pub fn trust(&self) -> Trust {
        self.server.trust.expect("a paired client has a trust")
    }

    fn request(
        &self,
        method: &str,
        path: &str,
        body: Option<&[u8]>,
        wait: Wait,
    ) -> Result<Response, Error> {
        let path = format!("/owner/v1/{path}");
        self.server.send(method, &path, Some(&self.token), body, wait)
    }

    /// GETs a route under /owner/v1/, given without that prefix.
    pub fn get<T: DeserializeOwned>(&self, path: &str) -> Result<T, Error> {
        self.request("GET", path, None, CALL)?.json()
    }

    /// GETs a chat's picture at path, a route under /owner/v1/ (owner-api.md,
    /// Chat pictures): its PNG, none, or ask again later.
    pub fn picture(&self, path: &str) -> Result<Picture, Error> {
        let res = self.request("GET", path, None, CALL)?;
        match res.status {
            200..=299 => {
                let (mut bytes, keep) = (Vec::new(), res.cache.immutable);
                std::io::Read::read_to_end(
                    &mut std::io::Read::take(res.body, MAX_PICTURE),
                    &mut bytes,
                )?;
                Ok(Picture::Png { bytes, keep })
            }
            404 => Ok(Picture::None),
            503 => Ok(Picture::Busy(Duration::from_secs(res.cache.retry_after.unwrap_or(5)))),
            _ => Err(res.error()),
        }
    }

    /// POSTs, PUTs, PATCHes or DELETEs a route under /owner/v1/, given
    /// without that prefix, with a JSON body.
    pub fn send<T: DeserializeOwned>(
        &self,
        method: &str,
        path: &str,
        body: &impl Serialize,
    ) -> Result<T, Error> {
        self.request(method, path, Some(&to_json(body)?), CALL)?.json()
    }

    /// POSTs a …/challenge route and answers the unsigned envelope. The app
    /// shows the owner `record()`, then signs it and passes it to `signed`.
    pub fn challenge(&self, path: &str, body: &impl Serialize) -> Result<Envelope, Error> {
        self.send("POST", path, body)
    }

    pub fn signed(&self, envelope: &Envelope) -> Result<Signed, Error> {
        self.send("POST", "signed", envelope)
    }

    /// The server's key log, verified here.
    pub fn keys(&self) -> Result<Log, Error> {
        #[derive(Deserialize)]
        struct Keys {
            log: Vec<Envelope>,
        }
        Log::new(self.get::<Keys>("keys")?.log)
    }

    /// The owner's event stream. It ends with the connection; the app
    /// reconnects, then refetches `now`.
    pub fn events(&self) -> Result<Events, Error> {
        let res = self.request("GET", "stream", None, STREAM)?;
        if res.status != 200 {
            return Err(res.error());
        }
        Ok(Events { body: res.body })
    }
}

/// An event of the owner's stream.
#[derive(Clone, Debug, PartialEq, Eq, Deserialize)]
#[serde(tag = "event", content = "data", rename_all = "lowercase")]
pub enum Event {
    /// An access request or a proposal (`kind`) waits for the owner.
    Waiting { kind: String, id: String, identity: String, connector: String },
    /// A code exchange the owner opened changed state: "open", "confirmed"
    /// (the new party's code matched; it waits to be let in), "approved" or
    /// "ended".
    Exchange { id: String, kind: String, state: String },
    /// What changed: "now", "identities", "connectors" or "keys".
    Changed { what: String },
}

/// The events of the owner's stream, until the connection ends.
pub struct Events {
    body: Box<dyn BufRead + Send>,
}

impl Iterator for Events {
    type Item = Result<Event, Error>;

    fn next(&mut self) -> Option<Result<Event, Error>> {
        let (mut name, mut data, mut line) = (String::new(), String::new(), String::new());
        loop {
            match read_line(&mut self.body, MAX_LINE, &mut line) {
                Ok(false) => return None,
                Ok(true) => {}
                Err(e) => return Some(Err(e.into())),
            }
            let line = line.trim_end_matches(['\r', '\n']);
            if line.is_empty() {
                // Events this app does not know are left out.
                if matches!(name.as_str(), "waiting" | "exchange" | "changed") {
                    let event = serde_json::from_str(&data).and_then(|data: serde_json::Value| {
                        serde_json::from_value(json!({ "event": name, "data": data }))
                    });
                    return Some(event.map_err(|e| invalid(format!("{name} event: {e}"))));
                }
                name.clear();
                data.clear();
                continue;
            }
            let (field, value) = line.split_once(':').unwrap_or((line, ""));
            let value = value.strip_prefix(' ').unwrap_or(value);
            match field {
                "event" => name = value.into(),
                "data" if data.is_empty() => data = value.into(),
                "data" if data.len() + value.len() >= MAX_EVENT => {
                    return Some(Err(invalid("stream event over 64 KiB")));
                }
                "data" => {
                    data.push('\n');
                    data.push_str(value);
                }
                _ => {} // comments
            }
        }
    }
}

/// A new device joining by code exchange, as the new party
/// (owner-api.md, Another device): it is given the code that the owner's
/// device shows.
pub struct Join {
    server: Server,
    name: String,
    /// The exchange, once a try at the code started it.
    id: Option<[u8; 16]>,
    payload: Payload,
    token: String,
    /// The connection that proved the server's key, once the code matched.
    peer: Option<Peer>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum JoinStatus {
    /// The code did not match; the owner's device still shows it.
    WrongCode { attempts_left: u32 },
    /// The code matched and the server proved its TLS key; the owner lets
    /// the device in next.
    Confirmed,
    /// The owner signed: the device's key is trusted and its token works.
    Approved,
    /// Three wrong codes, 10 minutes, or the owner cancelled.
    Ended,
    /// The owner saw the code match and did not let the device in.
    Denied,
}

impl Join {
    /// Joins the server at url as a device with key and name. Nothing is
    /// sent before `answer`.
    pub fn start(url: &str, key: &dyn DeviceKey, name: &str, token: &str) -> Result<Join, Error> {
        Ok(Join {
            server: Server::new(url, None)?,
            name: name.into(),
            id: None,
            payload: Payload { key: Some(key.key(name)), token_sha256: token_sha256(token) },
            token: token.into(),
            peer: None,
        })
    }

    /// Tries the code: starts an attempt on the exchange the owner opened,
    /// answers it, and checks the server's confirmation against the TLS key
    /// the answer came over. With nothing open to join, the server's
    /// refusal is the error.
    pub fn answer(&mut self, code: &str) -> Result<JoinStatus, Error> {
        #[derive(Deserialize)]
        struct Started {
            id: String,
            msg: String,
        }
        #[derive(Deserialize)]
        struct Confirm {
            confirm: String,
        }
        if self.peer.is_some() {
            return Err(invalid("a new try at the code after it matched"));
        }
        let body = to_json(&json!({ "kind": "device", "name": self.name }))?;
        let started: Started =
            self.server.send("POST", "/owner/v1/join", None, Some(&body), CALL)?.json()?;
        let id: [u8; 16] = unhex(&started.id)
            .and_then(|b| b.try_into().ok())
            .ok_or_else(|| invalid("the exchange ID is not 16 bytes in hex"))?;
        self.id = Some(id);
        let ya = unhex(&started.msg).ok_or_else(|| invalid("the server's message is not hex"))?;
        let reply = exchange::respond(&id, code, &ya, &self.payload)?;
        let body = json!({
            "msg": hex(&reply.msg),
            "confirm": hex(&reply.confirm),
            "sealed": hex(&reply.sealed),
        });
        let path = format!("/owner/v1/join/{}", hex(&id));
        let res = self.server.send("POST", &path, None, Some(&to_json(&body)?), CALL)?;
        match res.status {
            403 => {
                return Ok(match res.error() {
                    Error::Api { attempts_left: Some(0), .. } => JoinStatus::Ended,
                    Error::Api { attempts_left: Some(n), .. } => {
                        JoinStatus::WrongCode { attempts_left: n }
                    }
                    _ => JoinStatus::WrongCode { attempts_left: 0 },
                });
            }
            404 | 410 => return Ok(JoinStatus::Ended),
            _ => {}
        }
        let peer = res.peer;
        let confirm = unhex(&res.json::<Confirm>()?.confirm).ok_or(Error::Confirm)?;
        reply.check(&confirm, &peer.spki_sha256)?;
        // From here on the exchange talks only to the key that proved itself.
        self.server.trust = Some(Trust::Pin(peer.spki_sha256));
        self.peer = Some(peer);
        Ok(JoinStatus::Confirmed)
    }

    /// Waits up to a minute, after the code matched, for the owner to let
    /// the device in.
    pub fn poll(&mut self) -> Result<JoinStatus, Error> {
        #[derive(Deserialize)]
        struct Poll {
            status: String,
            #[serde(default)]
            reason: String,
        }
        let (Some(id), Some(_)) = (self.id, self.peer) else {
            return Err(invalid("the code has not matched yet"));
        };
        let path = format!("/owner/v1/join/{}?wait={POLL}", hex(&id));
        let wait = Wait::Total(Duration::from_secs(2 * POLL));
        let p: Poll = self.server.send("GET", &path, None, None, wait)?.json()?;
        Ok(match p.status.as_str() {
            "open" | "confirmed" => JoinStatus::Confirmed,
            "approved" => JoinStatus::Approved,
            "ended" if p.reason == "denied" => JoinStatus::Denied,
            "ended" => JoinStatus::Ended,
            status => return Err(invalid(format!("unexpected join status {status:?}"))),
        })
    }

    /// The joined device's client, once `poll` said Approved. Keep its
    /// `trust()` with the URL.
    pub fn into_client(self) -> Result<Client, Error> {
        let peer = self.peer.ok_or_else(|| invalid("the code was not confirmed"))?;
        Ok(Client::connected(self.server, peer, self.token))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::SoftwareKey;
    use crate::exchange::tests::Server as Attempt;
    use crate::http::tests::endless;
    use proptest::collection::vec;
    use proptest::prelude::*;
    use rustls::pki_types::{CertificateDer, PrivateKeyDer, PrivateSec1KeyDer};
    use rustls::version::{TLS12, TLS13};
    use rustls::{ServerConfig, ServerConnection, StreamOwned, SupportedProtocolVersion};
    use serde_json::Value;
    use std::io::{self, Read, Write};
    use std::net::TcpListener;
    use std::sync::atomic::Ordering;
    use std::sync::{Arc, mpsc};

    // Two self-signed P-256 certificates with their SEC1 keys, and the
    // SHA-256 of the first's SubjectPublicKeyInfo as openssl computes it.
    const CERT: &str = "MIIBlTCCATygAwIBAgIUdKCDbAf7nK0qkvqWApDQpMziquUwCgYIKoZIzj0EAwIwFzEVMBMGA1UEAwwMbWFuZGF0ZS10ZXN0MCAXDTI2MTEwMzA3NDY0N1oYDzIxMjYxMDEwMDc0NjQ3WjAXMRUwEwYDVQQDDAxtYW5kYXRlLXRlc3QwWTATBgcqhkjOPQIBBggqhkjOPQMBBwNCAAQpiuK1qGWxbrxKFuE7ixH38nOrKHS2KzZAuLcW3lorOMezkvFPwiFpVr5U2M8QtA6fB3hgilOBFl0ZiyqITMiKo2QwYjAdBgNVHQ4EFgQU4sSz4aA2ghrfsYhJ8mJYtqybAm4wHwYDVR0jBBgwFoAU4sSz4aA2ghrfsYhJ8mJYtqybAm4wDwYDVR0TAQH_BAUwAwEB_zAPBgNVHREECDAGhwR_AAABMAoGCCqGSM49BAMCA0cAMEQCIC1y93GGLcCj4DBPrPMAiGpReP7RGIta2qNUdBFWG_95AiBig6WmuDpB2aWyDTX9cshthb0gWBwdtYrAlPHDCmu1yg";
    const KEY: &str = "MHcCAQEEIOf-bSPuHajP8AXsEPsITZK_u-3SCLNwa9xgr9jg_RMaoAoGCCqGSM49AwEHoUQDQgAEKYritahlsW68ShbhO4sR9_Jzqyh0tis2QLi3Ft5aKzjHs5LxT8IhaVa-VNjPELQOnwd4YIpTgRZdGYsqiEzIig";
    const SPKI_SHA256: &str = "120d3a8992a6c76328727bded93323f953c13f3e6abb18aa9263a7f78809e287";
    const OTHER_CERT: &str = "MIIBhjCCAS2gAwIBAgIUWvMEwp3XZCHsdVjb5p2kDNMGruwwCgYIKoZIzj0EAwIwGDEWMBQGA1UEAwwNbWFuZGF0ZS1vdGhlcjAgFw0yNjExMDMwODAzMzJaGA8yMTI2MTAxMDA4MDMzMlowGDEWMBQGA1UEAwwNbWFuZGF0ZS1vdGhlcjBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABDH1ld9xOqecplmohxI5A4wAW0zQFTyWGNasecmdRjXTnm9-sc7g8fTS-6aH21s7bWyxUQa5-RAZF1zREY8sBpmjUzBRMB0GA1UdDgQWBBSkQ1wEQzqEjXzJEkOi93AKsOZ2_TAfBgNVHSMEGDAWgBSkQ1wEQzqEjXzJEkOi93AKsOZ2_TAPBgNVHRMBAf8EBTADAQH_MAoGCCqGSM49BAMCA0cAMEQCICJJBLII8othJlTO7dj6ZpOUfrhRjxcPe69UI1YuK_8cAiAanUF_YydnsiTukIz8lppvIuzG40_3Arkf1J9P3hCxkg";
    const OTHER_KEY: &str = "MHcCAQEEIIOFa7iV6lnBaV_eEonVMVvf3ogYjoy58cHpFOGCDy-SoAoGCCqGSM49AwEHoUQDQgAEMfWV33E6p5ymWaiHEjkDjABbTNAVPJYY1qx5yZ1GNdOeb36xzuDx9NL7pofbWzttbLFRBrn5EBkXXNERjywGmQ";

    fn spki() -> [u8; 32] {
        unhex(SPKI_SHA256).unwrap().try_into().unwrap()
    }

    fn tls(cert: &str, key: &str, version: &'static SupportedProtocolVersion) -> Arc<ServerConfig> {
        let b64 = |s: &str| URL_SAFE_NO_PAD.decode(s).unwrap();
        let provider = Arc::new(rustls::crypto::ring::default_provider());
        let config = ServerConfig::builder_with_provider(provider)
            .with_protocol_versions(&[version])
            .unwrap()
            .with_no_client_auth()
            .with_single_cert(
                vec![CertificateDer::from(b64(cert))],
                PrivateKeyDer::Sec1(PrivateSec1KeyDer::from(b64(key))),
            )
            .unwrap();
        Arc::new(config)
    }

    struct Request {
        line: String,
        auth: String,
        body: Value,
    }

    /// A mock owner side on 127.0.0.1. Connection n, from 1, gets the TLS
    /// config tls(n), then the bytes handle answers its one request with.
    fn serve(
        tls: impl Fn(usize) -> Arc<ServerConfig> + Send + 'static,
        mut handle: impl FnMut(Request) -> Vec<u8> + Send + 'static,
    ) -> String {
        let ln = TcpListener::bind("127.0.0.1:0").unwrap();
        let url = format!("https://{}", ln.local_addr().unwrap());
        std::thread::spawn(move || {
            for (n, tcp) in ln.incoming().flatten().enumerate() {
                let mut s = StreamOwned::new(ServerConnection::new(tls(n + 1)).unwrap(), tcp);
                // A client that refuses the key, or stops reading, ends here.
                let _ = read_request(&mut s).and_then(|req| {
                    s.write_all(&handle(req))?;
                    s.conn.send_close_notify();
                    s.flush()
                });
            }
        });
        url
    }

    fn read_request(s: &mut impl Read) -> io::Result<Request> {
        let mut r = io::BufReader::new(s);
        let (mut line, mut auth, mut length) = (String::new(), String::new(), 0);
        r.read_line(&mut line)?;
        loop {
            let mut h = String::new();
            r.read_line(&mut h)?;
            let Some((k, v)) = h.trim_end().split_once(": ") else { break };
            match k {
                "Authorization" => auth = v.into(),
                "Content-Length" => length = v.parse().unwrap(),
                _ => {}
            }
        }
        let mut body = vec![0; length];
        r.read_exact(&mut body)?;
        let body = serde_json::from_slice(&body).unwrap_or_default();
        Ok(Request { line: line.trim_end().into(), auth, body })
    }

    fn reply(status: u16, body: &str) -> Vec<u8> {
        format!("HTTP/1.1 {status} X\r\nContent-Length: {}\r\n\r\n{body}", body.len()).into()
    }

    fn chunked(body: &str) -> Vec<u8> {
        let head = "HTTP/1.1 200 X\r\nTransfer-Encoding: chunked\r\n\r\n";
        format!("{head}{:x}\r\n{body}\r\n0\r\n\r\n", body.len()).into()
    }

    /// Pairs a phone, joins a laptop by the code the owner's device shows,
    /// with one wrong try, and reads the stream, against a mock owner side.
    /// After the code matched, the mock answers from another key once.
    #[test]
    fn pair_join_stream() {
        let spki = spki();
        let (phone, laptop) = (SoftwareKey::generate(), SoftwareKey::generate());
        let (phone_token, laptop_token) = (crate::new_token(), crate::new_token());
        let (payload_tx, payload_rx) = mpsc::channel();
        let (id, code) = ([9; 16], "123456");
        let (mut step, mut attempt) = (0, None);
        let stream_auth = format!("Bearer {laptop_token}");
        let (ours, other) = (tls(CERT, KEY, &TLS13), tls(OTHER_CERT, OTHER_KEY, &TLS13));
        let pick = move |n| if n == 7 { other.clone() } else { ours.clone() };
        let url = serve(pick, move |req| {
            step += 1;
            let body = &req.body;
            let hex_field = |k: &str| unhex(body[k].as_str().unwrap()).unwrap();
            let ok = |v: Value| reply(200, &v.to_string());
            match step {
                1 => {
                    assert_eq!(req.line, "POST /owner/v1/pair HTTP/1.1");
                    let entry: Envelope = serde_json::from_value(body["entry"].clone()).unwrap();
                    assert_eq!(body["secret"], "c2VjcmV0");
                    let log = Log::new([entry]).unwrap();
                    ok(json!({ "kind": "key", "id": log.trusted().next().unwrap().id }))
                }
                2 | 4 => {
                    assert_eq!(req.line, "POST /owner/v1/join HTTP/1.1");
                    assert_eq!(*body, json!({ "kind": "device", "name": "MacBook" }));
                    let a = Attempt::start("device", id, code);
                    let msg = hex(&a.msg);
                    attempt = Some(a);
                    reply(201, &json!({ "id": hex(&id), "msg": msg }).to_string())
                }
                3 | 5 => {
                    assert_eq!(req.line, format!("POST /owner/v1/join/{} HTTP/1.1", hex(&id)));
                    let a = attempt.take().unwrap();
                    let (msg, sealed) = (hex_field("msg"), hex_field("sealed"));
                    match a.finish(&msg, &hex_field("confirm"), &sealed, &spki) {
                        Ok((payload, confirm)) => {
                            payload_tx.send(payload).unwrap();
                            ok(json!({ "confirm": hex(&confirm) }))
                        }
                        Err(_) => reply(
                            403,
                            r#"{"error":"wrong code","code":"forbidden","attempts_left":2}"#,
                        ),
                    }
                }
                6 => {
                    assert_eq!(
                        req.line,
                        format!("GET /owner/v1/join/{}?wait=60 HTTP/1.1", hex(&id))
                    );
                    ok(json!({ "status": "confirmed" }))
                }
                7 => ok(json!({ "status": "approved" })),
                8 if req.auth == stream_auth => chunked(concat!(
                    ": open\n\n",
                    "event: changed\ndata: {\"what\":\"keys\"}\n\n",
                    ": ping\n\n",
                    "event: later\ndata: {}\n\n",
                    "event: waiting\ndata: {\"kind\":\"request\",\"id\":\"R\",\"identity\":\"I\",",
                    "\"connector\":\"signal\"}\n\n",
                )),
                _ => reply(500, &req.line),
            }
        });

        let escaped = url.replace(':', "%3A").replace('/', "%2F");
        let link = format!("mandate://pair?url={escaped}&spki={SPKI_SHA256}&secret=c2VjcmV0");
        let link: PairLink = link.parse().unwrap();
        assert_eq!(
            link,
            PairLink { url: url.clone(), spki_sha256: spki, secret: b"secret".to_vec() }
        );
        let client = Client::pair(&link, &phone, "Pixel 9", &phone_token).unwrap();
        assert_eq!(client.trust(), Trust::Pin(spki));

        let mut join = Join::start(&url, &laptop, "MacBook", &laptop_token).unwrap();
        assert!(matches!(join.poll(), Err(Error::Invalid(_))), "a poll before the code");
        assert_eq!(join.answer("123457").unwrap(), JoinStatus::WrongCode { attempts_left: 2 });
        assert_eq!(join.answer(code).unwrap(), JoinStatus::Confirmed);
        let want =
            Payload { key: Some(laptop.key("MacBook")), token_sha256: token_sha256(&laptop_token) };
        assert_eq!(payload_rx.recv().unwrap(), want);
        assert!(matches!(join.answer(code), Err(Error::Invalid(_))), "a new try after the match");
        assert_eq!(join.poll().unwrap(), JoinStatus::Confirmed);
        assert!(matches!(join.poll(), Err(Error::Io(_))), "approved by another key");
        assert_eq!(join.poll().unwrap(), JoinStatus::Approved);
        let client = join.into_client().unwrap();
        assert_eq!(client.trust(), Trust::Pin(spki));

        let events: Vec<Event> = client.events().unwrap().collect::<Result<_, _>>().unwrap();
        assert_eq!(
            events,
            [
                Event::Changed { what: "keys".into() },
                Event::Waiting {
                    kind: "request".into(),
                    id: "R".into(),
                    identity: "I".into(),
                    connector: "signal".into(),
                },
            ]
        );

        // A pinned client refuses another key before it sends anything.
        let other = Client::new(&url, Trust::Pin([0; 32]), &laptop_token).unwrap();
        assert!(matches!(other.get::<Value>("now"), Err(Error::Io(_))));
    }

    /// A server speaking only TLS 1.2, then answers past their bounds: an
    /// endless header line, a body over 4 MiB, and an endless event.
    #[test]
    fn hostile_answers() {
        let old = serve(|_| tls(CERT, KEY, &TLS12), |_| reply(200, "{}"));
        let client = Client::new(&old, Trust::Pin(spki()), &crate::new_token()).unwrap();
        assert!(matches!(client.get::<Value>("now"), Err(Error::Io(_))), "TLS 1.2");

        let mut step = 0;
        let url = serve(
            |_| tls(CERT, KEY, &TLS13),
            move |_| {
                step += 1;
                let a = |n| "a".repeat(n);
                match step {
                    1 => format!("HTTP/1.1 200 X\r\nX-Long: {}\r\n\r\n{{}}", a(1 << 20)).into(),
                    2 => chunked(&format!("\"{}\"", a(5 << 20))),
                    _ => chunked(&format!(
                        "event: changed\n{}",
                        format!("data: {}\n", a(1000)).repeat(100)
                    )),
                }
            },
        );
        let client = Client::new(&url, Trust::Pin(spki()), &crate::new_token()).unwrap();
        let long = client.get::<Value>("now");
        assert!(
            matches!(&long, Err(Error::Io(e)) if e.kind() == io::ErrorKind::InvalidData),
            "{long:?}"
        );
        assert!(matches!(client.get::<Value>("now"), Err(Error::Invalid(_))), "5 MiB body");
        assert!(matches!(client.events().unwrap().next(), Some(Err(_))), "endless event");
    }

    fn events_of(b: impl Into<Vec<u8>>) -> Events {
        Events { body: Box::new(io::Cursor::new(b.into())) }
    }

    fn event() -> impl Strategy<Value = Event> {
        let s = || vec(any::<char>(), 0..12).prop_map(String::from_iter);
        prop_oneof![
            (s(), s(), s(), s()).prop_map(|(kind, id, identity, connector)| Event::Waiting {
                kind,
                id,
                identity,
                connector
            }),
            (s(), s(), s()).prop_map(|(id, kind, state)| Event::Exchange { id, kind, state }),
            s().prop_map(|what| Event::Changed { what }),
        ]
    }

    /// The event as a server may write it: after a comment and an event the
    /// app does not know, its data over several lines, with CRLFs.
    fn sse(e: &Event, (pretty, crlf, noise): (bool, bool, bool)) -> String {
        let (name, data) = match e {
            Event::Waiting { kind, id, identity, connector } => (
                "waiting",
                json!({ "kind": kind, "id": id, "identity": identity, "connector": connector }),
            ),
            Event::Exchange { id, kind, state } => {
                ("exchange", json!({ "id": id, "kind": kind, "state": state }))
            }
            Event::Changed { what } => ("changed", json!({ "what": what })),
        };
        let data =
            if pretty { serde_json::to_string_pretty(&data).unwrap() } else { data.to_string() };
        let mut s =
            if noise { ": ping\n\nevent: later\ndata: {}\n\n".into() } else { String::new() };
        s += &format!("event: {name}\n");
        for line in data.lines() {
            s += &format!("data: {line}\n");
        }
        s += "\n";
        if crlf { s.replace('\n', "\r\n") } else { s }
    }

    proptest! {
        #[test]
        fn events_round_trip(events in vec((event(), any::<(bool, bool, bool)>()), 0..8)) {
            let wire: String = events.iter().map(|(e, how)| sse(e, *how)).collect();
            let got: Vec<Event> = events_of(wire).collect::<Result<_, _>>().unwrap();
            let want: Vec<Event> = events.into_iter().map(|(e, _)| e).collect();
            prop_assert_eq!(got, want);
        }

        /// Anything else is read to its end without a panic.
        #[test]
        fn any_stream(pieces in vec(prop_oneof![
            Just("event: changed\n".as_bytes().to_vec()),
            Just(b"event: waiting\n".to_vec()),
            Just(b"data: {\"what\":\"keys\"}\n".to_vec()),
            Just(b"data:".to_vec()),
            Just(b"\n".to_vec()),
            Just(b"\r\n".to_vec()),
            Just(b": c\n".to_vec()),
            vec(any::<u8>(), 0..8),
        ], 0..32)) {
            for _ in events_of(pieces.concat()) {}
        }
    }

    /// An event whose data never ends, in long lines or one endless line, is
    /// refused soon after 64 KiB.
    #[test]
    fn endless_events_are_bounded() {
        let slack = 2 * MAX_LINE as usize + (8 << 10);
        let long = format!("data: {}\n", "a".repeat(4000));
        let streams: [(&[u8], &[u8]); 2] =
            [(b"event: changed\n", long.as_bytes()), (b"event: changed\ndata: ", b"a")];
        for (prefix, unit) in streams {
            let (r, given) = endless(prefix, unit);
            assert!(matches!(Events { body: Box::new(r) }.next(), Some(Err(_))));
            assert!(given.load(Ordering::Relaxed) <= MAX_EVENT + slack);
        }
    }
}
