//! Agents, Connections, Activity and Settings, as the console laid them out.

use crate::api::{Activity, Grant, History};
use crate::pick::NewGrant;
use crate::screens::{button, clickable, column, id_for, link, panel, table};
use crate::theme::{MEDIUM, Theme, bold, chars, lines};
use crate::words::{self, connector, horizon};
use crate::{Confirm, Next, Screen, Shell, segment};
use gpui::{Context, Div, SharedString, canvas, div, prelude::*};
use mandate_core::{Time, Trust};
use serde_json::json;

impl Shell {
    pub(crate) fn agents_tab(&self, t: Theme, cx: &mut Context<Self>) -> Div {
        let add = button(t, "add-agent", "Add an agent", cx, |this, cx| this.invite("agent", cx));
        let empty = self.identities.is_empty().then(|| t.alt("No agents yet."));
        let agents = self.identities.iter().filter(|a| self.shown(&a.name)).map(|a| {
            let seen = a.last_seen.as_deref().map_or("not connected yet".into(), |s| {
                format!("last seen {} {}", words::date(s), words::clock(s))
            });
            let (id, name, grantee) = (a.id.clone(), a.name.clone(), a.id.clone());
            let open: Vec<&Grant> = a.grants.iter().filter(|g| g.closed_at.is_none()).collect();
            panel(t)
                .child(
                    column()
                        .child(bold(a.name.clone()))
                        .child(t.alt(format!("created {} · {seen}", words::date(&a.created_at)))),
                )
                .when(open.is_empty(), |d| d.child(t.alt("No grants.")))
                .children(open.into_iter().map(|g| grant_line(t, g)))
                .child(div().flex().child(button(
                    t,
                    id_for("grant", &a.id),
                    "New grant",
                    cx,
                    move |this, cx| {
                        let mut g = NewGrant::new(grantee.clone());
                        let linked: Vec<_> = this.connectors.iter().filter(|c| !c.setup).collect();
                        if let [c] = linked[..] {
                            g.connector = c.name.clone();
                        }
                        this.go(Screen::Grant(g), cx)
                    },
                )))
                .child(
                    link(t, id_for("delete", &a.id), "Delete agent", cx, move |this, cx| {
                        let (id, name) = (id.clone(), name.clone());
                        this.ask(Confirm::DeleteAgent { id, name }, cx)
                    })
                    .font_weight(MEDIUM),
                )
        });
        column().gap(lines(1.)).children(empty).child(div().flex().child(add)).children(agents)
    }

    pub(crate) fn connections_tab(&self, t: Theme, cx: &mut Context<Self>) -> Div {
        let panels = self.connectors.iter().filter(|c| self.shown(&c.name)).map(|c| {
            let state = if c.setup { "setup" } else { c.health.state.as_str() };
            let mut rows: Vec<(&str, String)> = Vec::new();
            if !c.health.detail.is_empty() {
                rows.push(("detail", c.health.detail.clone()));
            } else if c.setup {
                rows.push(("detail", "not linked".into()));
            }
            if let Some(u) = &c.health.until {
                rows.push(("until", format!("{} {}", words::clock(u), words::left(u))));
            }
            panel(t)
                .when(c.setup, |d| d.border_dashed())
                .child(
                    div().flex().justify_between().child(bold(connector(&c.name))).child(
                        div()
                            .when(state != "ok", |d| d.font_weight(MEDIUM))
                            .child(format!("[{state}]")),
                    ),
                )
                .children(rows.into_iter().map(|(k, v)| {
                    div()
                        .flex()
                        .gap(chars(2.))
                        .child(t.alt(k).w(chars(8.)).flex_none())
                        .child(div().flex_1().min_w_0().child(v))
                }))
                .child(self.link_control(t, &c.name, c.setup, cx))
        });
        column()
            .gap(lines(1.))
            .when(self.connectors.is_empty(), |d| d.child(t.alt("No connectors configured.")))
            .children(panels)
    }

    /// Link for a connector waiting to be linked; Unlink for one that is.
    fn link_control(&self, t: Theme, name: &str, setup: bool, cx: &mut Context<Self>) -> Div {
        let name = name.to_string();
        if setup {
            return div().flex().child(button(
                t,
                id_for("link", &name),
                "Link",
                cx,
                move |this, cx| this.open_link(name.clone(), cx),
            ));
        }
        div().child(
            link(t, id_for("unlink", &name), "Unlink", cx, move |this, cx| {
                this.ask(Confirm::Unlink { name: name.clone() }, cx)
            })
            .font_weight(MEDIUM),
        )
    }

    /// Everything that happened, newest first, under the local day it
    /// happened; scrolling near the end loads further back.
    pub(crate) fn activity_tab(&self, t: Theme, cx: &mut Context<Self>) -> Div {
        let Some(history) = &self.history else {
            return column().child(t.alt("Loading…"));
        };
        let today = words::day(Time::now().unix());
        let mut days: Vec<(String, Vec<&Activity>)> = Vec::new();
        for a in &history.entries {
            let day = words::date(&a.at);
            match days.last_mut() {
                Some((d, rows)) if *d == day => rows.push(a),
                _ => days.push((day, vec![a])),
            }
        }
        self.day_tops.borrow_mut().clear();
        let mut n = 0usize;
        let sections: Vec<Div> = days
            .into_iter()
            .map(|(day, rows)| {
                let title = if day == today { "Today".to_string() } else { day };
                // Each header says where it is, for the one that sticks on top.
                let tops = self.day_tops.clone();
                let at = title.clone();
                let mark = canvas(
                    move |bounds, _, _| tops.borrow_mut().push((at, f32::from(bounds.top()))),
                    |_, _, _, _| {},
                )
                .absolute()
                .size_full();
                // A row keeps its laid-out height by when it happened (and
                // its place among entries of the same moment), so newer
                // entries arriving on top don't disturb it.
                let mut same = (None, 0usize);
                let rows: Vec<_> = rows
                    .into_iter()
                    .map(|a| {
                        n += 1;
                        same = (Some(&a.at), if same.0 == Some(&a.at) { same.1 + 1 } else { 0 });
                        let shown = a.clone();
                        let row = clickable(("entry", n), cx, move |this, cx| {
                            this.go(Screen::Entry(Box::new(shown.clone())), cx)
                        })
                        .flex()
                        .gap(chars(2.))
                        .py(lines(0.5))
                        .border_t_1()
                        .border_color(t.bg_alt)
                        .child(t.alt(words::clock(&a.at)).flex_none())
                        .child(div().flex_1().min_w_0().child(self.entry(a)));
                        crate::scroll::lazy((SharedString::from(a.at.clone()), same.1), row)
                    })
                    .collect();
                column()
                    .child(div().relative().child(day_heading(&title)).child(mark))
                    .children(rows)
            })
            .collect();
        let end = match (&history.next, history.entries.is_empty(), &self.search) {
            (Some(_), _, _) => Some(t.alt("Loading more…")),
            (None, true, Some(q)) => Some(t.alt(format!("Nothing matches \"{q}\"."))),
            (None, true, None) => Some(t.alt("Nothing yet.")),
            (None, false, _) => None,
        };
        column().gap(lines(1.)).children(sections).children(end)
    }

    /// An entry in full: who, what, the agent's words, the chats and the
    /// terms, what was approved or sent, when, and on which device.
    pub(crate) fn entry_screen(&self, t: Theme, a: &Activity) -> Div {
        let who = self.identities.iter().find(|i| i.id == a.identity).map(|i| i.name.clone());
        let chats: Vec<&str> = a
            .chats
            .iter()
            .map(|c| if c.name.is_empty() { c.id.as_str() } else { c.name.as_str() })
            .collect();
        let terms: Vec<String> = a.terms.iter().map(|t| format!("\"{}\"", t.term)).collect();
        let mut rows: Vec<(String, String)> =
            vec![("When".into(), format!("{} {}", words::date(&a.at), words::clock(&a.at)))];
        let mut add = |label: &str, value: String| {
            if !value.is_empty() {
                rows.push((label.into(), value));
            }
        };
        add("Agent", who.unwrap_or_default());
        add(
            "Account",
            if a.connector.is_empty() { String::new() } else { connector(&a.connector) },
        );
        add("Asked for", terms.join(" or "));
        add("Chats", chats.join(", "));
        add("Reason", a.reason.clone());
        add("Note", a.note.clone());
        // A send's detail is what was approved, field by field.
        if a.kind.starts_with("proposal.") && a.detail.contains(": ") {
            for field in a.detail.split("; ") {
                let (label, value) = field.split_once(": ").unwrap_or(("", field));
                add(if label.is_empty() { "Detail" } else { label }, value.to_string());
            }
        } else if !a.kind.starts_with("read") && !a.kind.starts_with("request.asked") {
            add("Detail", a.detail.clone());
        }
        if let Some(n) = a.count.filter(|_| a.kind == "read") {
            add("Count", n.to_string());
        }
        add("Device", a.device.clone());
        let rows = rows.into_iter().map(|(l, v)| (l, div().child(v).into_any_element())).collect();
        column().gap(lines(1.)).child(bold(self.entry(a))).child(table(t, rows))
    }

    /// One plain line for an entry: who did what, and how it ended.
    fn entry(&self, a: &Activity) -> String {
        let who = self
            .identities
            .iter()
            .find(|i| i.id == a.identity)
            .map_or("an agent", |i| i.name.as_str());
        let on = connector(&a.connector);
        let chats: Vec<&str> = a
            .chats
            .iter()
            .map(|c| if c.name.is_empty() { c.id.as_str() } else { c.name.as_str() })
            .collect();
        let chats = chats.join(", ");
        let n = |what: &str| {
            let n = a.count.unwrap_or(0);
            format!("{n} {what}{}", if n == 1 { "" } else { "s" })
        };
        let d = &a.detail;
        match a.kind.as_str() {
            "read" => match d.as_str() {
                "messages" => format!("{who} read {chats} on {on} · {}", n("message")),
                "chats" => format!("{who} listed {on} chats"),
                "search" => format!("{who} searched {on} · {}", n("result")),
                "media" => format!("{who} opened a file in {chats} on {on}"),
                op => format!("{who} used {op} on {on}"),
            },
            "request.asked" if a.reason.is_empty() => format!("{who} asked to read {on}"),
            "request.asked" => format!("{who} asked to read {on}: \"{}\"", a.reason),
            "request.approved" => format!("you approved {who} on {on} · {d}"),
            "request.denied" => format!("you denied {who}'s request for {on}"),
            "request.expired" => format!("{who}'s request for {on} expired"),
            "proposal.made" => format!("{who} asked to {d} on {on}"),
            "proposal.approved" => format!("you approved {who}'s send on {on} · {d}"),
            "proposal.done" => format!("sent · {who} on {on} · {d}"),
            "proposal.failed" => format!("failed · {who} on {on} · {d}"),
            "proposal.unknown" => format!("may have sent · {who} on {on} · {d}"),
            "proposal.denied" => format!("you denied {who}'s send on {on}"),
            "proposal.expired" => format!("{who}'s {d} on {on} expired"),
            "grant.made" => format!("you granted {who} {on}"),
            "grant.extended" => format!("you extended {who}'s grant on {on} {d}"),
            "grant.closed" => format!("{who}'s grant on {on} closed"),
            "routine.paused" => format!("you paused {who}'s routine on {on}"),
            "routine.resumed" => format!("you resumed {who}'s routine on {on}"),
            "agent.admitted" => format!("you let {d} in"),
            "agent.deleted" => format!("you deleted {d}"),
            "device.paired" => format!("{d} paired"),
            "device.joined" => format!("{d} joined"),
            "key.added" => format!("{d} changed how it approves"),
            "key.retired" => format!("{d}'s old key retired"),
            "key.removed" => format!("you removed {d}"),
            "stop_all" => format!("you stopped all access · {d}"),
            kind => format!("{kind} · {d}"),
        }
    }

    /// The day whose header has scrolled above the page, to keep on top.
    pub(crate) fn sticky_day(&self) -> Option<String> {
        let top = f32::from(self.scroll.bounds().top());
        let tops = self.day_tops.borrow();
        tops.iter().take_while(|(_, y)| *y < top).last().map(|(day, _)| day.clone())
    }

    /// Loads the newest history, ahead of what is already loaded.
    pub(crate) fn load_history(&mut self, cx: &mut Context<Self>) {
        self.page_history(None, cx);
    }

    /// Loads the page before the last loaded, while there is one.
    pub(crate) fn more_history(&mut self, cx: &mut Context<Self>) {
        if let Some(next) = self.history.as_ref().and_then(|h| h.next.clone()) {
            self.page_history(Some(next), cx);
        }
    }

    fn page_history(&mut self, before: Option<String>, cx: &mut Context<Self>) {
        if self.history_loading {
            return;
        }
        self.history_loading = true;
        let q = self.history_q.clone();
        let mut path = "activity?limit=100".to_string();
        if let Some(next) = &before {
            path += &format!("&before={}", segment(next));
        }
        if !q.is_empty() {
            path += &format!("&q={}", segment(&q));
        }
        let client = self.device().client;
        self.call(
            cx,
            move || Ok(client.get::<History>(&path)),
            move |this, page, _| {
                this.history_loading = false;
                // The search changed meanwhile; this page is for the old one.
                if this.history_q != q {
                    return;
                }
                let page = match page {
                    Ok(page) => page,
                    // No retry on every frame: the next refresh tries the
                    // newest page again.
                    Err(e) => {
                        let h = this.history.get_or_insert_with(History::default);
                        h.next = None;
                        return this.error = Some(e.to_string());
                    }
                };
                match (&mut this.history, before) {
                    // Older entries go after what is loaded.
                    (Some(h), Some(_)) => {
                        h.entries.extend(page.entries);
                        h.next = page.next;
                    }
                    // Newer ones go before it, up to the first already there.
                    (Some(h), None) if !h.entries.is_empty() => {
                        let first = (h.entries[0].at.clone(), h.entries[0].kind.clone());
                        let fresh: Vec<Activity> = page
                            .entries
                            .into_iter()
                            .take_while(|e| (&e.at, &e.kind) != (&first.0, &first.1))
                            .collect();
                        h.entries.splice(0..0, fresh);
                    }
                    _ => this.history = Some(page),
                }
            },
        );
    }

    /// Saves the name typed for this device. A name is the owner's own
    /// label, so nothing is signed.
    pub(crate) fn rename(&mut self, cx: &mut Context<Self>) {
        let Some(name) = self.renaming.take().map(|n| n.trim().to_string()) else { return };
        crate::keyboard(false);
        if name.is_empty() {
            return cx.notify();
        }
        let (client, path) = (self.device().client, format!("keys/{}", segment(&self.key_id)));
        self.call(
            cx,
            move || client.send::<serde_json::Value>("PATCH", &path, &json!({ "name": name })),
            |this, _, cx| this.refresh(cx),
        );
    }

    /// This device and the others, what is never shared, and the server.
    pub(crate) fn settings(&self, t: Theme, cx: &mut Context<Self>) -> Div {
        // A key retired by rotation is no device of its own any more.
        let active: Vec<_> =
            self.keys.trusted.iter().filter(|k| !self.keys.retired.contains(&k.id)).collect();
        let removable = active.len() > 1;
        let devices =
            active.into_iter().enumerate().map(|(i, k)| {
                let id = k.id.clone();
                let name = self.keys.name(k).to_string();
                let name2 = name.clone();
                let this_one = k.id == self.key_id;
                if let Some(typed) = self.renaming.as_ref().filter(|_| this_one) {
                    let field = crate::screens::field(t, "rename", typed, "Name", true, |_| {}, cx);
                    return row(t, i).child(div().flex_1().min_w_0().child(field)).child(button(
                        t,
                        "save-name",
                        "Save",
                        cx,
                        |this, cx| this.rename(cx),
                    ));
                }
                row(t, i)
                    .child(div().flex_1().child(name.clone()).when(this_one, |d| {
                        d.child(div().text_color(t.info).child("[this device]"))
                    }))
                    .when(this_one, |d| {
                        d.child(link(t, "rename", "Rename", cx, move |this, cx| {
                            this.renaming = Some(name.clone());
                            crate::keyboard(true);
                            cx.notify();
                        }))
                    })
                    .when(removable, |d| {
                        d.child(link(t, id_for("remove", &k.id), "Remove", cx, move |this, cx| {
                            let (id, name) = (id.clone(), name2.clone());
                            this.ask(Confirm::RemoveDevice { id, name }, cx)
                        }))
                    })
            });
        let devices: Vec<Div> = devices.collect();
        let add_device =
            button(t, "add-device", "Add a device", cx, |this, cx| this.invite("device", cx));
        let hidden = self.hidden.iter().enumerate().map(|(i, h)| {
            let path =
                format!("never-share/{}/{}/{}", segment(&h.connector), h.kind, segment(&h.id));
            let name = if h.name.is_empty() { h.id.clone() } else { h.name.clone() };
            row(t, i)
                .child(column().flex_1().child(name.clone()).child(t.alt(format!(
                    "{} on {}",
                    h.kind,
                    connector(&h.connector)
                ))))
                .child(link(t, id_for("share", &path), "Share again", cx, move |this, cx| {
                    let notice = format!("{name} is shared again, where grants allow.");
                    this.post("DELETE", path.clone(), cx, move |_| Some(notice))
                }))
        });
        let hidden: Vec<Div> = hidden.collect();
        // The same short key the setup page and `mandate setup` print.
        let key = match self.device.as_ref().map(|d| d.client.trust()) {
            Some(Trust::Pin(k)) => crate::screens::fingerprint(&k),
            _ => "certificate verified".into(),
        };
        // How this device approves, and a way to change it: a new key under
        // the new way takes this one's place.
        #[cfg(target_os = "android")]
        let (approve_rows, changeable) = {
            let way = match self.device.as_ref().map(|d| d.key.method()) {
                Some(crate::android::Method::ScreenLock) => "Screen lock",
                _ => "Fingerprint or face",
            };
            let rows = vec![("Way".to_string(), div().child(way).into_any_element())];
            (rows, crate::android::biometrics_enrolled())
        };
        #[cfg(not(target_os = "android"))]
        let (approve_rows, changeable) = {
            let rows = self.device.as_ref().map(|d| d.key.rows()).unwrap_or_default();
            let rows =
                rows.into_iter().map(|(k, v)| (k.to_string(), div().child(v).into_any_element()));
            (rows.collect::<Vec<_>>(), true)
        };
        let change = changeable.then(|| {
            div().flex().child(button(t, "change-way", "Change", cx, |this, cx| {
                this.new_key(Next::Rotate, cx)
            }))
        });
        let lock = clickable("lock", cx, |this, cx| this.set_lock(!this.lock, cx))
            .flex()
            .items_center()
            .gap(chars(1.))
            .child(crate::now::checkbox(t, self.lock))
            .child("Lock mandate when I leave it");
        let stop =
            button(t, "stop-all", "Stop all access", cx, |this, cx| this.ask(Confirm::StopAll, cx))
                .mt(lines(2.))
                .border_color(t.danger)
                .text_color(t.danger);
        column()
            .gap(lines(1.))
            .child(t.heading("Devices"))
            .child(column().border_2().border_color(t.text).children(devices))
            .child(div().flex().child(add_device))
            .child(t.heading("How you approve"))
            .child(table(t, approve_rows))
            .children(change)
            .child(lock)
            .child(t.heading("Never share"))
            .child(if self.hidden.is_empty() {
                t.alt("Nothing hidden.").into_any_element()
            } else {
                column().border_2().border_color(t.text).children(hidden).into_any_element()
            })
            .child(div().flex().child(button(t, "hide", "Add a chat or person", cx, |this, cx| {
                // With one connector there is nothing to choose.
                let only = match &this.connectors[..] {
                    [c] => Some(c.name.clone()),
                    _ => None,
                };
                this.go(Screen::Hide { connector: only.clone() }, cx);
                if let Some(c) = only {
                    this.open_picker(crate::pick::Pick::Hide, c, cx);
                }
            })))
            .child(t.heading("Server"))
            .child(table(
                t,
                vec![
                    ("Address".into(), div().child(self.server.clone()).into_any_element()),
                    ("Key".into(), div().child(key).into_any_element()),
                ],
            ))
            .child(stop)
    }
}

/// A row of a ruled list.
fn row(t: Theme, i: usize) -> Div {
    div()
        .flex()
        .items_center()
        .gap(chars(1.))
        .px(chars(1.))
        .py(lines(0.5))
        .when(i > 0, |d| d.border_t_2().border_color(t.text))
}

fn grant_line(t: Theme, g: &Grant) -> Div {
    let chats = if g.chats.is_empty() {
        "every chat".to_string()
    } else {
        g.chats
            .iter()
            .map(|id| g.chat_names.get(id).map_or(id.as_str(), String::as_str))
            .collect::<Vec<_>>()
            .join(", ")
    };
    let state = match (&g.until, &g.schedule) {
        (_, Some(s)) => {
            format!("[{} at {}]", words::days(&s.days), words::time_of_day(&s.start))
        }
        (Some(u), None) => format!("[until {}]", words::clock(u)),
        _ => String::new(),
    };
    column()
        .child(format!("{chats} · {}", connector(&g.connector)))
        .child(div().text_color(if g.schedule.is_some() { t.routine } else { t.open }).child(state))
        .child(t.alt(horizon(g.horizon)))
}

/// A day's header in Activity, in the list and stuck on top.
pub(crate) fn day_heading(day: &str) -> Div {
    bold(day.to_uppercase()).py(lines(0.5))
}
