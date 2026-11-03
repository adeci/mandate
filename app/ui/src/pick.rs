//! The chat picker, as the console's: a search over a connector's chat
//! list. It adds to never-share, picks a request's chats, and picks the
//! chats of the owner's own grant.

use crate::api::Container;
use crate::now::{FOR, HORIZON, checkbox, chip};
use crate::screens::{approve, button, clickable, column, id_for, link};
use crate::theme::{MEDIUM, Theme, chars, lines};
use crate::words::{self, connector, horizon, span};
use crate::{Adjust, Screen, Shell, keyboard, segment};
use gpui::{AnyElement, Context, Div, div, prelude::*};
use mandate_core::Record;
use serde_json::{Value, json};
use std::collections::{BTreeMap, BTreeSet};

/// What the picked chats are for.
#[derive(Clone, PartialEq)]
pub enum Pick {
    /// Ticks them on this waiting request.
    Request(String),
    /// Adds them to never-share.
    Hide,
    /// Ticks them on the new grant.
    Grant,
}

pub struct Picker {
    pub pick: Pick,
    pub connector: String,
    pub q: String,
    /// None until the first answer.
    pub found: Option<Vec<Container>>,
}

const LENGTHS: [u64; 5] = [1800, 3600, 7200, 14400, 28800];

/// The owner's own grant, as it is being set up.
#[derive(Clone)]
pub struct NewGrant {
    pub identity: String,
    pub connector: String,
    /// None is every chat; otherwise the picked chats by ID, with names.
    pub chats: Option<BTreeMap<String, String>>,
    pub routine: bool,
    pub for_: u64,
    pub days: Vec<u8>,
    /// Minutes after midnight.
    pub start: u32,
    pub length: u64,
    pub horizon: u64,
}

impl NewGrant {
    pub fn new(identity: String) -> NewGrant {
        NewGrant {
            identity,
            connector: String::new(),
            chats: None,
            routine: false,
            for_: 3600,
            days: Vec::new(),
            start: 9 * 60,
            length: 3600,
            horizon: 86400,
        }
    }

    fn start(&self) -> String {
        format!("{:02}:{:02}", self.start / 60, self.start % 60)
    }
}

impl Shell {
    pub(crate) fn open_picker(&mut self, pick: Pick, connector: String, cx: &mut Context<Self>) {
        self.search = None;
        self.picker = Some(Picker { pick, connector, q: String::new(), found: None });
        self.find_chats(cx);
    }

    pub(crate) fn close_picker(&mut self, cx: &mut Context<Self>) {
        self.picker = None;
        keyboard(false);
        cx.notify();
    }

    /// Asks the connector for the chats matching the picker's search.
    pub(crate) fn find_chats(&mut self, cx: &mut Context<Self>) {
        let Some(p) = &self.picker else { return };
        let (q, conn) = (p.q.clone(), p.connector.clone());
        let path = format!("connectors/{}/chats?limit=50&q={}", segment(&conn), segment(&q));
        let client = self.device().client;
        self.call(
            cx,
            move || client.get::<Vec<Container>>(&path),
            move |this, found, _| {
                if let Some(p) = &mut this.picker
                    && p.q == q
                    && p.connector == conn
                {
                    p.found = Some(found);
                }
            },
        );
    }

    /// The picker's search and results; row draws each chat's control.
    pub(crate) fn picker(
        &self,
        t: Theme,
        cx: &mut Context<Self>,
        row: impl Fn(&Container, &mut Context<Self>) -> AnyElement,
    ) -> Div {
        let Some(p) = &self.picker else { return column() };
        let results: Vec<AnyElement> = match &p.found {
            None => vec![t.alt("Loading chats…").into_any_element()],
            Some(f) if f.is_empty() => vec![t.alt("No chat matches.").into_any_element()],
            Some(f) => f.iter().map(|c| row(c, cx)).collect(),
        };
        column()
            .gap(lines(0.5))
            .child(
                clickable("chat-search", cx, |_, _| keyboard(true))
                    .flex()
                    .items_center()
                    .gap(chars(1.))
                    .border_2()
                    .border_color(t.text)
                    .px(chars(1.))
                    .py(lines(0.5))
                    .child(crate::screens::icon("search", t.alt))
                    .child(if p.q.is_empty() {
                        t.alt(format!("search {} chats", connector(&p.connector))).flex_1()
                    } else {
                        div().flex_1().child(format!("{}▏", p.q))
                    }),
            )
            .children(results)
    }

    pub(crate) fn hide_screen(&self, t: Theme, cx: &mut Context<Self>) -> Div {
        let Screen::Hide { connector: current } = &self.screen else { return column() };
        let conns = self
            .connectors
            .iter()
            .map(|c| {
                let name = c.name.clone();
                let on = current.as_deref() == Some(c.name.as_str());
                chip(t, id_for("conn", &c.name), &connector(&c.name), on, cx, move |this, cx| {
                    this.screen = Screen::Hide { connector: Some(name.clone()) };
                    this.open_picker(crate::pick::Pick::Hide, name.clone(), cx);
                })
            })
            .collect::<Vec<_>>();
        let hidden: BTreeSet<(String, String, String)> = self
            .hidden
            .iter()
            .map(|h| (h.connector.clone(), h.kind.clone(), h.id.clone()))
            .collect();
        column()
            .child("Pick a chat or a person to keep out of every read, whatever the grants say.")
            .child(div().font_weight(MEDIUM).child("CONNECTOR"))
            .child(div().flex().flex_wrap().gap(chars(1.)).children(conns))
            .when(self.picker.is_some(), |d| {
                d.child(self.picker(t, cx, |c, cx| {
                    let conn =
                        self.picker.as_ref().map(|p| p.connector.clone()).unwrap_or_default();
                    let is =
                        |kind: &str| hidden.contains(&(conn.clone(), kind.into(), c.id.clone()));
                    let mut actions = vec![("chat", "Hide chat")];
                    if c.kind == "dm" {
                        actions.push(("person", "Hide person"));
                    }
                    div()
                        .flex()
                        .flex_wrap()
                        .items_center()
                        .gap_x(chars(1.))
                        .py(lines(0.25))
                        .child(div().flex_1().child(display(c)))
                        .child(t.alt(c.kind.clone()))
                        .children(actions.into_iter().map(|(kind, label)| {
                            if is(kind) {
                                return t.alt(format!("[{kind} hidden]")).into_any_element();
                            }
                            let (conn, id, name) = (conn.clone(), c.id.clone(), display(c));
                            link(t, id_for(kind, &c.id), label, cx, move |this, cx| {
                                this.hide(&conn, kind, &id, &name, cx)
                            })
                            .into_any_element()
                        }))
                        .into_any_element()
                }))
            })
    }

    fn hide(&mut self, conn: &str, kind: &str, id: &str, name: &str, cx: &mut Context<Self>) {
        let path = format!("never-share/{}/{kind}/{}", segment(conn), segment(id));
        let (client, name) = (self.device().client, name.to_string());
        self.error = None;
        self.call(
            cx,
            move || client.send::<Value>("PUT", &path, &json!({"name": name})),
            |this, _, cx| this.refresh(cx),
        );
    }

    pub(crate) fn grant_screen(&self, t: Theme, cx: &mut Context<Self>) -> Div {
        let Screen::Grant(g) = &self.screen else { return column() };
        let set = |f: fn(&mut NewGrant)| {
            move |this: &mut Shell, cx: &mut Context<Shell>| {
                if let Screen::Grant(g) = &mut this.screen {
                    f(g);
                }
                cx.notify();
            }
        };
        let heading = |s: &str| div().font_weight(MEDIUM).child(s.to_string());
        let chips = |d: Div| d.flex().flex_wrap().gap(chars(1.));
        let agents = self
            .identities
            .iter()
            .map(|a| {
                let id = a.id.clone();
                chip(t, id_for("agent", &a.id), &a.name, g.identity == a.id, cx, move |this, cx| {
                    if let Screen::Grant(g) = &mut this.screen {
                        g.identity = id.clone();
                    }
                    cx.notify();
                })
            })
            .collect::<Vec<_>>();
        let conns = self
            .connectors
            .iter()
            .filter(|c| !c.setup)
            .map(|c| {
                let name = c.name.clone();
                chip(
                    t,
                    id_for("conn", &c.name),
                    &connector(&c.name),
                    g.connector == c.name,
                    cx,
                    move |this, cx| {
                        if let Screen::Grant(g) = &mut this.screen {
                            (g.connector, g.chats) = (name.clone(), None);
                        }
                        this.close_picker(cx);
                    },
                )
            })
            .collect::<Vec<_>>();
        let fors = FOR
            .into_iter()
            .map(|v| {
                chip(t, ("for", v as usize), &span(v), g.for_ == v, cx, move |this, cx| {
                    if let Screen::Grant(g) = &mut this.screen {
                        g.for_ = v;
                    }
                    cx.notify();
                })
            })
            .collect::<Vec<_>>();
        let lengths = LENGTHS
            .into_iter()
            .map(|v| {
                chip(t, ("length", v as usize), &span(v), g.length == v, cx, move |this, cx| {
                    if let Screen::Grant(g) = &mut this.screen {
                        g.length = v;
                    }
                    cx.notify();
                })
            })
            .collect::<Vec<_>>();
        let horizons = HORIZON
            .into_iter()
            .map(|v| {
                chip(
                    t,
                    ("horizon", v as usize),
                    &horizon(v),
                    g.horizon == v,
                    cx,
                    move |this, cx| {
                        if let Screen::Grant(g) = &mut this.screen {
                            g.horizon = v;
                        }
                        cx.notify();
                    },
                )
            })
            .collect::<Vec<_>>();
        let days: [(&str, &[u8]); 3] =
            [("every day", &[]), ("weekdays", &[1, 2, 3, 4, 5]), ("weekends", &[0, 6])];
        let days = days
            .into_iter()
            .enumerate()
            .map(|(i, (label, d))| {
                chip(t, ("days", i), label, g.days == d, cx, move |this, cx| {
                    if let Screen::Grant(g) = &mut this.screen {
                        g.days = d.to_vec();
                    }
                    cx.notify();
                })
            })
            .collect::<Vec<_>>();
        let picked: Vec<Div> =
            g.chats
                .iter()
                .flatten()
                .map(|(id, name)| {
                    let id2 = id.clone();
                    div().flex().gap(chars(1.)).child(div().flex_1().child(name.clone())).child(
                        link(t, id_for("unpick", id), "Remove", cx, move |this, cx| {
                            if let Screen::Grant(g) = &mut this.screen
                                && let Some(c) = &mut g.chats
                            {
                                c.remove(&id2);
                            }
                            cx.notify();
                        }),
                    )
                })
                .collect();
        let named = self.identities.iter().find(|a| a.id == g.identity).map(|a| a.name.clone());
        let chats = match &g.chats {
            None => Some("every chat".to_string()),
            Some(c) if c.is_empty() => None,
            Some(c) if c.len() == 1 => c.values().next().cloned(),
            Some(c) => Some(format!("{} chats", c.len())),
        };
        let window = if g.routine {
            format!("{} {} for {}", words::days(&g.days), g.start(), span(g.length))
        } else {
            span(g.for_)
        };
        let ready = named.is_some() && !g.connector.is_empty() && chats.is_some();
        let label = match (&named, &chats) {
            (Some(n), Some(c)) if ready => {
                format!("Grant {n} · {window} · {c} · {}", horizon(g.horizon))
            }
            _ => "Grant".into(),
        };
        let picking = self.picker.as_ref().is_some_and(|p| p.pick == crate::pick::Pick::Grant);
        let ng = g.clone();
        column()
            .child(heading("AGENT"))
            .child(chips(div()).children(agents))
            .child(heading("CONNECTOR"))
            .child(if self.connectors.iter().all(|c| c.setup) {
                t.alt("No connector is linked.").into_any_element()
            } else {
                chips(div()).children(conns).into_any_element()
            })
            .when(!g.connector.is_empty(), |d| {
                let conn = g.connector.clone();
                d.child(heading("CHATS"))
                    .child(
                        chips(div())
                            .child(chip(t, "every", "every chat", g.chats.is_none(), cx, |this, cx| {
                                if let Screen::Grant(g) = &mut this.screen {
                                    g.chats = None;
                                }
                                this.close_picker(cx);
                            }))
                            .child(chip(t, "only", "only these", g.chats.is_some(), cx, move |this, cx| {
                                if let Screen::Grant(g) = &mut this.screen
                                    && g.chats.is_none()
                                {
                                    g.chats = Some(BTreeMap::new());
                                }
                                this.open_picker(crate::pick::Pick::Grant, conn.clone(), cx);
                            })),
                    )
                    .when(g.chats.is_some(), |d| {
                        d.children(picked).when(picking, |d| {
                            d.child(self.picker(t, cx, |c, cx| {
                                let on = matches!(&ng.chats, Some(m) if m.contains_key(&c.id));
                                let (id, name) = (c.id.clone(), display(c));
                                clickable(id_for("pick", &c.id), cx, move |this, cx| {
                                    if let Screen::Grant(g) = &mut this.screen
                                        && let Some(m) = &mut g.chats
                                        && m.remove(&id).is_none()
                                    {
                                        m.insert(id.clone(), name.clone());
                                    }
                                    cx.notify();
                                })
                                .flex()
                                .gap(chars(1.))
                                .py(lines(0.25))
                                .child(checkbox(t, on))
                                .child(display(c))
                                .into_any_element()
                            }))
                            .child(t.alt("Each chat is granted on its own. Chats created later are not included."))
                        })
                    })
            })
            .child(heading("WHEN"))
            .child(
                chips(div())
                    .child(chip(t, "once", "now, for", !g.routine, cx, set(|g| g.routine = false)))
                    .child(chip(t, "routine", "on a schedule", g.routine, cx, set(|g| g.routine = true))),
            )
            .when(!g.routine, |d| d.child(chips(div()).children(fors)))
            .when(g.routine, |d| {
                d.child(chips(div()).children(days))
                    .child(
                        div()
                            .flex()
                            .items_center()
                            .gap(chars(1.))
                            .child("at")
                            .child(button(t, "earlier", "-", cx, set(|g| g.start = (g.start + 1410) % 1440)))
                            .child(div().font_weight(MEDIUM).child(g.start()))
                            .child(button(t, "later", "+", cx, set(|g| g.start = (g.start + 30) % 1440))),
                    )
                    .child(chips(div()).children(lengths))
                    .child(t.alt(format!("in {}", words::zone())))
            })
            .child(heading("MESSAGES FROM"))
            .child(chips(div()).children(horizons))
            .child(approve(t, "grant", label.into(), ready, cx, move |this, cx| this.grant(&ng, cx)))
    }

    fn grant(&mut self, g: &NewGrant, cx: &mut Context<Self>) {
        let chats: Vec<String> = g.chats.iter().flatten().map(|(id, _)| id.clone()).collect();
        let mut body =
            json!({"identity": g.identity, "connector": g.connector, "horizon": g.horizon});
        if g.chats.is_some() {
            body["chats"] = json!(chats);
        }
        let schedule = g.routine.then(|| (g.days.clone(), g.start(), g.length));
        match &schedule {
            Some((days, start, length)) => {
                body["schedule"] =
                    json!({"days": days, "start": start, "length": length, "zone": words::zone()})
            }
            None => body["for"] = g.for_.into(),
        }
        let (identity, conn, horizon, for_) =
            (g.identity.clone(), g.connector.clone(), g.horizon, g.for_);
        let check = move |r: &Record| {
            let Record::Grant(x) = r else { return false };
            let window = match (&schedule, &x.schedule) {
                (Some((d, s, l)), Some(y)) => {
                    y.days == *d && y.start == *s && u64::from(y.length) == *l
                }
                (None, None) => {
                    x.until.zip(x.from).is_some_and(|(u, f)| (u.unix() - f.unix()) as u64 == for_)
                }
                _ => false,
            };
            let mut granted = x.chats.clone();
            granted.sort();
            x.identity == identity
                && x.connector == conn
                && x.horizon == horizon
                && window
                && granted == chats
        };
        self.sign("grants/challenge".into(), body, check, cx);
    }

    /// Picks the request's chats, beyond or instead of what its terms matched.
    pub(crate) fn request_picker(
        &self,
        t: Theme,
        id: &str,
        a: &Adjust,
        cx: &mut Context<Self>,
    ) -> Div {
        let id = id.to_string();
        let ticked = a.chats.clone();
        self.picker(t, cx, move |c, cx| {
            let (rid, cid, name) = (id.clone(), c.id.clone(), display(c));
            clickable(id_for("pick", &c.id), cx, move |this, cx| {
                if let Some(a) = this.adjust.get_mut(&rid) {
                    a.names.entry(cid.clone()).or_insert_with(|| name.clone());
                    if !a.chats.remove(&cid) {
                        a.chats.insert(cid.clone());
                    }
                }
                cx.notify();
            })
            .flex()
            .gap(chars(1.))
            .py(lines(0.25))
            .child(checkbox(t, ticked.contains(&c.id)))
            .child(display(c))
            .into_any_element()
        })
    }
}

/// A chat's name, or its ID when it has none.
fn display(c: &Container) -> String {
    if c.name.is_empty() { c.id.clone() } else { c.name.clone() }
}
