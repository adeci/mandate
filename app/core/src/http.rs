//! HTTP/1.1 over rustls, one connection per request, with the TLS checks of
//! owner-api.md: a pinned key, or a certificate that chains to a known root.

use crate::{Error, invalid, sha256};
use rustls::client::WebPkiServerVerifier;
use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::crypto::{CryptoProvider, verify_tls12_signature, verify_tls13_signature};
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
use std::time::Duration;

const CONNECT: Duration = Duration::from_secs(15);

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
}

impl Server {
    pub fn new(url: &str, trust: Option<Trust>) -> Result<Server, Error> {
        let rest = url
            .strip_prefix("https://")
            .ok_or_else(|| invalid("the server URL is not https://"))?;
        let authority = rest.strip_suffix('/').unwrap_or(rest);
        if authority.is_empty() || authority.contains(['/', '?', '#', '@']) {
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
    /// timeout bounds each read, so it must outlast long polls.
    pub fn send(
        &self,
        method: &str,
        path: &str,
        token: Option<&str>,
        body: Option<&[u8]>,
        timeout: Duration,
    ) -> Result<Response, Error> {
        let tcp = connect(&self.host, self.port)?;
        tcp.set_read_timeout(Some(timeout))?;
        tcp.set_write_timeout(Some(CONNECT))?;
        let verifier = Arc::new(Verifier { trust: self.trust, peer: Mutex::new(None) });
        let config = ClientConfig::builder_with_provider(TLS.provider.clone())
            .with_safe_default_protocol_versions()
            .map_err(tls_error)?
            .dangerous()
            .with_custom_certificate_verifier(verifier.clone())
            .with_no_client_auth();
        let name = ServerName::try_from(self.host.clone())
            .map_err(|_| invalid("the server URL's host is not a name or an address"))?;
        let conn = ClientConnection::new(Arc::new(config), name).map_err(tls_error)?;
        let mut stream = StreamOwned::new(conn, tcp);

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
        // any of this.
        stream.write_all(head.as_bytes())?;
        stream.write_all(body.unwrap_or_default())?;
        stream.flush()?;
        let peer =
            verifier.peer.lock().unwrap().ok_or_else(|| invalid("TLS without a certificate"))?;

        let mut r = BufReader::new(stream);
        let mut line = String::new();
        r.read_line(&mut line)?;
        let status = line
            .strip_prefix("HTTP/1.1 ")
            .and_then(|s| s.get(..3)?.parse().ok())
            .ok_or_else(|| invalid(format!("not an HTTP/1.1 response: {line:?}")))?;
        let (mut length, mut chunked) = (None, false);
        loop {
            line.clear();
            if r.read_line(&mut line)? == 0 {
                return Err(io::Error::from(io::ErrorKind::UnexpectedEof).into());
            }
            let Some((k, v)) = line.trim_end().split_once(':') else {
                break;
            };
            let v = v.trim();
            if k.eq_ignore_ascii_case("content-length") {
                length = Some(v.parse().map_err(|_| invalid("bad Content-Length"))?);
            } else if k.eq_ignore_ascii_case("transfer-encoding") {
                chunked = v.eq_ignore_ascii_case("chunked");
            }
        }
        let body: Box<dyn BufRead + Send> = match (chunked, length) {
            (true, _) => Box::new(BufReader::new(Chunked { r, left: 0, done: false })),
            (false, Some(n)) => Box::new(r.take(n)),
            (false, None) => Box::new(r),
        };
        Ok(Response { status, peer, body })
    }
}

impl Response {
    /// The body as JSON for a 2xx status, else the owner API's error.
    pub fn json<T: DeserializeOwned>(self) -> Result<T, Error> {
        if !(200..300).contains(&self.status) {
            return Err(self.error());
        }
        serde_json::from_reader(self.body).map_err(|e| invalid(format!("server answer: {e}")))
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
        message: &[u8],
        cert: &CertificateDer,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        verify_tls12_signature(message, cert, dss, &TLS.provider.signature_verification_algorithms)
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
        if self.left == 0 && !self.done {
            let mut line = String::new();
            self.r.read_line(&mut line)?;
            let size = line.trim_end().split(';').next().unwrap_or_default();
            self.left = u64::from_str_radix(size, 16)
                .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "bad chunk size"))?;
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
        }
        Ok(n)
    }
}
