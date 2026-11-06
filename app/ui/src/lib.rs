#[cfg(target_os = "android")]
mod android;
mod api;
#[cfg(not(target_os = "android"))]
mod choose;
#[cfg(not(target_os = "android"))]
pub mod desktop;
mod device;
mod fido;
mod link;
mod now;
mod pick;
mod screens;
mod scroll;
mod tabs;
mod theme;
mod words;

use api::{Connector, Exchange, Hidden, Identity, Keys, Now, Opened, Proposal, Request, Step};
use device::Device;
use futures::StreamExt;
use gpui::{
    App, AppContext, Context, Entity, FocusHandle, KeyDownEvent, ScrollHandle, Window,
    WindowOptions,
};
use mandate_core::{
    Client, DeviceKey, Envelope, Error, Join, JoinStatus, PairLink, Record, Signed, new_token,
};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::borrow::Cow;
use std::collections::{BTreeMap, BTreeSet, HashMap};
use std::mem::Discriminant;
use std::path::PathBuf;
use std::time::Duration;

/// Why a join ended, and what to do next.
const JOIN_ENDED: &str = "That code no longer works. Get a new one from Add a device.";

/// What a security key waits for while it signs.
#[cfg(target_os = "android")]
const TOUCH: &str = "Plug in your security key or hold it to the back of the phone, then touch it.";
#[cfg(not(target_os = "android"))]
const TOUCH: &str = "Touch your security key.";

/// How long to wait before trying an unreachable server again, at first
/// and at most; each failure doubles the wait.
const RETRY_FIRST: Duration = Duration::from_secs(2);
const RETRY_MAX: Duration = Duration::from_secs(30);

/// What a refresh fetches, by route; the last answers are kept on disk so
/// that a start without the server still shows them.
const ROUTES: [&str; 7] =
    ["now", "exchanges", "identities", "connectors", "keys", "never-share", "server"];
const CACHE: &str = "cache.json";

/// A page as drawn: the screen's kind, and the tab on the tabs.
type Page = (Discriminant<Screen>, Option<Tab>);

#[derive(Clone, Copy, PartialEq)]
enum Tab {
    Now,
    Agents,
    Connections,
    Activity,
}

enum Screen {
    /// What mandate is, before pairing or joining.
    Welcome,
    Pair {
        text: String,
    },
    /// Joining as another device: the server's address and the code that a
    /// device already in shows, as typed; code_field says which takes typing.
    Join {
        url: String,
        code: String,
        code_field: bool,
    },
    /// The code matched; the device already in lets this one in.
    Joining,
    /// Picking this computer's device key.
    #[cfg(not(target_os = "android"))]
    Choose(choose::Choose),
    Tab(Tab),
    Settings,
    /// An exchange the owner opened: the code to give an agent or a new
    /// device ("agent" or "device"), until its code matches.
    Invite {
        kind: &'static str,
        opened: Opened,
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
    /// How this device approves: picked before its key is made, when it
    /// pairs, joins or changes its key.
    #[cfg(target_os = "android")]
    Method {
        next: Next,
        /// The new security key's PIN, while it is typed.
        pin: Option<String>,
    },
    /// One entry of Activity, in full.
    Entry(std::rc::Rc<api::Activity>),
}

/// What a new device key is for: pairing as the first device, joining the
/// server at a URL by a code as another, or another way this device
/// approves.
#[derive(Clone)]
enum Next {
    Pair(PairLink),
    Join { url: String, code: String },
    Add,
}

/// An action asked once more in a sheet before it runs; the ones that take
/// access away also want the owner to prove it is them.
#[derive(Clone)]
enum Confirm {
    DeleteAgent {
        id: String,
        name: String,
    },
    /// Every key of a device: one remove each.
    RemoveDevice {
        keys: Vec<String>,
        name: String,
    },
    /// One way this device approves, not the one that signs.
    DropMethod {
        id: String,
        title: String,
    },
    Unlink {
        name: String,
    },
    Close {
        id: String,
        what: String,
    },
    StopAll,
}

/// The device key's PIN, asked before a signature or, for a gate, before
/// the key proves the owner is here; then what follows.
struct Unlock {
    pin: String,
    then: Then,
    gate: bool,
}

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
    /// History loaded so far, newest first; None until Activity first shows.
    history: Option<tabs::Loaded>,
    history_loading: bool,
    /// What Activity asks for: search, kinds and the day it starts at. The
    /// loaded history shows until a page for this arrives.
    wanted: tabs::HistoryQuery,
    /// The search text as last typed, applied once typing pauses.
    typed: String,
    /// Activity's sheet for its kinds or its day, while open.
    history_sheet: Option<tabs::HistorySheet>,
    /// The days with entries in the span the day picker shows.
    marks: Option<tabs::Marks>,
    /// Where each day's header of Activity sat on the last frame, for the
    /// one that sticks on top.
    day_tops: std::rc::Rc<std::cell::RefCell<Vec<(String, f32)>>>,
    /// The page's scroll, for loading more history near its end.
    scroll: ScrollHandle,
    /// The action whose sheet asks for a second tap.
    sheet: Option<Confirm>,
    notice: Option<String>,
    error: Option<String>,
    /// What the device key waits for while it signs, e.g. a touch.
    prompt: Option<&'static str>,
    unlock: Option<Unlock>,
    /// A waiting item to open once a refresh shows it, from a notification.
    pending: Option<String>,
    /// The server can't be reached: one notice says so, the last content
    /// stays, and a refresh tries again after backoff.
    offline: bool,
    backoff: Duration,
    /// The bottom inset with the keyboard down, which sheets sit on.
    nav_bottom: f32,
    retrying: bool,
    /// Wakes the event stream's thread to connect again at once.
    nudge: Option<std::sync::mpsc::Sender<()>>,
    /// The last answers of the server, by route, as kept on disk.
    cache: serde_json::Map<String, Value>,
    focus: FocusHandle,
    /// The wordmark shows alone for a moment after Android's splash.
    launching: bool,
    /// How long the launch runs, fixed at its first frame.
    launch: Option<Duration>,
    /// The page last drawn, and how many pages have been, so that a new
    /// one fades in.
    page: (Option<Page>, usize),
    /// The pairing link last checked before pairing, and whether its
    /// certificate is verified: None while the check runs.
    pair_check: Option<(PairLink, Option<bool>)>,
    /// What agents log in to, as the server says (`GET /owner/v1/server`).
    agent_url: String,
    /// The device key a refused join made, for the next try.
    join_key: Option<device::Key>,
    /// The screens Back returns to, the latest last; empty at a tab's root.
    back: Vec<Screen>,
    /// The exchange on its way to Let in, so that it goes there once.
    admitting: Option<String>,
    /// A signature is in flight; no other starts until it ends.
    signing: bool,
    /// An exchange is being opened; a second tap waits for it.
    opening: bool,
    /// This device's new name, while it is typed in Settings.
    renaming: Option<String>,
    /// The owner's choice to lock mandate when it leaves the screen.
    lock: bool,
    /// Locked until the owner proves it is them.
    locked: bool,
    /// Asking the owner to unlock, so the lock screen asks only once.
    unlocking: bool,
}

impl Shell {
    fn new(dir: PathBuf, window: &mut Window, cx: &mut Context<Self>) -> Self {
        let device = Device::load(&dir);
        let lock = Prefs::load(&dir).lock && device.is_some();
        let screen = match device {
            Some(_) => Screen::Tab(Tab::Now),
            None => Screen::Welcome,
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
            history: None,
            history_loading: false,
            wanted: tabs::HistoryQuery::default(),
            typed: String::new(),
            history_sheet: None,
            marks: None,
            day_tops: Default::default(),
            scroll: ScrollHandle::new(),
            sheet: None,
            notice: None,
            error: None,
            prompt: None,
            unlock: None,
            pending: None,
            offline: false,
            backoff: RETRY_FIRST,
            nav_bottom: 0.,
            retrying: false,
            nudge: None,
            cache: serde_json::Map::new(),
            focus: cx.focus_handle(),
            launching: true,
            launch: None,
            page: (None, 0),
            pair_check: None,
            agent_url: String::new(),
            join_key: None,
            back: Vec::new(),
            admitting: None,
            signing: false,
            opening: false,
            renaming: None,
            lock,
            locked: lock,
            unlocking: false,
        };
        if let Some(device) = device {
            shell.paired(device, cx);
        }
        shell
    }

    fn paired(&mut self, device: Device, cx: &mut Context<Self>) {
        let saved: Option<Value> = std::fs::read(self.dir.join(device::FILE))
            .ok()
            .and_then(|b| serde_json::from_slice(&b).ok());
        self.server = saved.and_then(|v| Some(v["url"].as_str()?.to_string())).unwrap_or_default();
        self.device = Some(device);
        // What this server last answered shows until it answers again.
        let cache: Option<serde_json::Map<String, Value>> =
            std::fs::read(self.dir.join(CACHE)).ok().and_then(|b| serde_json::from_slice(&b).ok());
        if let Some(cache) = cache.filter(|c| c.get("url") == Some(&json!(self.server))) {
            self.cache = cache;
            self.show_cache(cx);
            if let Some(page) = self.cache.get("activity").cloned() {
                self.history = Some(tabs::Loaded::from_page(page));
            }
        }
        self.listen(cx);
    }

    /// The app is back on screen after being away: locked again if the owner
    /// chose so, and the server tried again at once if it was out of reach.
    pub fn foreground(&mut self, cx: &mut Context<Self>) {
        if self.lock && self.device.is_some() {
            (self.locked, self.unlocking) = (true, false);
            cx.notify();
        }
        if self.offline {
            if let Some(nudge) = &self.nudge {
                let _ = nudge.send(());
            }
            self.refresh(cx);
        }
    }

    /// Shows what failed: a server out of reach as the one notice, tried
    /// again after a backoff until it answers; anything else as an error.
    fn fail(&mut self, e: Error, cx: &mut Context<Self>) {
        if !matches!(e, Error::Io(_)) || self.device.is_none() {
            self.error = Some(e.to_string());
            return;
        }
        self.offline = true;
        if self.retrying {
            return;
        }
        self.retrying = true;
        let wait = self.backoff;
        self.backoff = (self.backoff * 2).min(RETRY_MAX);
        cx.spawn(async move |this, cx| {
            cx.background_executor().timer(wait).await;
            this.update(cx, |this, cx| {
                this.retrying = false;
                if this.offline {
                    this.refresh(cx);
                }
            })
        })
        .detach();
    }

    /// The server answered: the notice goes, and the backoff starts over.
    fn reached(&mut self) {
        (self.offline, self.backoff) = (false, RETRY_FIRST);
    }

    /// Keeps answers of the server on disk, by route, for the next start.
    fn keep(&mut self, answers: impl IntoIterator<Item = (String, Value)>) {
        self.cache.insert("url".into(), json!(self.server));
        self.cache.extend(answers);
        let cache = serde_json::to_vec(&self.cache).unwrap_or_default();
        let _ = std::fs::write(self.dir.join(CACHE), cache);
    }

    /// Asks the owner to prove it is them, and unlocks the app.
    pub(crate) fn unlock_app(&mut self, cx: &mut Context<Self>) {
        self.unlocking = true;
        self.gate(
            |this, cx| {
                (this.locked, this.unlocking) = (false, false);
                cx.notify();
            },
            cx,
        );
    }

    /// Turns the app lock on or off, kept on this device.
    pub(crate) fn set_lock(&mut self, on: bool, cx: &mut Context<Self>) {
        self.lock = on;
        if let Err(e) = (Prefs { lock: on }).save(&self.dir) {
            self.error = Some(e.to_string());
        }
        cx.notify();
    }

    /// A pairing link opened from outside the app: shown on the pairing
    /// screen for the owner to check, ignored once paired.
    pub fn take_link(&mut self, link: String, cx: &mut Context<Self>) {
        if self.device.is_none() {
            let screen = Screen::Pair { text: link };
            if matches!(self.screen, Screen::Pair { .. }) {
                self.swap(screen, cx);
            } else {
                self.go(screen, cx);
            }
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
        if let Some(ex) = self.exchanges.iter().find(|e| e.id == id && e.state == "confirmed") {
            self.admit_exchange(ex.clone(), cx);
        } else if let Some(p) = self.now.proposals.iter().find(|p| p.id == id) {
            self.review(p.clone(), cx);
        } else if self.now.requests.iter().any(|r| r.id == id) {
            // Requests are decided on their card in Now.
            self.root(Tab::Now, cx);
        }
    }

    /// Opens a code exchange for an agent or a new device, and shows its
    /// code. Opening another of the same kind closes the earlier one.
    fn invite(&mut self, kind: &'static str, cx: &mut Context<Self>) {
        // A second tap would open a second exchange, closing the first
        // while its code shows.
        if self.opening {
            return;
        }
        self.opening = true;
        let client = self.device().client;
        self.error = None;
        self.call(
            cx,
            move || Ok(client.send::<Opened>("POST", "exchanges", &json!({ "kind": kind }))),
            move |this, opened, cx| {
                this.opening = false;
                match opened {
                    Ok(opened) => this.go(Screen::Invite { kind, opened }, cx),
                    Err(e) => this.fail(e, cx),
                }
            },
        );
    }

    /// Cancels the exchange the owner opened, back where it was opened from.
    fn cancel_invite(&mut self, cx: &mut Context<Self>) {
        let Screen::Invite { opened, .. } = &self.screen else { return };
        let (client, path) = (self.device().client, format!("exchanges/{}", opened.id));
        self.call(
            cx,
            move || client.send::<Value>("DELETE", &path, &json!({})),
            |this, _, cx| this.up(cx),
        );
    }

    /// Once the code shown on Invite matched, goes on to letting the new
    /// party in.
    fn follow_invite(&mut self, cx: &mut Context<Self>) {
        let Screen::Invite { opened, .. } = &self.screen else { return };
        let id = &opened.id;
        if let Some(ex) = self.exchanges.iter().find(|e| &e.id == id && e.state == "confirmed") {
            self.admit_exchange(ex.clone(), cx);
        }
    }

    /// Fetches the record that lets a confirmed exchange's party in, checks
    /// it against the payload the party sent, and shows it to sign.
    fn admit_exchange(&mut self, exchange: Exchange, cx: &mut Context<Self>) {
        // Refreshes overlap; each exchange goes on to Let in once, until
        // that fails or the owner leaves it.
        if self.admitting.as_ref() == Some(&exchange.id) {
            return;
        }
        self.admitting = Some(exchange.id.clone());
        let client = self.device().client;
        self.error = None;
        let fetch = move || {
            let refused = || {
                Error::Invalid(format!(
                    "Nothing was signed: the server asked for something other than letting {} in.",
                    exchange.name
                ))
            };
            let payload = exchange.payload.clone().ok_or_else(refused)?;
            let path = format!("exchanges/{}/challenge", exchange.id);
            let envelope = client.challenge(&path, &json!({}))?;
            if !payload.matches(&envelope.record()?) {
                return Err(refused());
            }
            Ok((exchange, envelope))
        };
        self.call(
            cx,
            move || Ok(fetch()),
            |this, fetched, cx| match fetched {
                // Let in takes Add an agent's place: Back leads past it.
                Ok((exchange, envelope)) => {
                    let admit = Screen::Admit { exchange, envelope };
                    if matches!(this.screen, Screen::Invite { .. }) {
                        this.swap(admit, cx)
                    } else {
                        this.go(admit, cx)
                    }
                }
                Err(e) => {
                    this.admitting = None;
                    this.fail(e, cx)
                }
            },
        );
    }

    /// The paired device; only the pairing screen runs without one.
    fn device(&self) -> Device {
        self.device.clone().expect("paired")
    }

    /// Goes on to screen; Back returns here.
    fn go(&mut self, screen: Screen, cx: &mut Context<Self>) {
        let here = std::mem::replace(&mut self.screen, screen);
        self.back.push(here);
        self.entered(cx);
    }

    /// Shows screen in this one's place; Back skips this one.
    fn swap(&mut self, screen: Screen, cx: &mut Context<Self>) {
        self.screen = screen;
        self.entered(cx);
    }

    /// A tab's first page, the way here forgotten.
    fn root(&mut self, tab: Tab, cx: &mut Context<Self>) {
        self.back.clear();
        self.swap(Screen::Tab(tab), cx);
    }

    /// Back one screen; false at the root, where Android's Back leaves.
    pub(crate) fn pop(&mut self, cx: &mut Context<Self>) -> bool {
        let Some(screen) = self.back.pop() else { return false };
        self.swap(screen, cx);
        true
    }

    /// Leaves a screen that is done: back where it was opened from, or a
    /// root if it was opened from nowhere.
    fn up(&mut self, cx: &mut Context<Self>) {
        if !self.pop(cx) {
            match self.device {
                Some(_) => self.root(Tab::Now, cx),
                None => self.swap(Screen::Welcome, cx),
            }
        }
    }

    /// After a decision: a screen made for it (Review, Let in, a new grant)
    /// is left; a tab or Settings stays.
    fn decided_here(&mut self, cx: &mut Context<Self>) {
        if matches!(self.screen, Screen::Review { .. } | Screen::Admit { .. } | Screen::Grant(_)) {
            self.up(cx);
        }
    }

    /// Back on Join, past any screen the try went through (a key chooser,
    /// Joining), showing screen in its place.
    fn at_join(&mut self, screen: Screen, cx: &mut Context<Self>) {
        if let Some(i) = self.back.iter().rposition(|s| matches!(s, Screen::Join { .. })) {
            self.back.truncate(i);
        }
        self.swap(screen, cx);
    }

    /// What every change of screen resets.
    fn entered(&mut self, cx: &mut Context<Self>) {
        if !matches!(self.screen, Screen::Admit { .. }) {
            self.admitting = None;
        }
        self.error = None;
        self.notice = None;
        self.search = None;
        self.picker = None;
        for a in self.adjust.values_mut() {
            a.open = false;
        }
        (self.sheet, self.renaming, self.unlock, self.history_sheet) = (None, None, None, None);
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
                    Err(e) => {
                        this.prompt = None;
                        this.fail(e, cx)
                    }
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
        let (tx, mut rx) = futures::channel::mpsc::unbounded::<Result<(), Error>>();
        let (nudge, woken) = std::sync::mpsc::channel();
        self.nudge = Some(nudge);
        std::thread::spawn(move || {
            let mut wait = RETRY_FIRST;
            while !tx.is_closed() {
                match device.client.events() {
                    Ok(events) => {
                        wait = RETRY_FIRST;
                        let _ = tx.unbounded_send(Ok(()));
                        for event in events {
                            match event {
                                // Silent past two of its pings, or cut: the
                                // server is out of reach until it answers.
                                Err(e @ Error::Io(_)) => {
                                    let _ = tx.unbounded_send(Err(e));
                                    break;
                                }
                                Err(_) => break,
                                Ok(_) if tx.unbounded_send(Ok(())).is_err() => break,
                                Ok(_) => {}
                            }
                        }
                    }
                    Err(e) => {
                        let _ = tx.unbounded_send(Err(e));
                        wait = (wait * 2).min(RETRY_MAX);
                    }
                }
                // The app coming back to the foreground connects at once.
                let _ = woken.recv_timeout(wait);
            }
        });
        cx.spawn(async move |this, cx| {
            while let Some(connected) = rx.next().await {
                let update = this.update(cx, |this, cx| match connected {
                    Ok(()) => this.refresh(cx),
                    Err(e) => {
                        this.fail(e, cx);
                        cx.notify();
                    }
                });
                if update.is_err() {
                    break;
                }
            }
        })
        .detach();
    }

    /// Fetches everything but Activity; a failed fetch leaves what showed.
    fn refresh(&mut self, cx: &mut Context<Self>) {
        let client = self.device().client;
        self.call(
            cx,
            move || ROUTES.iter().map(|r| Ok((r.to_string(), client.get::<Value>(r)?))).collect(),
            |this, answers: Vec<(String, Value)>, cx| {
                this.reached();
                this.keep(answers);
                this.show_cache(cx);
                if this.history.is_some() {
                    this.load_history(cx);
                }
            },
        );
    }

    /// Shows the server's last answers: those just fetched, or those kept
    /// from before.
    fn show_cache(&mut self, cx: &mut Context<Self>) {
        fn take<T: serde::de::DeserializeOwned + Default>(
            c: &serde_json::Map<String, Value>,
            route: &str,
        ) -> T {
            c.get(route).and_then(|v| T::deserialize(v).ok()).unwrap_or_default()
        }
        let c = &self.cache;
        let (now, server): (Now, api::Server) = (take(c, "now"), take(c, "server"));
        (self.exchanges, self.identities) = (take(c, "exchanges"), take(c, "identities"));
        (self.connectors, self.keys, self.hidden) =
            (take(c, "connectors"), take(c, "keys"), take(c, "never-share"));
        self.adjust.retain(|id, _| now.requests.iter().any(|r| &r.id == id));
        (self.now, self.agent_url) = (now, server.agent_url);
        self.label_keys(cx);
        self.open_pending(cx);
        self.follow_invite(cx);
    }

    /// Connects to a pairing link's server before the owner pairs, to show
    /// either its verified hostname or the key to compare.
    fn check_pair(&mut self, link: PairLink, cx: &mut Context<Self>) {
        self.pair_check = Some((link.clone(), None));
        let checked = link.clone();
        self.call(
            cx,
            move || checked.check(),
            move |this, verified, _| {
                if this.pair_check.as_ref().is_some_and(|(l, _)| *l == link) {
                    this.pair_check = Some((link, Some(verified)));
                }
            },
        );
    }

    /// Makes the device key for next, the way the owner picks: on Android
    /// a fingerprint, the screen lock or a security key, on desktop an
    /// authenticator or a token.
    fn new_key(&mut self, next: Next, cx: &mut Context<Self>) {
        self.error = None;
        #[cfg(target_os = "android")]
        self.go(Screen::Method { next, pin: None }, cx);
        #[cfg(not(target_os = "android"))]
        self.go(Screen::Choose(choose::Choose::new(next)), cx);
    }

    /// The ways this device could approve but does not yet, by the server's
    /// labels: on Android those the phone is set up for, on desktop a
    /// security key and a token.
    pub(crate) fn methods_left(&self) -> Vec<&'static str> {
        #[cfg(target_os = "android")]
        let all = {
            use android::Method::*;
            let set_up = |m: &android::Method| match m {
                Biometric => android::biometrics_enrolled(),
                ScreenLock => android::screen_lock_set(),
                SecurityKey => true,
            };
            [Biometric, ScreenLock, SecurityKey].into_iter().filter(set_up).map(|m| m.label())
        };
        #[cfg(not(target_os = "android"))]
        let all = ["security key", "token"].into_iter();
        let keys = self.device.as_ref().map(|d| d.keys.as_slice()).unwrap_or_default();
        all.filter(|l| !keys.iter().any(|k| k.label() == *l)).collect()
    }

    /// Makes a Keystore key that unlocks by method, or asks for a security
    /// key's PIN first, then goes on with the key.
    #[cfg(target_os = "android")]
    pub(crate) fn make_key(&mut self, next: Next, method: android::Method, cx: &mut Context<Self>) {
        self.error = None;
        if method == android::Method::SecurityKey {
            if let Screen::Method { pin, .. } = &mut self.screen {
                *pin = Some(String::new());
            }
            keyboard(true);
            return cx.notify();
        }
        self.call(
            cx,
            move || android::Keystore::generate(method),
            move |this, key, cx| this.with_key(next, key.into(), cx),
        );
    }

    /// Makes a credential on the security key the owner plugs in or holds
    /// to the phone, with the PIN typed on How you approve.
    #[cfg(target_os = "android")]
    pub(crate) fn make_fido(&mut self, cx: &mut Context<Self>) {
        let Screen::Method { next, pin: Some(pin) } = &self.screen else { return };
        if pin.is_empty() || self.prompt.is_some() {
            return;
        }
        let (next, pin) = (next.clone(), pin.clone());
        keyboard(false);
        (self.error, self.prompt) = (None, Some(TOUCH));
        self.call(
            cx,
            move || crate::fido::android::Fido::create(&pin, &device::name()),
            move |this, key, cx| {
                this.prompt = None;
                this.with_key(next, android::Key::Fido(key), cx)
            },
        );
    }

    fn with_key(&mut self, next: Next, key: device::Key, cx: &mut Context<Self>) {
        let dir = self.dir.clone();
        match next {
            Next::Add => self.add_method(key, cx),
            Next::Pair(link) => self.call(
                cx,
                move || Device::pair(&dir, &link, key),
                |this, device, cx| {
                    this.paired(device, cx);
                    this.root(Tab::Now, cx);
                },
            ),
            Next::Join { url, code } => {
                let typed = url.clone();
                // A refusal goes back to the Join screen, the address kept,
                // with what to do next; the key waits for the next try, as
                // a hardware key costs a PIN and a touch to make.
                let attempt = move || {
                    let token = new_token();
                    let tried = Join::start(&url, &key, &device::name(), &token)
                        .and_then(|mut join| Ok((join.answer(&code)?, join)));
                    let refused = match tried {
                        Ok((JoinStatus::Confirmed, join)) => return Ok((join, key, token, url)),
                        Ok((JoinStatus::WrongCode { attempts_left }, _)) => {
                            Error::Invalid(format!("Wrong code. {attempts_left} tries left."))
                        }
                        Ok(_) => Error::Invalid(JOIN_ENDED.into()),
                        Err(e) => e,
                    };
                    Err(Box::new((refused, key)))
                };
                self.call(
                    cx,
                    move || Ok(attempt()),
                    move |this, attempt, cx| match attempt {
                        Ok((join, key, token, url)) => {
                            this.at_join(Screen::Joining, cx);
                            this.poll_join(join, key, token, url, cx);
                        }
                        Err(refused) => {
                            let (e, key) = *refused;
                            let screen =
                                Screen::Join { url: typed, code: String::new(), code_field: true };
                            this.at_join(screen, cx);
                            (this.join_key, this.error) = (Some(key), Some(e.to_string()));
                        }
                    },
                )
            }
        }
    }

    /// Follows the join on its own thread, once the code matched, until the
    /// owner lets this device in or the exchange ends.
    fn poll_join(
        &mut self,
        mut join: Join,
        key: device::Key,
        token: String,
        url: String,
        cx: &mut Context<Self>,
    ) {
        let (dir, typed) = (self.dir.clone(), url.clone());
        // Long polls get their own thread, off the executor's.
        let (tx, rx) = futures::channel::oneshot::channel();
        std::thread::spawn(move || {
            let done = loop {
                match join.poll() {
                    Ok(JoinStatus::Approved) => {
                        break Device::joined(&dir, &url, join, key, &token);
                    }
                    Ok(JoinStatus::Confirmed) => {}
                    Ok(JoinStatus::Denied) => break Err(Error::Invalid("Not let in.".into())),
                    Ok(_) => break Err(Error::Invalid(JOIN_ENDED.into())),
                    Err(e) => break Err(e),
                }
            };
            let _ = tx.send(done);
        });
        cx.spawn(async move |this, cx| {
            let Ok(done) = rx.await else { return Ok(()) };
            this.update(cx, |this, cx| match done {
                Ok(device) => {
                    this.paired(device, cx);
                    this.root(Tab::Now, cx);
                }
                Err(e) => {
                    let screen = Screen::Join { url: typed, code: String::new(), code_field: true };
                    this.at_join(screen, cx);
                    this.error = Some(e.to_string());
                }
            })
        })
        .detach();
    }

    /// Asks for the device key's PIN when signing needs it, then runs then;
    /// shows what the key waits for while it signs. False if it asked.
    fn ready(
        &mut self,
        then: impl FnOnce(&mut Shell, &mut Context<Shell>) + 'static,
        cx: &mut Context<Self>,
    ) -> bool {
        let key = self.device().key;
        if key.locked() {
            self.unlock = Some(Unlock { pin: String::new(), then: Box::new(then), gate: false });
            keyboard(true);
            cx.notify();
            return false;
        }
        self.prompt = key.touch().then_some(TOUCH);
        true
    }

    fn submit_pin(&mut self, cx: &mut Context<Self>) {
        let Some(unlock) = self.unlock.take() else { return };
        let key = self.device().key;
        let (pin, gate) = (unlock.pin, unlock.gate);
        (self.error, self.prompt) = (None, (gate && key.touch()).then_some(TOUCH));
        keyboard(false);
        self.call(
            cx,
            move || if gate { key.confirm(Some(&pin)) } else { key.unlock(&pin) },
            move |this, (), cx| {
                this.prompt = None;
                (unlock.then)(this, cx)
            },
        );
    }

    /// Proves the owner is here the way this device approves, then runs
    /// then. Nothing is signed: a fingerprint or the screen lock, or a
    /// security key's PIN and a touch, or a token's PIN.
    fn gate(
        &mut self,
        then: impl FnOnce(&mut Shell, &mut Context<Shell>) + 'static,
        cx: &mut Context<Self>,
    ) {
        let key = self.device().key;
        self.error = None;
        if key.confirm_needs_pin() {
            self.unlock = Some(Unlock { pin: String::new(), then: Box::new(then), gate: true });
            keyboard(true);
            return cx.notify();
        }
        self.prompt = key.touch().then_some(TOUCH);
        self.call(
            cx,
            move || key.confirm(None),
            move |this, (), cx| {
                this.prompt = None;
                then(this, cx)
            },
        );
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
        self.sign_then(path, body, check, |_, _| {}, cx)
    }

    /// sign, then once the server took it, then.
    fn sign_then(
        &mut self,
        path: String,
        body: Value,
        check: impl FnOnce(&Record) -> bool + Clone + Send + 'static,
        then: impl FnOnce(&mut Shell, &mut Context<Shell>) + Clone + 'static,
        cx: &mut Context<Self>,
    ) {
        if self.signing {
            return;
        }
        let (p, b, c, t) = (path.clone(), body.clone(), check.clone(), then.clone());
        if !self.ready(move |this, cx| this.sign_then(p, b, c, t, cx), cx) {
            return;
        }
        let device = self.device();
        let decided = path.clone();
        self.signing_call(decided, then, cx, move || {
            let mut envelope = device.client.challenge(&path, &body)?;
            if !check(&envelope.record()?) {
                return Err(Error::Invalid(
                    "Nothing was signed: the server asked for something other than what you saw."
                        .into(),
                ));
            }
            envelope.sign(&*device.key)?;
            device.client.signed(&envelope)
        });
    }

    /// Runs a signature and what follows it. One runs at a time, app-wide,
    /// so two prompts for the device key never race; a second is dropped.
    fn signing_call(
        &mut self,
        decided: String,
        then: impl FnOnce(&mut Shell, &mut Context<Shell>) + 'static,
        cx: &mut Context<Self>,
        work: impl FnOnce() -> Result<Signed, Error> + Send + 'static,
    ) {
        if self.signing {
            return;
        }
        self.signing = true;
        self.error = None;
        self.call(
            cx,
            move || Ok(work()),
            move |this, done, cx| {
                (this.signing, this.prompt) = (false, None);
                match done {
                    Ok(_) => {
                        this.decided(&decided);
                        this.decided_here(cx);
                        this.refresh(cx);
                        then(this, cx);
                    }
                    Err(e) => this.fail(e, cx),
                }
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
        if self.signing {
            return;
        }
        let pending = envelope.clone();
        if !self.ready(move |this, cx| this.approve(pending, cx), cx) {
            return;
        }
        let device = self.device();
        let decided = match &self.screen {
            Screen::Review { proposal, .. } => format!("proposals/{}", proposal.id),
            Screen::Admit { exchange, .. } => format!("exchanges/{}", exchange.id),
            _ => String::new(),
        };
        self.signing_call(
            decided,
            |_, _| {},
            cx,
            move || {
                envelope.sign(&*device.key)?;
                device.client.signed(&envelope)
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
            |this, envelope, cx| this.go(Screen::Review { proposal, envelope }, cx),
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
                this.decided_here(cx);
                (this.sheet, this.notice) = (None, notice(answer));
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

    /// Asks once more, in a sheet, before c runs; the sheet never rides on
    /// the keyboard.
    pub(crate) fn ask(&mut self, c: Confirm, cx: &mut Context<Self>) {
        keyboard(false);
        self.sheet = Some(c);
        cx.notify();
    }

    /// Runs what the sheet asked about. Taking access away also wants the
    /// owner to prove it is them; removing a device signs, which does that.
    pub(crate) fn confirmed(&mut self, cx: &mut Context<Self>) {
        let Some(c) = self.sheet.take() else { return };
        match c {
            Confirm::DeleteAgent { id, name } => self.gate(
                move |this, cx| {
                    let notice = format!("You deleted {name}.");
                    this.post("DELETE", format!("identities/{id}"), cx, move |_| Some(notice))
                },
                cx,
            ),
            Confirm::RemoveDevice { keys, .. } => self.remove_keys(keys, cx),
            Confirm::DropMethod { id, .. } => self.drop_method(id, cx),
            Confirm::Unlink { name } => self.gate(move |this, cx| this.unlink(name, cx), cx),
            Confirm::Close { id, .. } => {
                self.post("POST", format!("grants/{id}/close"), cx, |_| Some("Closed.".into()))
            }
            Confirm::StopAll => self.stop_all(cx),
        }
        cx.notify();
    }

    /// Removes keys from the key log, one signature each: a device goes
    /// with all its ways of approving.
    fn remove_keys(&mut self, mut keys: Vec<String>, cx: &mut Context<Self>) {
        let Some(id) = keys.pop() else { return };
        let path = format!("keys/{id}/remove/challenge");
        let check = move |r: &Record| matches!(r, Record::Key(e) if e.op == mandate_core::Op::Remove && e.key.id == id);
        self.sign_then(path, json!({}), check, move |this, cx| this.remove_keys(keys, cx), cx);
    }

    /// Adds key as another way this device approves: a Keystore key first
    /// proves it works (a security key or token did as it was made), then
    /// the key that signs adds it to the key log.
    fn add_method(&mut self, key: device::Key, cx: &mut Context<Self>) {
        if self.signing {
            return;
        }
        // A locked security key wants its PIN before it signs.
        if self.device().key.locked() {
            let then: Then = Box::new(move |this, cx| this.add_method(key, cx));
            self.unlock = Some(Unlock { pin: String::new(), then, gate: false });
            keyboard(true);
            return cx.notify();
        }
        let (device, dir) = (self.device(), self.dir.clone());
        let name = self.keys.devices.iter().find(|d| d.current).map(|d| d.name.clone());
        (self.signing, self.error) = (true, None);
        self.prompt = device.key.touch().then_some(TOUCH);
        let work = move || {
            #[cfg(target_os = "android")]
            if let android::Key::Keystore(_) = key {
                key.confirm(None)?;
            }
            let new = key.key(&name.unwrap_or_else(device::name));
            let body = json!({ "key": new, "method": key.label() });
            let mut add = device.client.challenge("keys/add/challenge", &body)?;
            if !matches!(add.record()?, Record::Key(e) if e.op == mandate_core::Op::Add && e.key == new)
            {
                return Err(Error::Invalid(
                    "Nothing was signed: the server asked for something other than the new key."
                        .into(),
                ));
            }
            add.sign(&*device.key)?;
            device.client.signed(&add)?;
            device.add(&dir, key)
        };
        self.call(
            cx,
            move || Ok(work()),
            |this, added, cx| {
                (this.signing, this.prompt) = (false, None);
                match added {
                    Ok(device) => {
                        this.device = Some(device);
                        this.root(Tab::Now, cx);
                        this.go(Screen::Settings, cx);
                        this.notice = Some("Added.".into());
                        this.refresh(cx);
                    }
                    Err(e) => this.fail(e, cx),
                }
            },
        );
    }

    /// Signs with another of this device's keys from now on, once the one
    /// that signs now proves the owner is here. Nothing is signed.
    pub(crate) fn use_method(&mut self, id: String, cx: &mut Context<Self>) {
        self.gate(
            move |this, _| match this.device().activate(&this.dir, &id) {
                Ok(device) => {
                    this.device = Some(device);
                    this.notice = Some("Switched.".into());
                }
                Err(e) => this.error = Some(e.to_string()),
            },
            cx,
        );
    }

    /// Retires one of this device's keys that does not sign, signed by the
    /// one that does, then forgets it.
    fn drop_method(&mut self, id: String, cx: &mut Context<Self>) {
        let path = format!("keys/{id}/retire/challenge");
        let gone = id.clone();
        let check = move |r: &Record| matches!(r, Record::Key(e) if e.op == mandate_core::Op::Retire && e.key.id == gone);
        let then = move |this: &mut Shell, _: &mut Context<Shell>| match this
            .device()
            .forget(&this.dir, &id)
        {
            Ok(device) => (this.device, this.notice) = (Some(device), Some("Removed.".into())),
            Err(e) => this.error = Some(e.to_string()),
        };
        self.sign_then(path, json!({}), check, then, cx);
    }

    /// Tells the server how this device's keys approve where it does not
    /// know: keys from before several ways, or just paired or joined.
    fn label_keys(&mut self, cx: &mut Context<Self>) {
        let Some(device) = self.device.clone() else { return };
        let Some(this_device) = self.keys.devices.iter().find(|d| d.current) else { return };
        let unlabeled: Vec<(String, &'static str)> = this_device
            .keys
            .iter()
            .filter(|k| k.method.is_empty())
            .filter_map(|k| {
                let key = device.keys.iter().find(|d| device::id(d) == k.id)?;
                Some((k.id.clone(), key.label()))
            })
            .collect();
        if unlabeled.is_empty() {
            return;
        }
        self.call(
            cx,
            move || {
                for (id, method) in &unlabeled {
                    let body = json!({ "method": method });
                    device.client.send::<Value>("PATCH", &format!("keys/{id}"), &body)?;
                }
                Ok(unlabeled)
            },
            |this, labeled, cx| {
                let keys = this.keys.devices.iter_mut().flat_map(|d| d.keys.iter_mut());
                for k in keys {
                    if let Some((_, method)) = labeled.iter().find(|(id, _)| *id == k.id) {
                        k.method = (*method).into();
                    }
                }
                cx.notify();
            },
        );
    }

    /// Joins with the address and code typed on Join, once both look whole.
    pub(crate) fn submit_join(&mut self, cx: &mut Context<Self>) {
        let Screen::Join { url, code, .. } = &self.screen else { return };
        let url = url.trim().to_string();
        if url.starts_with("https://") && code.len() == 6 {
            let next = Next::Join { url, code: code.clone() };
            match self.join_key.take() {
                Some(key) => {
                    self.error = None;
                    self.with_key(next, key, cx);
                }
                None => self.new_key(next, cx),
            }
        }
    }

    fn key(&mut self, ev: &KeyDownEvent, _: &mut Window, cx: &mut Context<Self>) {
        // gpui-mobile hands Android's Back over as escape.
        #[cfg(target_os = "android")]
        if ev.keystroke.key == "escape" {
            if !self.go_back(cx) {
                android::leave();
            }
            return;
        }
        self.typed(ev, cx);
    }

    /// Typing into the field that takes it, from the keyboard or a paste.
    fn typed(&mut self, ev: &KeyDownEvent, cx: &mut Context<Self>) {
        let k = &ev.keystroke;
        // Every field is one line: a pasted line break types a space.
        let typed = k.key_char.as_deref().unwrap_or_default().replace(['\n', '\r'], " ");
        let typed = typed.as_str();
        if let Some(p) = &mut self.picker {
            // Search runs as it is typed; Enter only puts the keyboard away.
            if k.key == "enter" {
                keyboard(false);
                return cx.notify();
            }
            if k.key == "backspace" {
                p.q.pop();
            } else {
                p.q.push_str(typed);
            }
            self.find_chats(cx);
            return cx.notify();
        }
        if let Some(name) = &mut self.renaming {
            if k.key == "enter" {
                return self.rename(cx);
            }
            type_into(name, ev, cx);
            return cx.notify();
        }
        if let Some(unlock) = &mut self.unlock {
            match k.key.as_str() {
                "enter" => self.submit_pin(cx),
                "escape" => {
                    self.unlock = None;
                    keyboard(false);
                }
                _ => type_into(&mut unlock.pin, ev, cx),
            }
            return cx.notify();
        }
        match (&mut self.screen, &mut self.search) {
            #[cfg(not(target_os = "android"))]
            (Screen::Choose(_), _) => return self.choose_key(ev, cx),
            #[cfg(target_os = "android")]
            (Screen::Method { pin: Some(_), .. }, _) if k.key == "enter" => self.make_fido(cx),
            #[cfg(target_os = "android")]
            (Screen::Method { pin: Some(pin), .. }, _) => type_into(pin, ev, cx),
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
            (Screen::Tab(_), Some(_)) if k.key == "enter" => keyboard(false),
            (Screen::Tab(_), Some(q)) if k.key == "backspace" => {
                q.pop();
            }
            (Screen::Tab(_), Some(q)) => q.push_str(typed),
            (Screen::Join { code_field, .. }, _) if k.key == "enter" => {
                if *code_field {
                    self.submit_join(cx);
                } else {
                    *code_field = true;
                }
            }
            (Screen::Join { code, code_field: true, .. }, _) => {
                type_into(code, ev, cx);
                code.retain(|c| c.is_ascii_digit());
                code.truncate(6);
            }
            (Screen::Pair { .. }, _) if k.key == "enter" => keyboard(false),
            (Screen::Pair { text } | Screen::Join { url: text, .. }, _) => type_into(text, ev, cx),
            _ => return,
        }
        cx.notify();
    }

    /// Whether text belongs on the current page while searching.
    fn shown(&self, text: &str) -> bool {
        self.search.as_deref().is_none_or(|q| text.to_lowercase().contains(&q.to_lowercase()))
    }

    /// Puts text on the clipboard.
    pub(crate) fn copy(&mut self, text: String, cx: &mut Context<Self>) {
        #[cfg(target_os = "android")]
        let _ = cx;
        #[cfg(target_os = "android")]
        android::copy(&text);
        #[cfg(not(target_os = "android"))]
        cx.write_to_clipboard(gpui::ClipboardItem::new_string(text));
    }

    /// Types the clipboard's text into the field that takes typing.
    pub(crate) fn paste(&mut self, cx: &mut Context<Self>) {
        #[cfg(target_os = "android")]
        let pasted = android::clipboard();
        #[cfg(not(target_os = "android"))]
        let pasted = cx.read_from_clipboard().and_then(|c| c.text());
        let Some(pasted) = pasted else { return };
        let keystroke = gpui::Keystroke {
            key: String::new(),
            key_char: Some(pasted.trim().to_string()),
            modifiers: gpui::Modifiers::default(),
        };
        let ev = KeyDownEvent { keystroke, is_held: false, prefer_character_input: false };
        self.typed(&ev, cx);
    }
}

/// Types into a one-line field: characters, backspace, and paste.
fn type_into(text: &mut String, ev: &KeyDownEvent, cx: &App) {
    let k = &ev.keystroke;
    if k.modifiers.secondary() && k.key == "v" {
        let pasted = cx.read_from_clipboard().and_then(|c| c.text());
        let pasted = pasted.unwrap_or_default();
        text.push_str(&pasted.trim().replace(['\n', '\r'], " "));
    } else if k.key == "backspace" {
        text.pop();
    } else {
        text.push_str(&k.key_char.as_deref().unwrap_or_default().replace(['\n', '\r'], " "));
    }
}

/// What waits for the owner, for notifications: each item's ID and a line
/// naming only its identity, connector and kind (owner-api.md).
fn waiting(client: &Client) -> Result<Vec<(String, String)>, Error> {
    let now: Now = client.get("now")?;
    let exchanges: Vec<Exchange> = client.get("exchanges")?;
    // An open exchange has no name yet; it waits for the owner once confirmed.
    let exchanges = exchanges.into_iter().filter(|e| e.state == "confirmed").map(|e| {
        let text = if e.kind == "device" {
            screens::DEVICE_JOIN.to_string()
        } else {
            format!("{} wants to join as an agent", e.name)
        };
        (e.id, text)
    });
    let requests = now.requests.into_iter().map(|r| {
        (r.id, format!("{} wants to read {}", r.identity_name, words::connector(&r.connector)))
    });
    let proposals = now.proposals.into_iter().map(|p| {
        (
            p.id,
            format!("{} wants to {} on {}", p.identity_name, p.op, words::connector(&p.connector)),
        )
    });
    Ok(exchanges.chain(requests).chain(proposals).collect())
}

/// The owner's choices kept on this device, beside the device file.
#[derive(Default, Serialize, Deserialize)]
#[serde(default)]
struct Prefs {
    /// Lock mandate when it leaves the screen.
    lock: bool,
}

impl Prefs {
    const FILE: &str = "prefs.json";

    fn load(dir: &std::path::Path) -> Prefs {
        std::fs::read(dir.join(Prefs::FILE))
            .ok()
            .and_then(|b| serde_json::from_slice(&b).ok())
            .unwrap_or_default()
    }

    fn save(&self, dir: &std::path::Path) -> std::io::Result<()> {
        std::fs::write(dir.join(Prefs::FILE), serde_json::to_vec(self)?)
    }
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
            "icons/chevron-down.svg" => include_bytes!("../assets/icons/chevron-down.svg"),
            "icons/chevron-left.svg" => include_bytes!("../assets/icons/chevron-left.svg"),
            "icons/chevron-right.svg" => include_bytes!("../assets/icons/chevron-right.svg"),
            "icons/plug.svg" => include_bytes!("../assets/icons/plug.svg"),
            "icons/search.svg" => include_bytes!("../assets/icons/search.svg"),
            "icons/settings.svg" => include_bytes!("../assets/icons/settings.svg"),
            "icons/x.svg" => include_bytes!("../assets/icons/x.svg"),
            "icons/discord.svg" => include_bytes!("../assets/icons/discord.svg"),
            "icons/fastmail.svg" => include_bytes!("../assets/icons/fastmail.svg"),
            "icons/gmessages.svg" => include_bytes!("../assets/icons/gmessages.svg"),
            "icons/matrix.svg" => include_bytes!("../assets/icons/matrix.svg"),
            "icons/signal.svg" => include_bytes!("../assets/icons/signal.svg"),
            "icons/whatsapp.svg" => include_bytes!("../assets/icons/whatsapp.svg"),
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
