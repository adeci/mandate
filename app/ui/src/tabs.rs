//! Agents, Connections, Activity and Settings, as the console laid them out.

use crate::Shell;
use crate::api::Grant;
use crate::device::hex;
use crate::screens::{column, id_for, link, panel, table};
use crate::theme::{MEDIUM, Theme, bold, chars, lines};
use crate::words::{self, connector, horizon};
use gpui::{AnyElement, Context, Div, div, prelude::*};
use mandate_core::{Op, Record, Trust};
use serde_json::json;

impl Shell {
    pub(crate) fn agents_tab(&self, t: Theme, cx: &mut Context<Self>) -> Div {
        let agents = self.identities.iter().filter(|a| self.shown(&a.name)).map(|a| {
            let seen = a.last_seen.as_deref().map_or("not connected yet".into(), |s| {
                format!("last seen {} {}", words::date(s), words::clock(s))
            });
            let confirm = self.deleting.as_deref() == Some(a.id.as_str());
            let (id, name) = (a.id.clone(), a.name.clone());
            let open: Vec<&Grant> = a.grants.iter().filter(|g| g.closed_at.is_none()).collect();
            panel(t)
                .child(
                    column()
                        .child(bold(a.name.clone()))
                        .child(t.alt(format!("created {} · {seen}", words::date(&a.created_at)))),
                )
                .when(open.is_empty(), |d| d.child(t.alt("No grants: this agent reads nothing.")))
                .children(open.into_iter().map(|g| grant_line(t, g)))
                .child(
                    link(
                        t,
                        id_for("delete", &a.id),
                        if confirm { "Confirm delete" } else { "Delete agent" },
                        cx,
                        move |this, cx| {
                            if this.deleting.as_deref() == Some(id.as_str()) {
                                let notice = format!("You deleted {name}.");
                                this.post("DELETE", format!("identities/{id}"), cx, move |_| {
                                    Some(notice)
                                });
                            } else {
                                this.deleting = Some(id.clone());
                                cx.notify();
                            }
                        },
                    )
                    .font_weight(MEDIUM),
                )
        });
        column()
            .gap(lines(1.))
            .when(self.identities.is_empty(), |d| {
                d.child(t.alt("No agents yet. An agent logs in with mandate login, and you type its code on Now."))
            })
            .children(agents)
    }

    pub(crate) fn connections_tab(&self, t: Theme) -> Div {
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
                        .child(div().flex_1().child(v))
                }))
        });
        column()
            .gap(lines(1.))
            .when(self.connectors.is_empty(), |d| d.child(t.alt("No connectors configured.")))
            .children(panels)
    }

    pub(crate) fn activity_tab(&self, t: Theme) -> Div {
        let rows: Vec<(String, AnyElement)> = self
            .now
            .activity
            .iter()
            .map(|a| (words::clock(&a.at), self.activity(a)))
            .filter(|(_, text)| self.shown(text))
            .map(|(at, text)| (at, div().child(text).into_any_element()))
            .collect();
        column().gap(lines(1.)).child(t.heading("Today")).child(if rows.is_empty() {
            t.alt("Nothing happened yet today.").into_any_element()
        } else {
            table(t, rows).into_any_element()
        })
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
        let hidden = self.hidden.iter().enumerate().map(|(i, h)| {
            let path = format!("never-share/{}/{}/{}", h.connector, h.kind, h.id);
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
        let key = match self.device.as_ref().map(|d| d.client.trust()) {
            Some(Trust::Pin(k)) => {
                let k = hex(&k);
                (0..k.len()).step_by(4).map(|i| &k[i..i + 4]).collect::<Vec<_>>().join(" ")
            }
            _ => "checked against the usual certificate roots".into(),
        };
        column()
            .gap(lines(1.))
            .child(t.heading("Devices"))
            .child(column().border_2().border_color(t.text).children(devices))
            .child(t.alt("Each device can approve. Another device joins with a code you type on Now."))
            .child(t.heading("Never share"))
            .child(if self.hidden.is_empty() {
                t.alt("Nothing is hidden. Chats and people hidden here stay out of every read, whatever the grants say.")
                    .into_any_element()
            } else {
                column().border_2().border_color(t.text).children(hidden).into_any_element()
            })
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
        (_, Some(s)) => format!("[{} at {}]", words::days(&s.days), s.start),
        (Some(u), None) => format!("[until {}]", words::clock(u)),
        _ => String::new(),
    };
    column()
        .child(format!("{chats} · {}", connector(&g.connector)))
        .child(div().text_color(if g.schedule.is_some() { t.routine } else { t.open }).child(state))
        .child(t.alt(horizon(g.horizon)))
}
