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
}

#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Chat {
    pub id: String,
    pub name: String,
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

/// A page of history, newest first; next pages back from its last entry.
#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct History {
    pub entries: Vec<Activity>,
    pub next: Option<String>,
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

#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Keys {
    pub trusted: Vec<Trusted>,
    /// Names given in the app, over the names keys were added with.
    pub names: BTreeMap<String, String>,
    /// Keys rotated out: still in the log, no device of their own.
    pub retired: Vec<String>,
}

impl Keys {
    /// What the owner calls a device: a name given in the app, else the
    /// one it joined with.
    pub fn name<'a>(&'a self, k: &'a Trusted) -> &'a str {
        self.names.get(&k.id).map_or(k.name.as_str(), String::as_str)
    }
}

#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Trusted {
    pub id: String,
    pub name: String,
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

/// A chat from a connector's chat list, for the picker.
#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Container {
    pub id: String,
    pub name: String,
    pub kind: String,
    pub parent_id: String,
    pub last_activity: Option<String>,
    /// The people in it, where the connector lists them (connector.md,
    /// Chats); a person is hidden by their own ID, not the chat's.
    pub participants: Vec<Person>,
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
