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
    pub activity: Vec<Activity>,
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

#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Activity {
    pub at: String,
    pub identity: String,
    pub connector: String,
    pub verb: String,
    pub chats: Option<Vec<Chat>>,
    pub count: u64,
}

#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Exchange {
    pub id: String,
    pub kind: String,
    pub name: String,
    pub address: String,
    pub started_at: String,
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

#[derive(Clone, Default, Deserialize)]
#[serde(default)]
pub struct Keys {
    pub trusted: Vec<Trusted>,
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
