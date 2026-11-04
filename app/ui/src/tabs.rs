//! Agents, Connections, Activity and Settings, as the console laid them out.

use crate::api::{Activity, Grant, History};
use crate::pick::NewGrant;
use crate::screens::{button, column, id_for, link, panel, table};
use crate::theme::{MEDIUM, Theme, bold, chars, lines};
use crate::words::{self, connector, horizon};
use crate::{Screen, Shell, segment};
use gpui::{Context, Div, canvas, div, prelude::*};
use mandate_core::{Op, Record, Time, Trust};
use serde_json::json;

impl Shell {
    pub(crate) fn agents_tab(&self, t: Theme, cx: &mut Context<Self>) -> Div {
        let add = button(t, "add-agent", "Add an agent", cx, |this, cx| this.invite("agent", cx));
        let empty = self.identities.is_empty().then(|| t.alt("No agents yet."));
        let agents = self.identities.iter().filter(|a| self.shown(&a.name)).map(|a| {
            let seen = a.last_seen.as_deref().map_or("not connected yet".into(), |s| {
                format!("last seen {} {}", words::date(s), words::clock(s))
            });
            let confirm = self.confirming.as_deref() == Some(a.id.as_str());
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
                    link(
                        t,
                        id_for("delete", &a.id),
                        if confirm { "Confirm delete" } else { "Delete agent" },
                        cx,
                        move |this, cx| {
                            if this.confirming.as_deref() == Some(id.as_str()) {
                                let notice = format!("You deleted {name}.");
                                this.post("DELETE", format!("identities/{id}"), cx, move |_| {
                                    Some(notice)
                                });
                            } else {
                                this.confirming = Some(id.clone());
                                cx.notify();
                            }
                        },
                    )
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

    /// Link for a connector waiting to be linked; Unlink, asked twice, for
    /// one that is.
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
        let key = format!("unlink {name}");
        let confirm = self.confirming.as_deref() == Some(key.as_str());
        let warning = format!("Unlink {}?", connector(&name));
        column().gap(lines(0.5)).when(confirm, |d| d.child(warning)).child(
            link(
                t,
                id_for("unlink", &name),
                if confirm { "Confirm unlink" } else { "Unlink" },
                cx,
                move |this, cx| {
                    if this.confirming.as_deref() == Some(key.as_str()) {
                        this.unlink(name.clone(), cx);
                    } else {
                        this.confirming = Some(key.clone());
                        cx.notify();
                    }
                },
            )
            .font_weight(MEDIUM),
        )
    }

    /// Everything that happened, newest first, under the local day it
    /// happened; scrolling near the end loads further back.
    pub(crate) fn activity_tab(&self, t: Theme, _cx: &mut Context<Self>) -> Div {
        let Some(history) = &self.history else {
            return column().child(t.alt("Loading activity…"));
        };
        let today = words::day(Time::now().unix());
        let mut days: Vec<(String, Vec<(String, String)>)> = Vec::new();
        for a in &history.entries {
            let line = self.entry(a);
            if !self.shown(&line) {
                continue;
            }
            let day = words::date(&a.at);
            let row = (words::clock(&a.at), line);
            match days.last_mut() {
                Some((d, rows)) if *d == day => rows.push(row),
                _ => days.push((day, vec![row])),
            }
        }
        self.day_tops.borrow_mut().clear();
        let sections = days.into_iter().map(|(day, rows)| {
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
            column().child(div().relative().child(day_heading(&title)).child(mark)).children(
                rows.into_iter().map(|(time, line)| {
                    div()
                        .flex()
                        .gap(chars(1.))
                        .py(lines(0.25))
                        .child(t.alt(time).flex_none())
                        .child(div().flex_1().min_w_0().child(line))
                }),
            )
        });
        let end = match (&history.next, history.entries.is_empty()) {
            (Some(_), _) => Some(t.alt("Loading more…")),
            (None, true) => Some(t.alt("Nothing yet.")),
            (None, false) => None,
        };
        column().gap(lines(1.)).children(sections).children(end)
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
            "request.asked" if d.is_empty() => format!("{who} asked to read {on}"),
            "request.asked" => format!("{who} asked to read {on}: \"{d}\""),
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
            "device.joined" => format!("{d} joined as a device"),
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
        let path = match &before {
            Some(next) => format!("activity?limit=100&before={}", segment(next)),
            None => "activity?limit=100".into(),
        };
        let client = self.device().client;
        self.call(
            cx,
            move || Ok(client.get::<History>(&path)),
            move |this, page, _| {
                this.history_loading = false;
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

    /// This device and the others, what is never shared, and the server.
    pub(crate) fn settings(&self, t: Theme, cx: &mut Context<Self>) -> Div {
        let removable = self.keys.trusted.len() > 1;
        let devices = self.keys.trusted.iter().enumerate().map(|(i, k)| {
            let id = k.id.clone();
            row(t, i)
                .child(
                    div()
                        .flex_1()
                        .child(k.name.clone())
                        .when(k.id == self.key_id, |d| d.child(div().text_color(t.info).child("[this device]"))),
                )
                .when(removable, |d| {
                    d.child(link(t, id_for("remove", &k.id), "Remove", cx, move |this, cx| {
                        let (path, id) = (format!("keys/{id}/remove/challenge"), id.clone());
                        let check = move |r: &Record| matches!(r, Record::Key(e) if e.op == Op::Remove && e.key.id == id);
                        this.sign(path, json!({}), check, cx);
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
        // The same short key the setup page and `mandate setup` print.
        let key = match self.device.as_ref().map(|d| d.client.trust()) {
            Some(Trust::Pin(k)) => crate::screens::fingerprint(&k),
            _ => "certificate verified".into(),
        };
        #[cfg(target_os = "android")]
        let this_key: Option<Vec<(String, gpui::AnyElement)>> = None;
        #[cfg(not(target_os = "android"))]
        let this_key = self.device.as_ref().map(|d| {
            let rows = d.key.rows().into_iter();
            rows.map(|(k, v)| (k.to_string(), div().child(v).into_any_element())).collect()
        });
        column()
            .gap(lines(1.))
            .child(t.heading("Devices"))
            .child(column().border_2().border_color(t.text).children(devices))
            .child(div().flex().child(add_device))
            .when_some(this_key, |d, rows| {
                d.child(t.heading("This computer's key")).child(table(t, rows))
            })
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
