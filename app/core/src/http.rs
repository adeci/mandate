//! HTTP/1.1 over rustls, one connection per request, with the TLS checks of
//! owner-api.md: a pinned key, or a certificate that chains to a known root.
//! The other end may be hostile, during the code exchange above all, so what
//! it sends is bounded in size and time.

use crate::{Error, invalid, sha256};
use httparse::Status;
use rustls::client::WebPkiServerVerifier;
use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::crypto::{CryptoProvider, verify_tls13_signature};
use rustls::pki_types::{CertificateDer, ServerName, UnixTime};
use rustls::{
    ClientConfig, ClientConnection, DigitallySignedStruct, RootCertStore, SignatureScheme,
    StreamOwned,
};
use serde::Deserialize;
use serde::de::DeserializeOwned;
use std::fmt::Write as _;
use std::io::{self, BufRead, BufReader, Read, Write};
use std::net::{TcpStream, ToSocketAddrs};
use std::sync::{Arc, LazyLock, Mutex};
use std::time::{Duration, Instant};

/// How long the TCP connect and then the TLS handshake may each take: a
/// server out of reach shows quickly, whatever the request may take after.
const CONNECT: Duration = Duration::from_secs(4);
/// How long one write may block.
const WRITE: Duration = Duration::from_secs(15);
/// A line of a response head or of a stream. The servers' are far shorter.
pub(crate) const MAX_LINE: u64 = 8 << 10;
const MAX_HEAD: usize = 64 << 10;
const MAX_BODY: u64 = 4 << 20;

/// How long a request may take.
#[derive(Clone, Copy, Debug)]
pub(crate) enum Wait {
    /// The whole request, the answer's body included.
    Total(Duration),
    /// Each read, for a stream that does not end.
    Idle(Duration),
}

/// How the app checks the server's TLS key once paired.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Trust {
    /// Only the key with this SPKI SHA-256.
    Pin([u8; 32]),
    /// A certificate for the host that chains to a known root: Mozilla's,
    /// from webpki-roots.
    Roots,
}

/// What a connection showed: its TLS key, and whether its certificate chains
/// to a known root.
#[derive(Clone, Copy, Debug)]
pub(crate) struct Peer {
    pub spki_sha256: [u8; 32],
    pub chained: bool,
}

impl Peer {
    /// The trust to keep after a first connect: normal checks if the
    /// certificate chains to a root, else its key pinned.
    pub fn trust(&self) -> Trust {
        if self.chained { Trust::Roots } else { Trust::Pin(self.spki_sha256) }
    }
}

/// An https origin and how its key is checked. With no trust, any key is
/// taken; only the code exchange does that, where server_confirm checks it.
#[derive(Clone, Debug)]
pub(crate) struct Server {
    authority: String,
    host: String,
    port: u16,
    pub trust: Option<Trust>,
}

pub(crate) struct Response {
    pub status: u16,
    pub peer: Peer,
    pub body: Box<dyn BufRead + Send>,
    pub cache: Cache,
}

/// What a response says about keeping it and asking again.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub(crate) struct Cache {
    /// `Cache-Control: immutable`: the body never changes.
    pub immutable: bool,
    /// `Retry-After`, in seconds.
    pub retry_after: Option<u64>,
}

impl Server {
    pub fn new(url: &str, trust: Option<Trust>) -> Result<Server, Error> {
        let rest = url
            .strip_prefix("https://")
            .ok_or_else(|| invalid("the server URL is not https://"))?;
        let authority = rest.strip_suffix('/').unwrap_or(rest);
        let host_bytes = |c: u8| c.is_ascii_alphanumeric() || b".-:[]".contains(&c);
        if authority.is_empty() || !authority.bytes().all(host_bytes) {
            return Err(invalid("the server URL is not https://host[:port]"));
        }
        let (host, port) = match authority.rsplit_once(':') {
            Some((h, p)) if !p.contains(']') => {
                (h, p.parse().map_err(|_| invalid("the server URL's port is not a number"))?)
            }
            _ => (authority, 443),
        };
        let host = host.strip_prefix('[').and_then(|h| h.strip_suffix(']')).unwrap_or(host);
        Ok(Server { authority: authority.into(), host: host.into(), port, trust })
    }

    /// Sends one request on a new connection and reads the response head.
    pub fn send(
        &self,
        method: &str,
        path: &str,
        token: Option<&str>,
        body: Option<&[u8]>,
        wait: Wait,
    ) -> Result<Response, Error> {
        // Nothing that could end the request line or a header, or leave
        // /owner/v1/.
        let plain = |s: &str| s.bytes().all(|c| c.is_ascii_graphic());
        let dots = path.split(['/', '?']).any(|s| s == "." || s == "..");
        if !plain(method)
            || !plain(path)
            || !path.starts_with('/')
            || dots
            || !token.is_none_or(plain)
        {
            return Err(invalid("a request's method, path or token is malformed"));
        }
        let (until, idle) = match wait {
            Wait::Total(d) => (Some(Instant::now() + d), d),
            Wait::Idle(d) => (None, d),
        };
        let tcp = connect(&self.host, self.port)?;
        tcp.set_read_timeout(Some(CONNECT))?;
        tcp.set_write_timeout(Some(WRITE))?;
        let verifier = Arc::new(Verifier { trust: self.trust, peer: Mutex::new(None) });
        let config = ClientConfig::builder_with_provider(TLS.provider.clone())
            // Spelled out, since another crate of the app may build TLS 1.2
            // into rustls.
            .with_protocol_versions(&[&rustls::version::TLS13])
            .map_err(tls_error)?
            .dangerous()
            .with_custom_certificate_verifier(verifier.clone())
            .with_no_client_auth();
        let name = ServerName::try_from(self.host.clone())
            .map_err(|_| invalid("the server URL's host is not a name or an address"))?;
        let conn = ClientConnection::new(Arc::new(config), name).map_err(tls_error)?;
        let mut conn = Conn { tls: StreamOwned::new(conn, tcp), until, idle };

        let mut head = format!(
            "{method} {path} HTTP/1.1\r\nHost: {}\r\nConnection: close\r\n",
            self.authority
        );
        if let Some(t) = token {
            let _ = write!(head, "Authorization: Bearer {t}\r\n");
        }
        if let Some(b) = body {
            let _ =
                write!(head, "Content-Type: application/json\r\nContent-Length: {}\r\n", b.len());
        }
        head.push_str("\r\n");
        // rustls finishes the handshake, and so the checks, before it sends
        // any of this; past the handshake, reads wait as the request may.
        conn.tls.write_all(head.as_bytes())?;
        conn.tls.sock.set_read_timeout(Some(idle))?;
        conn.tls.write_all(body.unwrap_or_default())?;
        conn.tls.flush()?;
        let peer =
            verifier.peer.lock().unwrap().ok_or_else(|| invalid("TLS without a certificate"))?;

        let (status, body, cache) = read_head(BufReader::new(conn))?;
        Ok(Response { status, peer, body, cache })
    }
}

/// Reads a response head and returns its status, the body it frames, and
/// what it says about caching.
fn read_head<R: BufRead + Send + 'static>(
    mut r: R,
) -> Result<(u16, Box<dyn BufRead + Send>, Cache), Error> {
    let (mut head, mut line) = (String::new(), String::new());
    while line != "\r\n" {
        if !read_line(&mut r, MAX_LINE, &mut line)? {
            return Err(io::Error::from(io::ErrorKind::UnexpectedEof).into());
        }
        head.push_str(&line);
        if head.len() > MAX_HEAD {
            return Err(invalid("response head over 64 KiB"));
        }
    }
    let mut headers = [httparse::EMPTY_HEADER; 64];
    let mut res = httparse::Response::new(&mut headers);
    let complete = matches!(res.parse(head.as_bytes()), Ok(Status::Complete(n)) if n == head.len());
    let (Some(1), Some(status), true) = (res.version, res.code, complete) else {
        return Err(invalid("malformed HTTP/1.1 response head"));
    };
    let (mut length, mut chunked, mut cache) = (None, false, Cache::default());
    for h in res.headers.iter() {
        if h.name.eq_ignore_ascii_case("content-length") {
            let n = std::str::from_utf8(h.value)
                .ok()
                .filter(|v| v.bytes().all(|c| c.is_ascii_digit()))
                .and_then(|v| v.parse().ok())
                .filter(|&n| n <= MAX_BODY);
            if length.is_some() || n.is_none() {
                return Err(invalid("bad, repeated or oversized Content-Length"));
            }
            length = n;
        } else if h.name.eq_ignore_ascii_case("cache-control") {
            cache.immutable = String::from_utf8_lossy(h.value)
                .split(',')
                .any(|d| d.trim().eq_ignore_ascii_case("immutable"));
        } else if h.name.eq_ignore_ascii_case("retry-after") {
            cache.retry_after =
                std::str::from_utf8(h.value).ok().and_then(|v| v.trim().parse().ok());
        } else if h.name.eq_ignore_ascii_case("transfer-encoding") {
            if chunked || !h.value.eq_ignore_ascii_case(b"chunked") {
                return Err(invalid("Transfer-Encoding other than chunked"));
            }
            chunked = true;
        }
    }
    let body: Box<dyn BufRead + Send> = match (chunked, length) {
        (true, None) => Box::new(BufReader::new(Chunked { r, left: 0, done: false })),
        (true, Some(_)) => return Err(invalid("both Content-Length and chunked")),
        (false, Some(n)) => Box::new(r.take(n)),
        (false, None) => Box::new(r),
    };
    Ok((status, body, cache))
}

impl Response {
    /// The body as JSON for a 2xx status, else the owner API's error.
    pub fn json<T: DeserializeOwned>(self) -> Result<T, Error> {
        if !(200..300).contains(&self.status) {
            return Err(self.error());
        }
        serde_json::from_reader(self.body.take(MAX_BODY))
            .map_err(|e| invalid(format!("server answer: {e}")))
    }

    pub fn error(self) -> Error {
        #[derive(Deserialize)]
        struct Body {
            error: String,
            #[serde(default)]
            code: String,
            attempts_left: Option<u32>,
        }
        let mut text = String::new();
        // A refusal is short; whatever does not read is left out.
        let _ = self.body.take(1 << 16).read_to_string(&mut text);
        let (code, message, attempts_left) = match serde_json::from_str::<Body>(&text) {
            Ok(b) => (b.code, b.error, b.attempts_left),
            Err(_) => (String::new(), text.trim().into(), None),
        };
        Error::Api { status: self.status, code, message, attempts_left }
    }
}

/// Reads a line of at most limit bytes, its newline included; false at the
/// end of the input.
pub(crate) fn read_line(
    r: &mut (impl BufRead + ?Sized),
    limit: u64,
    line: &mut String,
) -> io::Result<bool> {
    line.clear();
    if Read::take(&mut *r, limit).read_line(line)? == 0 {
        return Ok(false);
    }
    if !line.ends_with('\n') {
        return Err(io::Error::new(io::ErrorKind::InvalidData, "line too long, or cut off"));
    }
    Ok(true)
}

fn connect(host: &str, port: u16) -> io::Result<TcpStream> {
    let mut last = io::Error::new(io::ErrorKind::NotFound, format!("{host} has no address"));
    for a in (host, port).to_socket_addrs()? {
        match TcpStream::connect_timeout(&a, CONNECT) {
            Ok(s) => return Ok(s),
            Err(e) => last = e,
        }
    }
    Err(last)
}

fn tls_error(e: rustls::Error) -> Error {
    Error::Io(io::Error::other(e))
}

/// A connection whose reads end at the request's deadline.
struct Conn {
    tls: StreamOwned<ClientConnection, TcpStream>,
    until: Option<Instant>,
    idle: Duration,
}

impl Read for Conn {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        if let Some(until) = self.until {
            let left = until.saturating_duration_since(Instant::now());
            if left.is_zero() {
                return Err(io::ErrorKind::TimedOut.into());
            }
            self.tls.sock.set_read_timeout(Some(left.min(self.idle)))?;
        }
        self.tls.read(buf)
    }
}

/// The crypto provider and the root verifier.
#[derive(Debug)]
struct Tls {
    provider: Arc<CryptoProvider>,
    roots: Arc<WebPkiServerVerifier>,
}

static TLS: LazyLock<Tls> = LazyLock::new(|| {
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let store = RootCertStore { roots: webpki_roots::TLS_SERVER_ROOTS.to_vec() };
    let roots = WebPkiServerVerifier::builder_with_provider(Arc::new(store), provider.clone())
        .build()
        .expect("webpki-roots has roots");
    Tls { provider, roots }
});

#[derive(Debug)]
struct Verifier {
    trust: Option<Trust>,
    /// Set by the handshake: this verifier serves one connection.
    peer: Mutex<Option<Peer>>,
}

impl ServerCertVerifier for Verifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer,
        intermediates: &[CertificateDer],
        name: &ServerName,
        ocsp: &[u8],
        now: UnixTime,
    ) -> Result<ServerCertVerified, rustls::Error> {
        let cert = webpki::EndEntityCert::try_from(end_entity)
            .map_err(|e| rustls::Error::General(format!("server certificate: {e}")))?;
        let spki_sha256 = sha256(cert.subject_public_key_info().as_ref());
        let chain = TLS.roots.verify_server_cert(end_entity, intermediates, name, ocsp, now);
        *self.peer.lock().unwrap() = Some(Peer { spki_sha256, chained: chain.is_ok() });
        match self.trust {
            Some(Trust::Roots) => chain,
            Some(Trust::Pin(pin)) if pin != spki_sha256 => {
                Err(rustls::Error::General("the server's TLS key is not the one pinned".into()))
            }
            _ => Ok(ServerCertVerified::assertion()),
        }
    }

    fn verify_tls12_signature(
        &self,
        _: &[u8],
        _: &CertificateDer,
        _: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        Err(rustls::Error::General("TLS 1.2 is not offered".into()))
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        verify_tls13_signature(message, cert, dss, &TLS.provider.signature_verification_algorithms)
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        TLS.provider.signature_verification_algorithms.supported_schemes()
    }
}

/// A chunked transfer-encoded body.
struct Chunked<R> {
    r: R,
    left: u64,
    done: bool,
}

impl<R: BufRead> Read for Chunked<R> {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        let bad = |what| io::Error::new(io::ErrorKind::InvalidData, what);
        if self.left == 0 && !self.done {
            let mut line = String::new();
            if !read_line(&mut self.r, 1 << 10, &mut line)? {
                return Err(io::ErrorKind::UnexpectedEof.into());
            }
            self.left = match httparse::parse_chunk_size(line.as_bytes()) {
                Ok(Status::Complete((n, size))) if n == line.len() => size,
                _ => return Err(bad("bad chunk size")),
            };
            // Trailers are left unread: the connection closes after this.
            self.done = self.left == 0;
        }
        if self.done || buf.is_empty() {
            return Ok(0);
        }
        let n = buf.len().min(usize::try_from(self.left).unwrap_or(usize::MAX));
        let n = self.r.read(&mut buf[..n])?;
        if n == 0 {
            return Err(io::ErrorKind::UnexpectedEof.into());
        }
        self.left -= n as u64;
        if self.left == 0 {
            let mut crlf = [0; 2];
            self.r.read_exact(&mut crlf)?;
            if crlf != *b"\r\n" {
                return Err(bad("chunk not ended by CRLF"));
            }
        }
        Ok(n)
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use proptest::collection::vec;
    use proptest::prelude::*;
    use std::sync::atomic::{AtomicUsize, Ordering::Relaxed};

    /// A peer that sends prefix, then unit forever.
    pub(crate) struct Endless {
        prefix: Vec<u8>,
        unit: Vec<u8>,
        given: Arc<AtomicUsize>,
    }

    /// The endless peer, buffered, and the count of the bytes read from it.
    pub(crate) fn endless(prefix: &[u8], unit: &[u8]) -> (BufReader<Endless>, Arc<AtomicUsize>) {
        let given = Arc::new(AtomicUsize::new(0));
        let e = Endless { prefix: prefix.into(), unit: unit.into(), given: given.clone() };
        (BufReader::new(e), given)
    }

    impl Read for Endless {
        fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
            let at = self.given.fetch_add(buf.len(), Relaxed);
            for (k, b) in (at..).zip(buf.iter_mut()) {
                *b = match k.checked_sub(self.prefix.len()) {
                    None => self.prefix[k],
                    Some(k) => self.unit[k % self.unit.len()],
                };
            }
            Ok(buf.len())
        }
    }

    /// The status and the body read to its end, or past the client's limit.
    fn parse(r: impl BufRead + Send + 'static) -> Result<(u16, Vec<u8>), Error> {
        let (status, body, _) = read_head(r)?;
        let mut out = Vec::new();
        body.take(MAX_BODY + 1).read_to_end(&mut out)?;
        Ok((status, out))
    }

    const CHUNKED: &[u8] = b"HTTP/1.1 200 X\r\nTransfer-Encoding: chunked\r\n\r\n";

    #[test]
    fn caching_headers_read() {
        let head = b"HTTP/1.1 503 X\r\nRetry-After: 7\r\nCache-Control: private, max-age=1, Immutable\r\nContent-Length: 0\r\n\r\n";
        let (status, _, cache) = read_head(io::Cursor::new(head.to_vec())).unwrap();
        assert_eq!((status, cache), (503, Cache { immutable: true, retry_after: Some(7) }));
        let plain = b"HTTP/1.1 200 X\r\nCache-Control: no-store\r\nContent-Length: 0\r\n\r\n";
        let (_, _, cache) = read_head(io::Cursor::new(plain.to_vec())).unwrap();
        assert_eq!(cache, Cache::default());
    }

    #[test]
    fn endless_answers_are_bounded() {
        let slack = 2 * MAX_LINE as usize + (8 << 10);
        // A head that never ends, and a line that never does.
        let heads: [(&[u8], &[u8]); 2] =
            [(b"HTTP/1.1 200 X\r\n", b"X-A: b\r\n"), (b"HTTP/1.1 2", b"0")];
        for (prefix, unit) in heads {
            let (r, given) = endless(prefix, unit);
            assert!(read_head(r).is_err());
            assert!(given.load(Relaxed) <= MAX_HEAD + slack, "{}", given.load(Relaxed));
        }
        // A chunk size line or extension that never ends.
        for (prefix, unit) in [(CHUNKED.to_vec(), b"f"), ([CHUNKED, b"1;"].concat(), b"a")] {
            let (r, given) = endless(&prefix, unit);
            assert!(
                matches!(parse(r), Err(Error::Io(e)) if e.kind() == io::ErrorKind::InvalidData)
            );
            assert!(given.load(Relaxed) <= slack + (1 << 10), "{}", given.load(Relaxed));
        }
        // Chunks without end are read only up to the client's limit.
        let unit = [&b"400\r\n"[..], &[b'a'; 0x400], b"\r\n"].concat();
        let (r, given) = endless(CHUNKED, &unit);
        let (_, body) = parse(r).unwrap();
        assert_eq!(body.len() as u64, MAX_BODY + 1);
        assert!(given.load(Relaxed) as u64 <= 2 * MAX_BODY, "{}", given.load(Relaxed));
    }

    prop_compose! {
        /// A well-formed response: its status, its body, and the bytes on
        /// the wire, with whatever follows the body.
        fn response()(
            status in 100u16..1000,
            reason in "[A-Za-z][A-Za-z ]{0,11}",
            headers in vec(("[A-Za-z][A-Za-z0-9-]{0,12}", "[!-~]([ -~]{0,20}[!-~])?"), 0..8),
            body in vec(any::<u8>(), 0..3000),
            chunks in prop::option::of(vec(1usize..600, 1..8)),
            ext in any::<bool>(),
            tail in vec(any::<u8>(), 0..16),
        ) -> (u16, Vec<u8>, Vec<u8>) {
            let mut w = format!("HTTP/1.1 {status} {reason}\r\n").into_bytes();
            let framing = |k: &str| {
                k.eq_ignore_ascii_case("content-length") || k.eq_ignore_ascii_case("transfer-encoding")
            };
            for (k, v) in headers.iter().filter(|(k, _)| !framing(k)) {
                w.extend(format!("{k}: {v}\r\n").bytes());
            }
            match &chunks {
                None => w.extend(format!("Content-Length: {}\r\n\r\n", body.len()).bytes()),
                Some(_) => w.extend(b"Transfer-Encoding: chunked\r\n\r\n"),
            }
            match &chunks {
                None => w.extend(&body),
                Some(sizes) => {
                    let mut rest = &body[..];
                    for &n in sizes.iter().cycle() {
                        if rest.is_empty() {
                            break;
                        }
                        let (c, r) = rest.split_at(n.min(rest.len()));
                        let size = if ext { format!("{:X};a=b", c.len()) } else { format!("{:x}", c.len()) };
                        w.extend(format!("{size}\r\n").bytes());
                        w.extend(c);
                        w.extend(b"\r\n");
                        rest = r;
                    }
                    w.extend(b"0\r\nX-Trailer: y\r\n\r\n");
                }
            }
            w.extend(&tail);
            (status, body, w)
        }
    }

    /// Bytes made mostly of the pieces of responses.
    fn pieces() -> impl Strategy<Value = Vec<u8>> {
        let piece = prop_oneof![
            Just(b"HTTP/1.1 200 OK\r\n".to_vec()),
            Just(b"Content-Length: 4\r\n".to_vec()),
            Just(b"Transfer-Encoding: chunked\r\n".to_vec()),
            Just(b"\r\n".to_vec()),
            "[0-9a-fA-F]{1,20}(;[ -~]{0,4})?\r?\n?".prop_map(String::into_bytes),
            vec(any::<u8>(), 0..8),
        ];
        vec(piece, 0..24).prop_map(|p| p.concat())
    }

    proptest! {
        #[test]
        fn responses_round_trip((status, body, wire) in response()) {
            let got = parse(io::Cursor::new(wire)).unwrap();
            prop_assert_eq!(got, (status, body));
        }

        /// Anything else is read without a panic, and never past its framing.
        #[test]
        fn any_response(b in pieces()) {
            if let Ok((_, body)) = parse(io::Cursor::new(b.clone())) {
                prop_assert!(body.len() <= b.len());
            }
        }
    }
}
