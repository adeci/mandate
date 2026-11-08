//! The owner API bodies the app reads (owner-api.md, Bodies). Times stay
//! RFC 3339 text; words.rs turns them into the owner's local time.

use mandate_core::Field;
use serde::Deserialize;
use std::collections::BTreeMap;

#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Now {
    pub requests: Vec<Request>,
    pub proposals: Vec<Proposal>,
    pub grants: Vec<Grant>,
    pub routines: Vec<Grant>,
    pub stopped: Vec<Grant>,
}

#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Request {
    pub id: String,
    pub identity_name: String,
    pub connector: String,
    /// once, routine or extend.
    pub kind: String,
    /// None asks for every chat.
    pub terms: Option<Vec<Term>>,
    #[serde(rename = "for")]
    pub for_: u64,
    pub horizon: u64,
    pub schedule: Option<Schedule>,
    pub reason: String,
}

#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Term {
    pub term: String,
    pub chats: Option<Vec<Chat>>,
    /// It matched more chats than those listed.
    pub more: bool,
}

#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Chat {
    pub id: String,
    /// Its full path name: "main / #general".
    pub name: String,
    /// guild, channel, dm and so on; a guild, space or forum is all of it.
    pub kind: String,
    /// Outermost first, as the listing gives them.
    pub parents: Vec<ParentRef>,
    pub icon: Option<String>,
}

impl Chat {
    /// Its own name, without its parents' path.
    pub fn own(&self) -> &str {
        let path: Vec<&str> = self.parents.iter().map(|p| p.name.as_str()).collect();
        let prefix = format!("{} / ", path.join(" / "));
        match self.name.strip_prefix(&prefix) {
            Some(own) if !self.parents.is_empty() => own,
            _ if self.name.is_empty() => &self.id,
            _ => &self.name,
        }
    }
}

impl Request {
    /// The chats the request's terms matched, each once.
    pub fn chats(&self) -> Vec<&Chat> {
        let mut out: Vec<&Chat> = Vec::new();
        for c in self.terms.iter().flatten().flat_map(|t| t.chats.iter().flatten()) {
            if !out.iter().any(|o| o.id == c.id) {
                out.push(c);
            }
        }
        out
    }
}

#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Schedule {
    pub days: Vec<u8>,
    pub start: String,
    pub length: u64,
}

#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Proposal {
    pub id: String,
    pub identity_name: String,
    pub connector: String,
    pub op: String,
    pub display: Vec<Field>,
    pub note: String,
}

impl Proposal {
    pub fn field(&self, label: &str) -> Option<&str> {
        self.display.iter().find(|f| f.label == label).map(|f| f.value.as_str())
    }
}

#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Grant {
    pub id: String,
    pub identity_name: String,
    pub connector: String,
    pub chats: Vec<String>,
    pub chat_names: BTreeMap<String, String>,
    /// Its chats in order, with their kinds and parents.
    pub chat_refs: Vec<Chat>,
    pub horizon: u64,
    pub until: Option<String>,
    pub schedule: Option<Schedule>,
    pub closed_at: Option<String>,
}

/// One entry of the owner's history (`GET /owner/v1/activity`): a read, a
/// request or send and its outcome, a grant changing, an agent or device.
#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Activity {
    pub at: String,
    /// The last of several reads merged into this one; `at` is the first.
    pub until: Option<String>,
    /// What the reads did, in the order first done: chats, messages,
    /// search, media.
    pub verbs: Vec<String>,
    /// read, request.asked, proposal.done, stop_all and so on.
    pub kind: String,
    pub identity: String,
    pub connector: String,
    pub chats: Vec<Chat>,
    pub count: Option<u64>,
    pub detail: String,
    /// A request's reason, in the agent's words.
    pub reason: String,
    /// A request's search terms and the chats they matched.
    pub terms: Vec<Term>,
    /// A proposal's note, in the agent's words.
    pub note: String,
    /// The device the owner decided on.
    pub device: String,
    /// Where a login or join came from.
    pub address: String,
}

impl Activity {
    /// Several reads merged into one: `until` marks them, and entries
    /// written before it did have more than one verb.
    pub fn merged(&self) -> bool {
        self.kind == "read" && (self.until.is_some() || self.verbs.len() > 1)
    }
}

/// A page of history, newest first; next pages back from its last entry.
#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct History {
    pub entries: Vec<Activity>,
    pub next: Option<String>,
}

/// The days with history in a span, for the day picker.
#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Days {
    pub days: Vec<DayCount>,
}

/// A local day, "2026-11-03", and how many entries it has.
#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct DayCount {
    pub day: String,
    pub count: u32,
}

/// A code exchange the owner opened (owner-api.md, Code exchange). Name,
/// address and payload come once the new party's code matched.
#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Exchange {
    pub id: String,
    pub kind: String,
    /// "open", then "confirmed" once the code matched.
    pub state: String,
    pub name: String,
    pub address: String,
    pub payload: Option<mandate_core::Payload>,
    pub started_at: String,
}

/// An exchange just opened, with the code to give the new party.
#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Opened {
    pub id: String,
    pub code: String,
    pub expires_at: String,
}

#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Identity {
    pub id: String,
    pub name: String,
    pub created_at: String,
    pub last_seen: Option<String>,
    pub grants: Vec<Grant>,
}

#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Connector {
    pub name: String,
    pub health: Health,
    pub setup: bool,
}

#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Health {
    pub state: String,
    pub detail: String,
    pub until: Option<String>,
}

/// The server's own facts (`GET /owner/v1/server`).
#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Server {
    /// What `mandate login` takes on an agent's machine.
    pub agent_url: String,
}

/// The devices that sign, from `GET keys`.
#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Keys {
    pub devices: Vec<Signer>,
}

/// A device and its keys, one per way it approves; retired keys are gone.
#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Signer {
    pub id: String,
    pub name: String,
    /// The device asking.
    pub current: bool,
    pub keys: Vec<SignerKey>,
}

#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct SignerKey {
    pub id: String,
    /// "fingerprint", "screen lock", "security key" or "token"; empty
    /// until the device says.
    pub method: String,
}

/// A chat or person never shared, whatever the grants say.
#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Hidden {
    pub connector: String,
    pub kind: String,
    pub id: String,
    pub name: String,
}

/// A chat from a connector's chat list, for the picker (connector.md, Chats
/// for the owner).
#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Container {
    pub id: String,
    /// Its own name; the path adds its parents'.
    pub name: String,
    pub kind: String,
    /// What it sits in, outermost first; empty at the top.
    pub parents: Vec<ParentRef>,
    /// A shelf, or a chat with chats in it: it opens.
    pub opens: bool,
    /// A group's members, the owner counted.
    pub members: Option<u32>,
    /// A mailbox's threads, filed directly in it.
    pub threads: Option<u32>,
    /// Never shared: it, a parent or its person is hidden.
    pub hidden: bool,
    /// A DM's handle: "@sam_p", "+15551234567", "@bob:example.org".
    pub handle: Option<String>,
    /// The version of the chat's picture, absent when it has none.
    pub icon: Option<String>,
    pub last_activity: Option<String>,
    /// The people in it, where the connector lists them (connector.md,
    /// Chats); a person is hidden by their own ID, not the chat's.
    pub participants: Vec<Person>,
}

/// A chat's parent, as a row names it.
#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct ParentRef {
    pub id: String,
    pub name: String,
    /// guild, channel, mailbox and so on.
    pub kind: String,
    pub icon: Option<String>,
}

/// One person in a chat.
#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Person {
    pub id: String,
    pub name: String,
}

/// One step of linking a connector, as the connector describes it
/// (connector.md, Setup).
#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Step {
    pub step: String,
    pub title: String,
    pub text: String,
    pub fields: Vec<SetupField>,
    pub qr: String,
    pub code: String,
    pub confirm: String,
    pub poll: u64,
    pub done: bool,
}

#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct SetupField {
    pub name: String,
    pub label: String,
    /// text, secret, number or choice.
    pub kind: String,
    pub required: bool,
    pub options: Vec<Choice>,
}

#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Choice {
    pub value: String,
    pub label: String,
}
