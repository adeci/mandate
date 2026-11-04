#[cfg(target_os = "android")]
mod android;
mod api;
#[cfg(not(target_os = "android"))]
mod choose;
#[cfg(not(target_os = "android"))]
pub mod desktop;
mod device;
mod link;
mod now;
mod pick;
mod screens;
mod tabs;
mod theme;
mod words;

use api::{Activity, Connector, Exchange, Hidden, Identity, Keys, Now, Proposal, Request, Step};
use device::Device;
use futures::StreamExt;
use gpui::{App, AppContext, Context, Entity, FocusHandle, KeyDownEvent, Window, WindowOptions};
use mandate_core::{
    Client, DeviceKey, Envelope, Error, Join, JoinStatus, PairLink, Record, new_token,
};
use serde::Deserialize;
use serde_json::{Value, json};
use std::borrow::Cow;
use std::collections::{BTreeMap, BTreeSet, HashMap};
use std::path::PathBuf;
use std::time::Duration;

#[derive(Clone, Copy, PartialEq)]
enum Tab {
    Now,
    Agents,
    Connections,
    Activity,
}

enum Screen {
    Pair {
        text: String,
    },
    /// Joining as another device: the server's address, as typed.
    Join {
        url: String,
    },
    /// The code to type on a device already in, and how the join goes.
    Joining {
        code: String,
        status: &'static str,
    },
    /// Picking this computer's device key.
    #[cfg(not(target_os = "android"))]
    Choose(choose::Choose),
    Tab(Tab),
    Settings,
    Code {
        exchange: Exchange,
        code: String,
    },
    Admit {
        exchange: Exchange,
        envelope: Envelope,
    },
    Review {
        proposal: Proposal,
        envelope: Envelope,
    },
    /// Linking a connector: the step it asked last, None while it loads.
    Link {
        name: String,
        step: Option<Step>,
        values: BTreeMap<String, String>,
        focus: Option<String>,
    },
    /// Adding to never-share, from a connector's chat list.
    Hide {
        connector: Option<String>,
    },
    /// The owner's own grant for an agent.
    Grant(pick::NewGrant),
}

/// What a new device key is for: pairing as the first device, or joining
/// the server at a URL as another.
#[derive(Clone)]
enum Next {
    Pair(PairLink),
    Join(String),
}

/// The device key's PIN, asked before a signature, and the signature.
#[cfg(not(target_os = "android"))]
struct Unlock {
    pin: String,
    then: Then,
}

#[cfg(not(target_os = "android"))]
type Then = Box<dyn FnOnce(&mut Shell, &mut Context<Shell>)>;

/// How the owner narrowed a request before approving it.
#[derive(Clone)]
struct Adjust {
    open: bool,
    chats: BTreeSet<String>,
    /// The names of the chats to tick: those the terms matched, then those
    /// the owner picked.
    names: BTreeMap<String, String>,
    for_: u64,
    horizon: u64,
}

impl Adjust {
    fn of(r: &Request) -> Adjust {
        let names: BTreeMap<String, String> =
            r.chats().iter().map(|c| (c.id.clone(), c.name.clone())).collect();
        let chats = names.keys().cloned().collect();
        Adjust { open: false, chats, names, for_: r.for_, horizon: r.horizon }
    }
}

pub struct Shell {
    dir: PathBuf,
    device: Option<Device>,
    /// The device key's ID, to mark it among the trusted keys.
    key_id: String,
    screen: Screen,
    now: Now,
    exchanges: Vec<Exchange>,
    identities: Vec<Identity>,
    connectors: Vec<Connector>,
    keys: Keys,
    /// Chats and people hidden from every read (never-share).
    hidden: Vec<Hidden>,
    /// The server's URL, for Settings.
    server: String,
    /// The search field's text, while it is open.
    search: Option<String>,
    adjust: HashMap<String, Adjust>,
    /// The chat picker, while one is open.
    picker: Option<pick::Picker>,
    /// Earlier days of activity, loaded by Show more, newest first.
    older: Vec<(String, Vec<Activity>)>,
    /// The last loaded day has more before its last activity.
    older_more: bool,
    /// Stop all's confirmation is open.
    stopping: bool,
    /// The agent or connector whose Delete or Unlink waits for a second tap.
    confirming: Option<String>,
    notice: Option<String>,
    error: Option<String>,
    /// What the device key waits for while it signs, e.g. a touch.
    prompt: Option<&'static str>,
    #[cfg(not(target_os = "android"))]
    unlock: Option<Unlock>,
    /// A waiting item to open once a refresh shows it, from a notification.
    pending: Option<String>,
    focus: FocusHandle,
}

impl Shell {
    fn new(dir: PathBuf, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let device = Device::load(&dir);
        let screen = match device {
            Some(_) => Screen::Tab(Tab::Now),
            None => Screen::Pair { text: String::new() },
        };
        cx.observe_window_appearance(window, |_, _, cx| cx.notify()).detach();
        // Keeps the countdowns of open grants current.
        cx.spawn(async move |this, cx| {
            loop {
                cx.background_executor().timer(Duration::from_secs(30)).await;
                if this.update(cx, |_, cx| cx.notify()).is_err() {
                    break;
                }
            }
        })
        .detach();
        let mut shell = Shell {
            dir,
            key_id: String::new(),
            device: None,
            screen,
            now: Now::default(),
            exchanges: Vec::new(),
            identities: Vec::new(),
            connectors: Vec::new(),
            keys: Keys::default(),
            hidden: Vec::new(),
            server: String::new(),
            search: None,
            adjust: HashMap::new(),
            picker: None,
            older: Vec::new(),
            older_more: false,
            stopping: false,
            confirming: None,
            notice: None,
            error: None,
            prompt: None,
            #[cfg(not(target_os = "android"))]
            unlock: None,
            pending: None,
            focus: cx.focus_handle(),
        };
        if let Some(device) = device {
            shell.paired(device, cx);
        }
        shell
    }

    fn paired(&mut self, device: Device, cx: &mut Context<Self>) {
        self.key_id = device.key.key("").id;
        let saved: Option<Value> = std::fs::read(self.dir.join(device::FILE))
            .ok()
            .and_then(|b| serde_json::from_slice(&b).ok());
        self.server = saved.and_then(|v| Some(v["url"].as_str()?.to_string())).unwrap_or_default();
        self.device = Some(device);
        self.listen(cx);
    }

    /// A pairing link opened from outside the app: shown on the pairing
    /// screen for the owner to check, ignored once paired.
    pub fn take_link(&mut self, link: String, cx: &mut Context<Self>) {
        if self.device.is_none() {
            self.screen = Screen::Pair { text: link };
            cx.notify();
        }
    }

    /// Opens the waiting request, proposal or exchange with this ID once a
    /// refresh shows it; if the refresh does not, it was decided meanwhile.
    pub fn take_item(&mut self, id: String, cx: &mut Context<Self>) {
        if self.device.is_some() {
            self.pending = Some(id);
            self.refresh(cx);
        }
    }

    fn open_pending(&mut self, cx: &mut Context<Self>) {
        let Some(id) = self.pending.take() else { return };
        if let Some(ex) = self.exchanges.iter().find(|e| e.id == id) {
            self.screen = Screen::Code { exchange: ex.clone(), code: String::new() };
        } else if let Some(p) = self.now.proposals.iter().find(|p| p.id == id) {
            self.review(p.clone(), cx);
        } else if self.now.requests.iter().any(|r| r.id == id) {
            // Requests are decided on their card in Now.
            self.screen = Screen::Tab(Tab::Now);
        }
    }

    /// The paired device; only the pairing screen runs without one.
    fn device(&self) -> Device {
        self.device.clone().expect("paired")
    }

    fn go(&mut self, screen: Screen, cx: &mut Context<Self>) {
        self.error = None;
        self.notice = None;
        self.stopping = false;
        self.search = None;
        self.picker = None;
        self.confirming = None;
        self.screen = screen;
        keyboard(false);
        cx.notify();
    }

    /// Runs a blocking call off the UI thread, then done with what it
    /// answered; an error shows instead.
    fn call<T: Send + 'static>(
        &mut self,
        cx: &mut Context<Self>,
        work: impl FnOnce() -> Result<T, Error> + Send + 'static,
        done: impl FnOnce(&mut Self, T, &mut Context<Self>) + 'static,
    ) {
        cx.spawn(async move |this, cx| {
            let result = cx.background_spawn(async move { work() }).await;
            this.update(cx, |this, cx| {
                match result {
                    Ok(v) => done(this, v, cx),
                    Err(e) => (this.error, this.prompt) = (Some(e.to_string()), None),
                }
                cx.notify();
            })
        })
        .detach();
    }

    /// Holds the event stream open on its own thread, refetching on
    /// connecting and on every event.
    fn listen(&mut self, cx: &mut Context<Self>) {
        let device = self.device();
        let (tx, mut rx) = futures::channel::mpsc::unbounded();
        std::thread::spawn(move || {
            while !tx.is_closed() {
                match device.client.events() {
                    Ok(events) => {
                        let _ = tx.unbounded_send(None);
                        for event in events {
                            if event.is_err() || tx.unbounded_send(None).is_err() {
                                break;
                            }
                        }
                    }
                    Err(e) => {
                        let _ = tx.unbounded_send(Some(e.to_string()));
                    }
                }
                std::thread::sleep(Duration::from_secs(5));
            }
        });
        cx.spawn(async move |this, cx| {
            while let Some(error) = rx.next().await {
                let update = this.update(cx, |this, cx| match error {
                    Some(e) => {
                        this.error = Some(e);
                        cx.notify();
                    }
                    None => this.refresh(cx),
                });
                if update.is_err() {
                    break;
                }
            }
        })
        .detach();
    }

    fn refresh(&mut self, cx: &mut Context<Self>) {
        let client = self.device().client;
        self.call(
            cx,
            move || {
                let now: Now = client.now(None, None)?;
                let exchanges: Vec<Exchange> = client.get("exchanges")?;
                let identities: Vec<Identity> = client.get("identities")?;
                let connectors: Vec<Connector> = client.get("connectors")?;
                let keys: Keys = client.get("keys")?;
                let hidden: Vec<Hidden> = client.get("never-share")?;
                Ok((now, exchanges, identities, connectors, keys, hidden))
            },
            |this, (now, exchanges, identities, connectors, keys, hidden), cx| {
                this.adjust.retain(|id, _| now.requests.iter().any(|r| &r.id == id));
                (this.now, this.exchanges, this.identities, this.connectors) =
                    (now, exchanges, identities, connectors);
                (this.keys, this.hidden) = (keys, hidden);
                this.open_pending(cx);
            },
        );
    }

    /// Makes the device key for next: on Android at once, on desktop with
    /// the authenticator or token the owner picks.
    fn new_key(&mut self, next: Next, cx: &mut Context<Self>) {
        self.error = None;
        #[cfg(target_os = "android")]
        self.call(cx, android::Keystore::generate, move |this, key, cx| {
            this.with_key(next, key, cx)
        });
        #[cfg(not(target_os = "android"))]
        self.go(Screen::Choose(choose::Choose::new(next)), cx);
    }

    fn with_key(&mut self, next: Next, key: device::Key, cx: &mut Context<Self>) {
        let dir = self.dir.clone();
        match next {
            Next::Pair(link) => self.call(
                cx,
                move || Device::pair(&dir, &link, key),
                |this, device, cx| {
                    this.paired(device, cx);
                    this.go(Screen::Tab(Tab::Now), cx);
                },
            ),
            Next::Join(url) => self.call(
                cx,
                move || {
                    let token = new_token();
                    let join = Join::start(&url, &key, &device::name(), &token)?;
                    Ok((join, key, token, url))
                },
                |this, (join, key, token, url), cx| {
                    let code = join.code().to_string();
                    this.go(Screen::Joining { code, status: "" }, cx);
                    this.poll_join(join, key, token, url, cx);
                },
            ),
        }
    }

    /// Follows the join on its own thread until the owner lets this device
    /// in, or it ends.
    fn poll_join(
        &mut self,
        mut join: Join,
        key: device::Key,
        token: String,
        url: String,
        cx: &mut Context<Self>,
    ) {
        let dir = self.dir.clone();
        let (tx, mut rx) = futures::channel::mpsc::unbounded();
        std::thread::spawn(move || {
            let done = loop {
                match join.poll() {
                    Ok(JoinStatus::Approved) => {
                        break Device::joined(&dir, &url, join, key, &token);
                    }
                    Ok(JoinStatus::Ended) => {
                        break Err(Error::Invalid(
                            "The join ended: three wrong codes, or 10 minutes passed. Start again."
                                .into(),
                        ));
                    }
                    Ok(status) => {
                        if tx.unbounded_send(Err(status)).is_err() {
                            return;
                        }
                    }
                    Err(e) => break Err(e),
                }
            };
            let _ = tx.unbounded_send(Ok(done));
        });
        cx.spawn(async move |this, cx| {
            while let Some(update) = rx.next().await {
                let _ = this.update(cx, |this, cx| {
                    match update {
                        Ok(Ok(device)) => {
                            this.paired(device, cx);
                            this.go(Screen::Tab(Tab::Now), cx);
                        }
                        Ok(Err(e)) => {
                            this.go(Screen::Pair { text: String::new() }, cx);
                            this.error = Some(e.to_string());
                        }
                        Err(status) => {
                            if let Screen::Joining { status: shown, .. } = &mut this.screen {
                                *shown = match status {
                                    JoinStatus::WrongCode => {
                                        "That code didn't match. Type it again on the other device."
                                    }
                                    JoinStatus::Confirmed => {
                                        "The code matches. Let this device in on the other one."
                                    }
                                    _ => "",
                                };
                            }
                        }
                    }
                    cx.notify();
                });
            }
        })
        .detach();
    }

    /// Asks for the device key's PIN when signing needs it, then runs then;
    /// shows what the key waits for while it signs. False if it asked.
    #[cfg(not(target_os = "android"))]
    fn ready(
        &mut self,
        then: impl FnOnce(&mut Shell, &mut Context<Shell>) + 'static,
        cx: &mut Context<Self>,
    ) -> bool {
        let key = self.device().key;
        if key.locked() {
            self.unlock = Some(Unlock { pin: String::new(), then: Box::new(then) });
            cx.notify();
            return false;
        }
        self.prompt = key.touch().then_some("Touch your security key.");
        true
    }

    #[cfg(not(target_os = "android"))]
    fn submit_pin(&mut self, cx: &mut Context<Self>) {
        let Some(unlock) = self.unlock.take() else { return };
        let key = self.device().key;
        let pin = unlock.pin;
        self.error = None;
        self.call(cx, move || key.unlock(&pin), move |this, (), cx| (unlock.then)(this, cx));
    }

    /// Fetches a challenge, checks that its record says what the owner was
    /// shown, then signs and posts it.
    fn sign(
        &mut self,
        path: String,
        body: Value,
        check: impl FnOnce(&Record) -> bool + Clone + Send + 'static,
        cx: &mut Context<Self>,
    ) {
        #[cfg(not(target_os = "android"))]
        {
            let (path, body, check) = (path.clone(), body.clone(), check.clone());
            if !self.ready(move |this, cx| this.sign(path, body, check, cx), cx) {
                return;
            }
        }
        let device = self.device();
        let decided = path.clone();
        self.error = None;
        self.call(
            cx,
            move || {
                let mut envelope = device.client.challenge(&path, &body)?;
                if !check(&envelope.record()?) {
                    return Err(Error::Invalid(
                        "What the server asked to sign isn't what you were shown, so nothing was signed."
                            .into(),
                    ));
                }
                envelope.sign(&*device.key)?;
                device.client.signed(&envelope)
            },
            move |this, _, cx| {
                this.prompt = None;
                this.decided(&decided);
                this.screen = Screen::Tab(Tab::Now);
                this.refresh(cx);
            },
        );
    }

    /// Drops a request, proposal or exchange the owner just decided, named
    /// by any route under it, so its card leaves Now before the refresh.
    fn decided(&mut self, path: &str) {
        let mut parts = path.split('/');
        let (Some(kind), Some(id)) = (parts.next(), parts.next()) else { return };
        match kind {
            "requests" => self.now.requests.retain(|r| r.id != id),
            "proposals" => self.now.proposals.retain(|p| p.id != id),
            "exchanges" => self.exchanges.retain(|e| e.id != id),
            _ => {}
        }
    }

    /// Signs a record already shown in full: an admit or a reviewed send.
    fn approve(&mut self, mut envelope: Envelope, cx: &mut Context<Self>) {
        #[cfg(not(target_os = "android"))]
        {
            let envelope = envelope.clone();
            if !self.ready(move |this, cx| this.approve(envelope, cx), cx) {
                return;
            }
        }
        let device = self.device();
        let decided = match &self.screen {
            Screen::Review { proposal, .. } => format!("proposals/{}", proposal.id),
            Screen::Admit { exchange, .. } => format!("exchanges/{}", exchange.id),
            _ => String::new(),
        };
        self.error = None;
        self.call(
            cx,
            move || {
                envelope.sign(&*device.key)?;
                device.client.signed(&envelope)
            },
            move |this, _, cx| {
                this.prompt = None;
                this.decided(&decided);
                this.screen = Screen::Tab(Tab::Now);
                this.refresh(cx);
            },
        );
    }

    fn approve_request(&mut self, r: &Request, cx: &mut Context<Self>) {
        let a = self.adjust.get(&r.id).cloned().unwrap_or_else(|| Adjust::of(r));
        let named = r.terms.is_some() || !a.chats.is_empty();
        let mut body = json!({ "horizon": a.horizon });
        if named {
            body["chats"] = json!(a.chats);
        }
        match r.kind.as_str() {
            "routine" => body["zone"] = words::zone().into(),
            "once" => body["for"] = a.for_.into(),
            _ => body = json!({}),
        }
        let (connector, kind) = (r.connector.clone(), r.kind.clone());
        let check = move |record: &Record| {
            let Record::Grant(g) = record else { return false };
            let window = match kind.as_str() {
                "once" => {
                    g.until.zip(g.from).is_some_and(|(u, f)| (u.unix() - f.unix()) as u64 == a.for_)
                }
                "routine" => g.schedule.is_some(),
                _ => return g.connector == connector,
            };
            let chats = if named {
                g.chats.iter().cloned().collect::<BTreeSet<_>>() == a.chats
            } else {
                g.chats.is_empty()
            };
            g.connector == connector && chats && g.horizon == a.horizon && window
        };
        self.sign(format!("requests/{}/challenge", r.id), body, check, cx);
    }

    fn approve_send(&mut self, p: &Proposal, cx: &mut Context<Self>) {
        let (connector, display) = (p.connector.clone(), p.display.clone());
        let check = move |record: &Record| matches!(record, Record::Write(w) if w.connector == connector && w.display == display);
        self.sign(format!("proposals/{}/challenge", p.id), json!({"edits": {}}), check, cx);
    }

    fn review(&mut self, proposal: Proposal, cx: &mut Context<Self>) {
        let client = self.device().client;
        let path = format!("proposals/{}/challenge", proposal.id);
        self.error = None;
        self.call(
            cx,
            move || client.challenge(&path, &json!({"edits": {}})),
            |this, envelope, _| this.screen = Screen::Review { proposal, envelope },
        );
    }

    /// Sends a route that answers JSON, then refreshes, with notice saying
    /// what happened.
    fn post(
        &mut self,
        method: &'static str,
        path: String,
        cx: &mut Context<Self>,
        notice: impl FnOnce(Value) -> Option<String> + 'static,
    ) {
        let client = self.device().client;
        let decided = path.clone();
        self.error = None;
        self.call(
            cx,
            move || client.send::<Value>(method, &path, &json!({})),
            move |this, answer, cx| {
                this.decided(&decided);
                this.screen = Screen::Tab(match this.screen {
                    Screen::Tab(tab) => tab,
                    _ => Tab::Now,
                });
                (this.stopping, this.confirming, this.notice) = (false, None, notice(answer));
                this.refresh(cx);
            },
        );
    }

    fn stop_all(&mut self, cx: &mut Context<Self>) {
        #[derive(Deserialize)]
        struct Stopped {
            grants: u64,
            requests: u64,
        }
        self.post("POST", "stop-all".into(), cx, |v| {
            let s: Stopped = serde_json::from_value(v).ok()?;
            let n = |n: u64, what: &str| format!("{n} {what}{}", if n == 1 { "" } else { "s" });
            Some(format!(
                "Stopped: {} closed, {} denied.",
                n(s.grants, "grant"),
                n(s.requests, "request")
            ))
        });
    }

    fn digit(&mut self, d: char, cx: &mut Context<Self>) {
        let Screen::Code { exchange, code } = &mut self.screen else { return };
        if code.len() == 6 {
            code.clear();
        }
        code.push(d);
        if code.len() < 6 {
            return;
        }
        let (id, code, name) = (exchange.id.clone(), code.clone(), exchange.name.clone());
        let client = self.device().client;
        self.error = None;
        self.call(
            cx,
            move || {
                let payload = client.exchange_code(&id, &code)?;
                let envelope =
                    client.challenge(&format!("exchanges/{id}/challenge"), &json!({}))?;
                if !payload.matches(&envelope.record()?) {
                    return Err(Error::Invalid(format!(
                        "What the server asked to sign doesn't let {name} in, so nothing was signed."
                    )));
                }
                Ok(envelope)
            },
            |this, envelope, _| {
                if let Screen::Code { exchange, .. } = &this.screen {
                    this.screen = Screen::Admit { exchange: exchange.clone(), envelope };
                }
            },
        );
    }

    fn key(&mut self, ev: &KeyDownEvent, _: &mut Window, cx: &mut Context<Self>) {
        let k = &ev.keystroke;
        let typed = k.key_char.as_deref().unwrap_or_default();
        if let Some(p) = &mut self.picker {
            if k.key == "backspace" {
                p.q.pop();
            } else {
                p.q.push_str(typed);
            }
            self.find_chats(cx);
            return cx.notify();
        }
        #[cfg(not(target_os = "android"))]
        if let Some(unlock) = &mut self.unlock {
            match k.key.as_str() {
                "enter" => self.submit_pin(cx),
                "escape" => self.unlock = None,
                _ => type_into(&mut unlock.pin, ev, cx),
            }
            return cx.notify();
        }
        match (&mut self.screen, &mut self.search) {
            #[cfg(not(target_os = "android"))]
            (Screen::Choose(_), _) => return self.choose_key(ev, cx),
            (Screen::Link { focus: Some(_), .. }, _) if k.key == "enter" => self.submit_step(cx),
            (Screen::Link { values, focus: Some(f), step: Some(step), .. }, _) => {
                let number = step.fields.iter().any(|x| &x.name == f && x.kind == "number");
                let value = values.entry(f.clone()).or_default();
                if k.key == "backspace" {
                    value.pop();
                } else if !number || typed.chars().all(|c| c.is_ascii_digit()) {
                    value.push_str(typed);
                }
            }
            (Screen::Tab(_), Some(q)) if k.key == "backspace" => {
                q.pop();
            }
            (Screen::Tab(_), Some(q)) => q.push_str(typed),
            (Screen::Join { url }, _) if k.key == "enter" => {
                let url = url.trim().to_string();
                self.new_key(Next::Join(url), cx);
            }
            (Screen::Pair { text } | Screen::Join { url: text }, _) => type_into(text, ev, cx),
            (Screen::Code { code, .. }, _) if k.key == "backspace" => {
                code.pop();
            }
            (Screen::Code { .. }, _) => {
                if let Some(d) = typed.chars().next()
                    && d.is_ascii_digit()
                {
                    self.digit(d, cx);
                }
            }
            _ => return,
        }
        cx.notify();
    }

    /// Whether text belongs on the current page while searching.
    fn shown(&self, text: &str) -> bool {
        self.search.as_deref().is_none_or(|q| text.to_lowercase().contains(&q.to_lowercase()))
    }
}

/// Types into a one-line field: characters, backspace, and paste.
fn type_into(text: &mut String, ev: &KeyDownEvent, cx: &App) {
    let k = &ev.keystroke;
    if k.modifiers.secondary() && k.key == "v" {
        let pasted = cx.read_from_clipboard().and_then(|c| c.text());
        text.push_str(pasted.unwrap_or_default().trim());
    } else if k.key == "backspace" {
        text.pop();
    } else {
        text.push_str(k.key_char.as_deref().unwrap_or_default());
    }
}

/// What waits for the owner, for notifications: each item's ID and a line
/// naming only its identity, connector and kind (owner-api.md).
fn waiting(client: &Client) -> Result<Vec<(String, String)>, Error> {
    let now: Now = client.now(None, None)?;
    let exchanges: Vec<Exchange> = client.get("exchanges")?;
    let exchanges = exchanges.into_iter().map(|e| {
        let text = if e.kind == "device" {
            screens::DEVICE_JOIN.to_string()
        } else {
            format!("{} wants to log in", e.name)
        };
        (e.id, text)
    });
    let requests = now
        .requests
        .into_iter()
        .map(|r| (r.id, format!("{} wants to read {}", r.identity_name, r.connector)));
    let proposals = now
        .proposals
        .into_iter()
        .map(|p| (p.id, format!("{} wants to {} on {}", p.identity_name, p.op, p.connector)));
    Ok(exchanges.chain(requests).chain(proposals).collect())
}

/// Shows or hides the phone's keyboard as a text field opens or closes.
fn keyboard(show: bool) {
    #[cfg(target_os = "android")]
    android::keyboard(show);
    #[cfg(not(target_os = "android"))]
    let _ = show;
}

/// One path segment, percent-encoded.
fn segment(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for b in s.bytes() {
        match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'.' | b'_' | b'~' => {
                out.push(b as char)
            }
            b => out.push_str(&format!("%{b:02X}")),
        }
    }
    out
}

/// The bundled Lucide icons (ISC), drawn in the text color.
pub struct Assets;

impl gpui::AssetSource for Assets {
    fn load(&self, path: &str) -> gpui::Result<Option<Cow<'static, [u8]>>> {
        let svg: &'static [u8] = match path {
            "icons/activity.svg" => include_bytes!("../assets/icons/activity.svg"),
            "icons/arrow-left.svg" => include_bytes!("../assets/icons/arrow-left.svg"),
            "icons/bell.svg" => include_bytes!("../assets/icons/bell.svg"),
            "icons/bot.svg" => include_bytes!("../assets/icons/bot.svg"),
            "icons/plug.svg" => include_bytes!("../assets/icons/plug.svg"),
            "icons/search.svg" => include_bytes!("../assets/icons/search.svg"),
            "icons/settings.svg" => include_bytes!("../assets/icons/settings.svg"),
            "icons/x.svg" => include_bytes!("../assets/icons/x.svg"),
            _ => return Ok(None),
        };
        Ok(Some(Cow::Borrowed(svg)))
    }

    fn list(&self, _: &str) -> gpui::Result<Vec<gpui::SharedString>> {
        Ok(Vec::new())
    }
}

/// Opens the app's one window; dir keeps the paired device.
pub fn open(cx: &mut App, options: WindowOptions, dir: PathBuf) -> Entity<Shell> {
    cx.text_system()
        .add_fonts(theme::FONTS.iter().map(|f| Cow::Borrowed(*f)).collect())
        .expect("the bundled fonts load");
    let mut opened = None;
    cx.open_window(options, |window, cx| {
        let shell = cx.new(|cx| Shell::new(dir, window, cx));
        let focus = shell.read(cx).focus.clone();
        window.focus(&focus, cx);
        opened = Some(shell.clone());
        shell
    })
    .expect("open window");
    opened.expect("the window's view")
}
