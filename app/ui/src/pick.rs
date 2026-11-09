//! The chat picker, as the console's: a search over a connector's chat
//! list. It adds to never-share, picks a request's chats, and picks the
//! chats of the owner's own grant.

use crate::api::Container;
use crate::now::{FOR, HORIZON, checkbox, chip, chip_frame, stops};
use crate::screens::{
    approve, button, clickable, column, connector_label, id_for, in_button, link, slider,
};
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
    /// The rows opened, outermost first; empty at the top.
    pub path: Vec<Container>,
    /// What the search, or the place shown, holds; None until it comes.
    pub found: Option<Vec<Container>>,
    /// The picked chats' own rows, for their pictures and ages.
    pub picked: Vec<Container>,
}

/// How many chats a list shows before `Search for more`; shelves are never
/// cut.
const LIMIT: usize = 50;

/// The columns an age takes: "2025", "12w", "Sep", "now".
const AGE: usize = 4;

/// Whether a chat only holds others, a Discord server or forum, or a Matrix
/// space: listed apart, it opens to its chats, and a tick on it is all of
/// them, as they come.
fn holds_chats(c: &Container) -> bool {
    covers_all(&c.kind)
}

/// Whether a tick on a chat of this kind covers all under it, at any depth.
fn covers_all(kind: &str) -> bool {
    matches!(kind, "guild" | "space" | "forum" | "media")
}

const LENGTHS: [u64; 5] = [1800, 3600, 7200, 14400, 28800];

/// The owner's own grant, as it is being set up.
#[derive(Clone)]
pub struct NewGrant {
    pub identity: String,
    pub connector: String,
    /// None is every chat; otherwise the picked chats by ID, with paths.
    pub chats: Option<BTreeMap<String, crate::api::Chat>>,
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
        self.picker = Some(Picker {
            pick,
            connector,
            q: String::new(),
            path: Vec::new(),
            found: None,
            picked: Vec::new(),
        });
        self.find_chats(cx);
    }

    pub(crate) fn close_picker(&mut self, cx: &mut Context<Self>) {
        self.picker = None;
        keyboard(false);
        cx.notify();
    }

    /// Asks the connector for what the picker shows (connector.md, Chats for
    /// the owner): the search's matches, inside the opened row if one is;
    /// else the opened row's chats; else the top level. And the ticked
    /// chats' own rows.
    pub(crate) fn find_chats(&mut self, cx: &mut Context<Self>) {
        let Some(p) = &self.picker else { return };
        let (q, conn) = (p.q.clone(), p.connector.clone());
        let parent = p.path.last().map(|c| c.id.clone());
        let mut path = format!("connectors/{}/chats?limit={LIMIT}", segment(&conn));
        if !q.is_empty() {
            path += &format!("&q={}", segment(&q));
        }
        match (&parent, q.is_empty()) {
            (Some(id), _) => path += &format!("&parent={}", segment(id)),
            (None, true) => path += "&parent=",
            (None, false) => {}
        }
        let picked: Vec<String> = self.ticked().into_iter().collect();
        let picked = (!picked.is_empty()).then(|| {
            let ids: Vec<String> = picked.iter().map(|i| segment(i)).collect();
            format!("connectors/{}/chats?ids={}", segment(&conn), ids.join(","))
        });
        let client = self.device().client;
        self.call(
            cx,
            move || {
                let found = client.get::<Vec<Container>>(&path)?;
                let picked = picked.map(|r| client.get::<Vec<Container>>(&r)).transpose()?;
                Ok((found, picked.unwrap_or_default()))
            },
            move |this, (found, picked), _| {
                if let Some(p) = &mut this.picker
                    && (&p.q, &p.connector, p.path.last().map(|c| &c.id))
                        == (&q, &conn, parent.as_ref())
                {
                    (p.found, p.picked) = (Some(found), picked);
                }
            },
        );
    }

    /// Opens a row to what it holds, one level down.
    pub(crate) fn open_row(&mut self, row: Container, cx: &mut Context<Self>) {
        if let Some(p) = &mut self.picker {
            p.path.push(row);
            p.found = None;
        }
        self.find_chats(cx);
        cx.notify();
    }

    /// Goes up one level; false at the top.
    pub(crate) fn up_row(&mut self, cx: &mut Context<Self>) -> bool {
        let Some(p) = &mut self.picker else { return false };
        if p.path.pop().is_none() {
            return false;
        }
        p.found = None;
        self.find_chats(cx);
        cx.notify();
        true
    }

    /// The chats ticked in the open picker, by ID.
    fn ticked(&self) -> BTreeSet<String> {
        match (self.picker.as_ref().map(|p| &p.pick), &self.screen) {
            (Some(Pick::Request(id)), _) => {
                self.adjust.get(id).map(|a| a.chats.clone()).unwrap_or_default()
            }
            (Some(Pick::Grant), Screen::Grant(g)) => {
                g.chats.iter().flatten().map(|(id, _)| id.clone()).collect()
            }
            _ => BTreeSet::new(),
        }
    }

    /// One bordered element: the search over a scrollable list of the
    /// connector's chats. At the top: PICKED, the shelves (servers, spaces,
    /// mailboxes) and the chats outside them; flat connectors list their
    /// chats alone. `›` opens a row; the back row goes up one level. pinned
    /// (the chats already picked) stay on top at the top level, under their
    /// servers as a tree; row draws a chat with its control, around the
    /// line the picker makes for it.
    pub(crate) fn picker(
        &self,
        t: Theme,
        pinned: Vec<Container>,
        cx: &mut Context<Self>,
        row: impl Fn(&Container, Div, &mut Context<Self>) -> AnyElement,
    ) -> Div {
        let Some(p) = &self.picker else { return column() };
        let inside = p.path.last();
        let top = inside.is_none() && p.q.is_empty();
        // Each part's chats, each chat once, in the first part that has it.
        let mut shown: BTreeSet<String> = BTreeSet::new();
        let mut part = |label: Option<String>, chats: Vec<Container>| {
            let chats: Vec<Container> =
                chats.into_iter().filter(|c| shown.insert(c.id.clone())).collect();
            (label, chats)
        };
        let found = p.found.clone().unwrap_or_default();
        let (shelves, chats): (Vec<Container>, Vec<Container>) =
            found.into_iter().partition(is_shelf);
        let labelled = top && !shelves.is_empty();
        // The picked chats as their rows show them, once those come. They
        // rise to the top only at the top level: inside a shelf its own chats
        // keep their order, picks ticked in place.
        let known = |id: &str| {
            let lists = [Some(&p.picked), p.found.as_ref()];
            lists.into_iter().flatten().flatten().find(|r| r.id == id).cloned()
        };
        let pinned: Vec<Container> = match inside {
            Some(_) => Vec::new(),
            None => pinned.into_iter().map(|c| known(&c.id).unwrap_or(c)).collect(),
        };
        let pinned_ids: BTreeSet<String> = pinned.iter().map(|c| c.id.clone()).collect();
        let mut parts = vec![part(labelled.then(|| "PICKED".into()), pinned)];
        if labelled {
            let kind = shelves.first().map_or("", |s| s.kind.as_str());
            parts.push(part(Some(shelf_label(kind).into()), shelves));
            parts.push(part(Some("CHATS".into()), chats));
        } else {
            parts.push(part(None, shelves.into_iter().chain(chats).collect()));
        }
        // Picked chats keep their full names; the rest are named from where
        // the list stands.
        let place = |c: &Container| if pinned_ids.contains(&c.id) { None } else { inside };
        // Rows named alike tell themselves apart by their IDs' ends.
        let names: Vec<String> = parts
            .iter()
            .flat_map(|(_, cs)| cs.iter().map(|c| own_path(c, place(c)).join(" / ")))
            .collect();
        let twin = |c: &Container| {
            let name = own_path(c, place(c)).join(" / ");
            names.iter().filter(|n| **n == name).count() > 1
        };
        // A ticked holder covers all under it; a ticked mailbox the threads
        // filed in it. Those rows show ticked and dim, and don't untick.
        let ticked = self.ticked();
        let covered = |c: &Container| {
            let by = |a: &crate::api::ParentRef| ticked.contains(&a.id);
            c.parents.iter().any(|a| by(a) && covers_all(&a.kind))
                || (c.kind != "mailbox"
                    && c.parents.last().is_some_and(|a| by(a) && a.kind == "mailbox"))
        };
        let dim = Theme { text: t.alt, ..t };
        let mut rows: Vec<AnyElement> = Vec::new();
        let draw = |c: &Container, place: Option<&Container>, cx: &mut Context<Self>| {
            let line = self.chat_line(t, c, place, twin(c), cx);
            match covered(c) && !c.hidden {
                true => div()
                    .flex()
                    .items_center()
                    .gap(chars(1.))
                    .py(lines(0.25))
                    .text_color(t.alt)
                    .child(checkbox(dim, true))
                    .child(line)
                    .into_any_element(),
                false => row(c, line, cx),
            }
        };
        for (i, (label, chats)) in parts.into_iter().enumerate() {
            if chats.is_empty() {
                continue;
            }
            rows.extend(
                label.map(|l| t.alt(l).font_weight(MEDIUM).pt(lines(0.25)).into_any_element()),
            );
            if i > 0 {
                rows.extend(chats.iter().map(|c| draw(c, place(c), cx)));
                continue;
            }
            // PICKED: each server once, its picked chats beneath as a tree;
            // a server picked whole heads them, and ends in "all channels".
            for (head, cs) in picked_groups(&chats) {
                let Some(head) = head else {
                    rows.extend(cs.iter().map(|c| draw(c, None, cx)));
                    continue;
                };
                let whole = cs.iter().find(|c| c.id == head.id);
                rows.push(match whole {
                    Some(h) => draw(h, None, cx),
                    None => div()
                        .flex()
                        .items_center()
                        .gap(chars(1.))
                        .py(lines(0.25))
                        .children(self.row_picture(t, &head, false, cx))
                        .child(div().min_w_0().truncate().font_weight(MEDIUM).child(display(&head)))
                        .into_any_element(),
                });
                let under: Vec<&Container> = cs.iter().filter(|c| c.id != head.id).collect();
                let breadth = whole.and_then(|h| crate::chats::whole(&h.kind));
                let n = under.len() + usize::from(breadth.is_some());
                for (j, c) in under.into_iter().enumerate() {
                    let row = draw(c, Some(&head), cx);
                    rows.push(
                        div()
                            .flex()
                            .items_center()
                            .child(branch(t, j + 1 == n))
                            .child(div().flex_1().min_w_0().child(row))
                            .into_any_element(),
                    );
                }
                rows.extend(breadth.map(|w| {
                    div()
                        .flex()
                        .items_center()
                        .py(lines(0.25))
                        .child(branch(t, true))
                        .child(w)
                        .into_any_element()
                }));
            }
        }
        if rows.is_empty() {
            let empty = match (&p.found, inside) {
                (None, _) => "Loading chats…".to_string(),
                _ if !p.q.is_empty() => format!("No chat matches \"{}\".", p.q),
                (_, Some(c)) => format!("No chats in {}.", display(c)),
                (_, None) => "No chats yet.".into(),
            };
            rows.push(t.alt(empty).py(lines(0.25)).into_any_element());
        }
        // The back row says where you are and goes up one level.
        let back = inside.map(|c| {
            let path = c.parents.iter().map(|p| p.name.as_str()).collect::<Vec<_>>();
            let name = words::fit_path(&path, &display(c), words::chat_cols().saturating_sub(4));
            clickable("close-parent", cx, |this, cx| {
                this.up_row(cx);
            })
            .flex()
            .items_center()
            .gap(chars(1.))
            .py(lines(0.25))
            .child(crate::screens::icon("chevron-left", t.alt))
            .children(self.row_picture(t, c, false, cx))
            .child(div().flex_1().min_w_0().truncate().font_weight(MEDIUM).child(name))
        });
        // A list stops at its limit; a search reaches the rest.
        let more =
            p.found.as_ref().is_some_and(|f| f.iter().filter(|c| !is_shelf(c)).count() >= LIMIT);
        column()
            .border_2()
            .border_color(t.text)
            .child(
                clickable("chat-search", cx, |_, _| keyboard(true))
                    .flex()
                    .items_center()
                    .gap(chars(1.))
                    .border_b_2()
                    .border_color(t.text)
                    .px(chars(1.))
                    .py(lines(0.5))
                    .child(crate::screens::icon("search", t.alt))
                    .child(if p.q.is_empty() {
                        t.alt(format!("Search {} chats", connector(&p.connector))).flex_1()
                    } else {
                        div().flex_1().child(format!("{}▏", p.q))
                    })
                    .child(crate::screens::icon(words::connector_icon(&p.connector), t.alt)),
            )
            .child(crate::scroll::view(
                "chat-list",
                div().max_h(lines(12.)).px(chars(1.)).py(lines(0.25)),
                column()
                    .children(back)
                    .children(rows)
                    .when(more, |d| d.child(t.alt("Search for more").py(lines(0.25)))),
            ))
    }

    /// A chat's line, after its tick: picture, name, the value that tells it
    /// from its neighbours, its age, and `›` when it opens (picker-design.md
    /// §2.3). Named by its path outside its parent, by its own name inside.
    fn chat_line(
        &self,
        t: Theme,
        c: &Container,
        inside: Option<&Container>,
        twin: bool,
        cx: &mut Context<Self>,
    ) -> Div {
        // A DM's handle shows when its name is the person's own choosing,
        // which anyone can copy, or when a neighbour shares it.
        let named_by_them = c.name.starts_with('~') || twin;
        let value = match c {
            c if c.hidden => "[hidden]".to_string(),
            c if matches!(c.kind.as_str(), "voice" | "stage") => c.kind.clone(),
            Container { members: Some(n), .. } => format!("{n} members"),
            Container { threads: Some(n), .. } if *n > 0 => format!("{n} threads"),
            // A long handle gives way from its start, keeping its end.
            Container { handle: Some(h), .. } if named_by_them && !h.is_empty() => {
                let n = h.chars().count();
                if n <= 16 {
                    h.clone()
                } else {
                    format!("…{}", h.chars().skip(n - 15).collect::<String>())
                }
            }
            c if twin => words::short_id(&c.id),
            _ => String::new(),
        };
        let age = c.last_activity.as_deref().map(words::age).unwrap_or_default();
        let mut path = own_path(c, inside);
        let own = path.pop().unwrap_or_default();
        let parents: Vec<&str> = path.iter().map(String::as_str).collect();
        // The name gets what the tick, picture, value, age and `›` leave:
        // `chat_cols` already keeps a card's padding and a tick's room.
        let picture = self.row_picture(t, c, inside.is_some(), cx);
        let reserve = 3
            + if picture.is_some() { 4 } else { 0 }
            + if value.is_empty() { 0 } else { value.chars().count() + 1 }
            + if age.is_empty() { 0 } else { AGE + 1 }
            + if c.opens { 3 } else { 0 };
        let room = words::chat_cols().saturating_sub(reserve);
        let name = words::fit_path(&parents, &own, room);
        let open = c.opens.then(|| {
            let r = c.clone();
            div()
                .id(id_for("open-row", &c.id))
                .cursor_pointer()
                .px(chars(0.5))
                .on_click(cx.listener(move |this, _, _, cx| {
                    // The row's own tap ticks; this one only opens.
                    cx.stop_propagation();
                    this.open_row(r.clone(), cx)
                }))
                .child(crate::screens::shade())
                .child(crate::screens::icon("chevron-right", t.alt))
        });
        div()
            .flex()
            .flex_1()
            .min_w_0()
            .items_center()
            .gap(chars(1.))
            .children(picture)
            .child(
                div()
                    .flex_1()
                    .min_w_0()
                    .truncate()
                    .when(holds_chats(c), |d| d.font_weight(MEDIUM))
                    .child(name),
            )
            .when(!value.is_empty(), |d| d.child(t.alt(value).flex_none().whitespace_nowrap()))
            // The longest age, "2025", "12w" or "Sep", in a column of its own
            // that never wraps: the name gives way.
            .child(
                t.alt(age)
                    .w(chars(AGE as f32) + gpui::px(2.))
                    .flex_none()
                    .whitespace_nowrap()
                    .text_right(),
            )
            .children(open)
    }

    pub(crate) fn hide_screen(&self, t: Theme, cx: &mut Context<Self>) -> Div {
        let Screen::Hide { connector: current } = &self.screen else { return column() };
        let conns = self
            .connectors
            .iter()
            .map(|c| {
                let name = c.name.clone();
                let on = current.as_deref() == Some(c.name.as_str());
                let label = connector_label(&c.name, if on { t.bg } else { t.text });
                chip_frame(t, id_for("conn", &c.name), on, cx, move |this, cx| {
                    this.screen = Screen::Hide { connector: Some(name.clone()) };
                    this.open_picker(crate::pick::Pick::Hide, name.clone(), cx);
                })
                .child(label)
            })
            .collect::<Vec<_>>();
        let hidden: BTreeSet<(String, String, String)> = self
            .hidden
            .iter()
            .map(|h| (h.connector.clone(), h.kind.clone(), h.id.clone()))
            .collect();
        column()
            .child(div().font_weight(MEDIUM).child("CONNECTOR"))
            .child(div().flex().flex_wrap().gap(chars(1.)).children(conns))
            .when(self.picker.is_some(), |d| {
                d.child(self.picker(t, Vec::new(), cx, |c, line, cx| {
                    let conn =
                        self.picker.as_ref().map(|p| p.connector.clone()).unwrap_or_default();
                    let is = |kind: &str, id: &str| {
                        hidden.contains(&(conn.clone(), kind.into(), id.into()))
                    };
                    // The chat itself, then each person in it, hidden by
                    // their own ID: a DM's one person, a group's members.
                    let noun = if c.kind == "mailbox" { "mailbox" } else { "chat" };
                    let chat = (("chat", c.id.clone()), format!("Hide {noun}"), display(c));
                    let people = c.participants.iter().map(|p| {
                        let name = if p.name.is_empty() { p.id.clone() } else { p.name.clone() };
                        (("person", p.id.clone()), format!("Hide {name}"), name)
                    });
                    let links = std::iter::once(chat).chain(people).filter_map(
                        |((kind, id), label, name)| {
                            // A hidden chat already says so on its line.
                            if is(kind, &id) {
                                return (kind != "chat")
                                    .then(|| t.alt(format!("[{name} hidden]")).into_any_element());
                            }
                            let key = id_for(kind, &format!("{}/{id}", c.id));
                            let conn = conn.clone();
                            Some(
                                link(t, key, &label, cx, move |this, cx| {
                                    this.hide(&conn, kind, &id, &name, cx)
                                })
                                .into_any_element(),
                            )
                        },
                    );
                    let links: Vec<AnyElement> =
                        if c.hidden { Vec::new() } else { links.collect() };
                    div()
                        .flex()
                        .flex_wrap()
                        .items_center()
                        .gap_x(chars(1.))
                        .py(lines(0.25))
                        .child(line)
                        .children(links)
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
                let on = g.connector == c.name;
                let label = connector_label(&c.name, if on { t.bg } else { t.text });
                chip_frame(t, id_for("conn", &c.name), on, cx, move |this, cx| {
                    if let Screen::Grant(g) = &mut this.screen {
                        (g.connector, g.chats) = (name.clone(), None);
                    }
                    this.close_picker(cx);
                })
                .child(label)
            })
            .collect::<Vec<_>>();
        let fors = slider(t, "for", stops(&FOR, g.for_, span), g.for_, cx, |this, v| {
            if let Screen::Grant(g) = &mut this.screen {
                g.for_ = v;
            }
        });
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
        let horizons =
            slider(t, "horizon", stops(&HORIZON, g.horizon, horizon), g.horizon, cx, |this, v| {
                if let Screen::Grant(g) = &mut this.screen {
                    g.horizon = v;
                }
            });
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
        // The picked chats as a set, each with its Remove, a whole server
        // with one.
        let chosen: Vec<&crate::api::Chat> = g.chats.iter().flatten().map(|(_, c)| c).collect();
        let unpick: fn(&mut Shell, &str) = |this, id| {
            if let Screen::Grant(g) = &mut this.screen
                && let Some(c) = &mut g.chats
            {
                c.remove(id);
            }
        };
        let picked = self.chat_set(
            t,
            &g.connector,
            &chosen,
            "new-grant",
            words::chat_cols(),
            crate::chats::Whole::Beneath,
            Some(unpick),
            cx,
        );
        let named = self.identities.iter().find(|a| a.id == g.identity).map(|a| a.name.clone());
        let chats = match &g.chats {
            None => Some("every chat".to_string()),
            Some(c) if c.is_empty() => None,
            Some(c) if c.len() == 1 => Some("1 chat".to_string()),
            Some(c) => Some(format!("{} chats", c.len())),
        };
        let window = if g.routine {
            let start = words::time_of_day(&g.start());
            format!("{} {start} for {}", words::days(&g.days), span(g.length))
        } else {
            span(g.for_)
        };
        let short = if g.routine {
            let (days, start) = (words::days_short(&g.days), words::start_short(&g.start()));
            format!("{days} {start} · {}", words::span_short(g.length))
        } else {
            words::span_short(g.for_)
        };
        let ready = named.is_some() && !g.connector.is_empty() && chats.is_some();
        // One line, whatever the room: the form above says the rest. Each
        // part has its compact form, tried before it goes.
        let cols = words::button_cols(in_button());
        let label = match (&named, &chats) {
            (Some(n), Some(c)) if ready => {
                let verb = format!("Grant {}", words::fit(n, cols.saturating_sub(6)));
                let parts = [
                    (verb.clone(), verb),
                    (window, short),
                    (horizon(g.horizon), words::horizon_short(g.horizon)),
                    (c.clone(), c.clone()),
                ];
                words::summary(&parts, cols)
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
                            .child(chip(
                                t,
                                "every",
                                "every chat",
                                g.chats.is_none(),
                                cx,
                                |this, cx| {
                                    if let Screen::Grant(g) = &mut this.screen {
                                        g.chats = None;
                                    }
                                    this.close_picker(cx);
                                },
                            ))
                            .child(chip(
                                t,
                                "only",
                                "only these",
                                g.chats.is_some(),
                                cx,
                                move |this, cx| {
                                    if let Screen::Grant(g) = &mut this.screen
                                        && g.chats.is_none()
                                    {
                                        g.chats = Some(BTreeMap::new());
                                    }
                                    this.open_picker(crate::pick::Pick::Grant, conn.clone(), cx);
                                },
                            )),
                    )
                    .when(g.chats.is_some(), |d| {
                        let pinned: Vec<Container> =
                            ng.chats.iter().flatten().map(|(_, c)| as_row(c)).collect();
                        d.when(!picking, |d| d.child(picked)).when(picking, |d| {
                            d.child(self.picker(t, pinned, cx, |c, line, cx| {
                                if c.hidden {
                                    return div()
                                        .py(lines(0.25))
                                        .pl(chars(3.))
                                        .child(line)
                                        .into_any_element();
                                }
                                let on = matches!(&ng.chats, Some(m) if m.contains_key(&c.id));
                                let (id, chat) = (c.id.clone(), as_chat(c));
                                clickable(id_for("pick", &c.id), cx, move |this, cx| {
                                    if let Screen::Grant(g) = &mut this.screen
                                        && let Some(m) = &mut g.chats
                                        && m.remove(&id).is_none()
                                    {
                                        m.insert(id.clone(), chat.clone());
                                    }
                                    cx.notify();
                                })
                                .flex()
                                .items_center()
                                .gap(chars(1.))
                                .py(lines(0.25))
                                .child(checkbox(t, on))
                                .child(line)
                                .into_any_element()
                            }))
                        })
                    })
            })
            .child(heading("WHEN"))
            .child(
                chips(div())
                    .child(chip(t, "once", "now, for", !g.routine, cx, set(|g| g.routine = false)))
                    .child(chip(
                        t,
                        "routine",
                        "on a schedule",
                        g.routine,
                        cx,
                        set(|g| g.routine = true),
                    )),
            )
            .when(!g.routine, |d| d.child(fors))
            .when(g.routine, |d| {
                d.child(chips(div()).children(days))
                    .child(
                        div()
                            .flex()
                            .items_center()
                            .gap(chars(1.))
                            .child("at")
                            .child(button(
                                t,
                                "earlier",
                                "-",
                                cx,
                                set(|g| g.start = (g.start + 1410) % 1440),
                            ))
                            .child(div().font_weight(MEDIUM).child(words::time_of_day(&g.start())))
                            .child(button(
                                t,
                                "later",
                                "+",
                                cx,
                                set(|g| g.start = (g.start + 30) % 1440),
                            )),
                    )
                    .child(chips(div()).children(lengths))
                    .child(t.alt(format!("in {}", words::zone())))
            })
            .child(heading("HISTORY"))
            .child(horizons)
            .child(approve(t, "grant", label.into(), ready, cx, move |this, cx| {
                this.grant(&ng, cx)
            }))
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
        let pinned = a.ticked().into_iter().map(as_row).collect();
        self.picker(t, pinned, cx, move |c, line, cx| {
            if c.hidden {
                return div().py(lines(0.25)).pl(chars(3.)).child(line).into_any_element();
            }
            let (rid, cid, chat) = (id.clone(), c.id.clone(), as_chat(c));
            clickable(id_for("pick", &c.id), cx, move |this, cx| {
                if let Some(a) = this.adjust.get_mut(&rid) {
                    a.names.entry(cid.clone()).or_insert_with(|| chat.clone());
                    if !a.chats.remove(&cid) {
                        a.chats.insert(cid.clone());
                    }
                }
                cx.notify();
            })
            .flex()
            .items_center()
            .gap(chars(1.))
            .py(lines(0.25))
            .child(checkbox(t, ticked.contains(&c.id)))
            .child(line)
            .into_any_element()
        })
    }

    /// A row's picture: its own, or its outermost parent's when it shows
    /// outside it (a channel shows its server's). Mail has none; inside an
    /// opened row only chats with pictures of their own show one, as the
    /// back row shows the parent's.
    fn row_picture(
        &self,
        t: Theme,
        c: &Container,
        inside: bool,
        cx: &mut Context<Self>,
    ) -> Option<AnyElement> {
        let p = self.picker.as_ref()?;
        if p.connector == "fastmail" || c.kind == "mailbox" || (inside && c.icon.is_none()) {
            return None;
        }
        let (id, icon, name) = match (c.icon.as_deref(), c.parents.first()) {
            (None, Some(top)) => (top.id.as_str(), top.icon.as_deref(), top.name.as_str()),
            (icon, _) => (c.id.as_str(), icon, c.name.as_str()),
        };
        Some(self.picture(t, &p.connector, id, icon, name, cx))
    }
}

/// Picked chats grouped under their outermost parent, in the order they
/// come: a server picked whole heads its own group; a chat with no parent
/// stands alone, with no head.
fn picked_groups(chats: &[Container]) -> Vec<(Option<Container>, Vec<Container>)> {
    let mut groups: Vec<(Option<Container>, Vec<Container>)> = Vec::new();
    for c in chats {
        let head = match (holds_chats(c), c.parents.first()) {
            (true, _) => Some(Container { parents: Vec::new(), ..c.clone() }),
            (false, Some(p)) => Some(Container {
                id: p.id.clone(),
                name: p.name.clone(),
                kind: p.kind.clone(),
                icon: p.icon.clone(),
                ..Container::default()
            }),
            (false, None) => None,
        };
        let key = head.as_ref().map(|h| h.id.clone());
        match groups
            .iter_mut()
            .find(|(h, _)| key.is_some() && h.as_ref().map(|h| &h.id) == key.as_ref())
        {
            Some((h, cs)) => {
                // The server's own row, once it comes, is its head.
                if holds_chats(c) {
                    *h = Some(c.clone());
                }
                cs.push(c.clone())
            }
            None => groups.push((head, vec![c.clone()])),
        }
    }
    groups
}

/// A tree's branch before a chat under its server: "├─", or "└─" for the
/// last, muted.
pub(crate) fn branch(t: Theme, last: bool) -> Div {
    t.alt(if last { "└─" } else { "├─" }).w(chars(3.)).flex_none()
}

/// A chat's own name, else the end of its ID.
fn display(c: &Container) -> String {
    if c.name.is_empty() { words::short_id(&c.id) } else { c.name.clone() }
}

/// A chat's full path name, as a picked chat keeps it: "main / #general".
fn full_name(c: &Container) -> String {
    own_path(c, None).join(" / ")
}

/// A row as a set of chats keeps it: its full path, kind and parents.
fn as_chat(c: &Container) -> crate::api::Chat {
    crate::api::Chat {
        id: c.id.clone(),
        name: full_name(c),
        kind: c.kind.clone(),
        parents: c.parents.clone(),
        icon: c.icon.clone(),
    }
}

/// A picked chat as the picker's row draws it, until its own row comes.
fn as_row(c: &crate::api::Chat) -> Container {
    Container {
        id: c.id.clone(),
        name: c.own().to_string(),
        kind: c.kind.clone(),
        parents: c.parents.clone(),
        icon: c.icon.clone(),
        ..Container::default()
    }
}

/// A chat's path below the row opened, its own name last: "main", "#general"
/// at the top, "#general" inside main.
fn own_path(c: &Container, inside: Option<&Container>) -> Vec<String> {
    let below = match inside.and_then(|i| c.parents.iter().position(|p| p.id == i.id)) {
        Some(i) => &c.parents[i + 1..],
        None => &c.parents[..],
    };
    below.iter().map(|p| p.name.clone()).chain([display(c)]).collect()
}

/// A shelf: a holder or a mailbox, listed apart at the top.
fn is_shelf(c: &Container) -> bool {
    holds_chats(c) || c.kind == "mailbox"
}

/// The top level's label for its shelves, after their kind.
fn shelf_label(kind: &str) -> &'static str {
    match kind {
        "space" => "SPACES",
        "mailbox" => "MAILBOXES",
        _ => "SERVERS",
    }
}
