//! Now, as the console had it: what waits for the owner, open grants,
//! routines, and Stop all at the bottom.

use crate::api::{Activity, Exchange, Grant, Proposal, Request};
use crate::screens::{approve, button, clickable, column, id_for, link, panel, reason, table, who};
use crate::theme::{MEDIUM, Theme, bold, chars, lines};
use crate::words::{self, connector, horizon, span};
use crate::{Adjust, Screen, Shell};
use gpui::{AnyElement, Context, Div, Stateful, div, prelude::*};
use mandate_core::Record;
use serde_json::json;

const FOR: [u64; 7] = [900, 1800, 3600, 7200, 14400, 28800, 43200];
const HORIZON: [u64; 4] = [86400, 604800, 2592000, 0];

impl Shell {
    pub(crate) fn now_tab(&self, t: Theme, cx: &mut Context<Self>) -> Div {
        let mut waiting: Vec<AnyElement> = Vec::new();
        for ex in self.exchanges.iter().filter(|e| self.shown(&e.name)) {
            waiting.push(login_card(t, ex, cx).into_any_element());
        }
        for r in &self.now.requests {
            let names: Vec<&str> = r.chats().iter().map(|c| c.name.as_str()).collect();
            if self.shown(&format!(
                "{} {} {} {}",
                r.identity_name,
                r.connector,
                r.reason,
                names.join(" ")
            )) {
                waiting.push(self.request_card(t, r, cx).into_any_element());
            }
        }
        for p in &self.now.proposals {
            let values: Vec<&str> = p.display.iter().map(|f| f.value.as_str()).collect();
            if self.shown(&format!(
                "{} wants to {} {} {} {}",
                p.identity_name,
                p.op,
                p.connector,
                p.note,
                values.join(" ")
            )) {
                waiting.push(send_card(t, p, cx).into_any_element());
            }
        }
        let shown = |g: &&Grant| {
            self.shown(&format!("{} {} {}", g.identity_name, g.connector, chat_list(g)))
        };
        let open: Vec<&Grant> = self.now.grants.iter().filter(shown).collect();
        let routines: Vec<&Grant> = self.now.routines.iter().filter(shown).collect();
        let stopped: Vec<&Grant> = self.now.stopped.iter().filter(shown).collect();
        column()
            .gap(lines(1.))
            .child(t.heading("Waiting for you"))
            .when(waiting.is_empty(), |d| {
                let (first, then) = match &self.search {
                    Some(q) => (
                        format!("Nothing here matches \"{q}\"."),
                        "Clear the search to see everything.",
                    ),
                    None => {
                        ("Nothing needs you right now.".into(), "Agents' requests show up here.")
                    }
                };
                d.child(
                    column()
                        .border_2()
                        .border_dashed()
                        .border_color(t.alt)
                        .px(chars(1.))
                        .py(lines(1.))
                        .child(first)
                        .child(t.alt(then)),
                )
            })
            .children(waiting)
            .when(!open.is_empty(), |d| {
                d.child(t.heading("Open now"))
                    .children(open.into_iter().map(|g| open_grant(t, g, cx)))
            })
            .when(!routines.is_empty() || !stopped.is_empty(), |d| {
                d.child(t.heading("Routines"))
                    .children(routines.into_iter().map(|g| routine(t, g, false, cx)))
                    .children(stopped.into_iter().map(|g| routine(t, g, true, cx)))
            })
            .child(self.stop_all_control(t, cx))
    }

    fn request_card(&self, t: Theme, r: &Request, cx: &mut Context<Self>) -> Div {
        let a = self.adjust.get(&r.id).cloned().unwrap_or_else(|| Adjust::of(r));
        let chats = r.chats();
        let named = r.terms.is_some();
        let nothing = named && chats.is_empty();
        let (verb, window) = match (r.kind.as_str(), &r.schedule) {
            ("extend", _) => {
                (format!("asks for {} more", span(r.for_)), format!("{} more", span(r.for_)))
            }
            (_, Some(s)) => (
                "wants to read".into(),
                format!("{} {} for {}", words::days(&s.days), s.start, span(s.length)),
            ),
            _ => ("wants to read".into(), span(a.for_)),
        };
        let chat_cell: AnyElement = if !named {
            div().child("every chat").into_any_element()
        } else {
            let unmatched =
                r.terms.iter().flatten().filter(|t| t.chats.as_ref().is_none_or(Vec::is_empty));
            column()
                .children(chats.iter().map(|c| {
                    div()
                        .flex()
                        .flex_wrap()
                        .gap_x(chars(1.))
                        .child(c.name.clone())
                        .child(t.alt(words::short_id(&c.id)))
                }))
                .children(unmatched.map(|u| t.alt(format!("no chat matched \"{}\"", u.term))))
                .into_any_element()
        };
        // An extend keeps the grant's chats and horizon; it asks only for time.
        let mut rows = vec![("Window".to_string(), div().child(window.clone()).into_any_element())];
        if r.kind != "extend" {
            rows.insert(0, ("Chats".into(), chat_cell));
            rows.push(("History".into(), div().child(horizon(a.horizon)).into_any_element()));
        }
        let chosen = match a.chats.len() {
            _ if !named => Some("every chat".to_string()),
            0 => None,
            1 => chats.iter().find(|c| a.chats.contains(&c.id)).map(|c| c.name.clone()),
            n => Some(format!("{n} chats")),
        };
        let label = match (&chosen, r.kind.as_str()) {
            (_, "extend") => format!("Approve · {window}"),
            (Some(c), _) => format!("Approve · {window} · {c} · {}", horizon(a.horizon)),
            (None, _) => "Approve".into(),
        };
        let ready = !nothing && (chosen.is_some() || r.kind == "extend");
        let (id, terms) =
            (r.id.clone(), r.terms.iter().flatten().map(|t| format!("\"{}\"", t.term)));
        let deny = format!("requests/{id}/deny");
        let name = r.identity_name.clone();
        let (ra, rb) = (r.clone(), r.clone());
        panel(t)
            .child(
                column()
                    .child(who(&r.identity_name, &verb))
                    .child(div().font_weight(MEDIUM).child(connector(&r.connector))),
            )
            .child(table(t, rows))
            .when(nothing, |d| {
                d.child(format!(
                    "Nothing matched {} on {}.",
                    terms.collect::<Vec<_>>().join(" or "),
                    connector(&r.connector)
                ))
            })
            .children(reason(t, &r.reason))
            .when(a.open, |d| d.child(adjust_panel(t, r, &a, cx)))
            .when(!nothing, |d| {
                d.child(approve(
                    t,
                    id_for("approve", &id),
                    label.into(),
                    ready,
                    cx,
                    move |this, cx| this.approve_request(&ra, cx),
                ))
            })
            .when(named && !nothing && a.chats.is_empty(), |d| {
                d.child(t.alt("tick the chats to grant"))
            })
            .child(
                div()
                    .flex()
                    .gap(chars(1.))
                    .when(r.kind != "extend" && !nothing, |d| {
                        d.child(
                            button(t, id_for("adjust", &id), "Adjust", cx, move |this, cx| {
                                let a = this
                                    .adjust
                                    .entry(rb.id.clone())
                                    .or_insert_with(|| Adjust::of(&rb));
                                a.open = !a.open;
                                cx.notify();
                            })
                            .flex_1(),
                        )
                    })
                    .child(
                        button(t, id_for("deny", &id), "Deny", cx, move |this, cx| {
                            let notice = format!("You denied {name}'s request.");
                            this.post("POST", deny.clone(), cx, move |_| Some(notice))
                        })
                        .flex_1(),
                    ),
            )
    }

    pub(crate) fn activity(&self, a: &Activity) -> String {
        let name = self
            .identities
            .iter()
            .find(|i| i.id == a.identity)
            .map_or("An agent", |i| i.name.as_str());
        let conn = connector(&a.connector);
        let chats: Vec<&str> = a
            .chats
            .iter()
            .flatten()
            .map(|c| if c.name.is_empty() { c.id.as_str() } else { c.name.as_str() })
            .collect();
        let chats = chats.join(", ");
        let n = |n: u64, what: &str| format!("{n} {what}{}", if n == 1 { "" } else { "s" });
        match a.verb.as_str() {
            "messages" => format!("{name} read {chats} on {conn} · {}", n(a.count, "message")),
            "chats" => format!("{name} listed {conn} chats"),
            "search" => format!("{name} searched {conn} · {}", n(a.count, "result")),
            "media" => format!("{name} opened a file in {chats}"),
            "send" => format!("You sent {name}'s message to {chats} on {conn}"),
            op => format!("You let {name} {op} on {conn}"),
        }
    }

    fn stop_all_control(&self, t: Theme, cx: &mut Context<Self>) -> AnyElement {
        if !self.stopping {
            return link(t, "stop", "stop all access…", cx, |this, cx| {
                this.stopping = true;
                cx.notify();
            })
            .mt(lines(2.))
            .into_any_element();
        }
        panel(t)
            .mt(lines(2.))
            .child("Close every grant, pause every routine and deny every waiting request? Waiting sends stay.")
            .child(
                div()
                    .flex()
                    .gap(chars(1.))
                    .child(button(t, "stop-all", "Stop all", cx, |this, cx| this.stop_all(cx)).flex_1().bg(t.text).text_color(t.bg))
                    .child(
                        button(t, "cancel", "Cancel", cx, |this, cx| {
                            this.stopping = false;
                            cx.notify();
                        })
                        .flex_1(),
                    ),
            )
            .into_any_element()
    }
}

fn adjust_panel(t: Theme, r: &Request, a: &Adjust, cx: &mut Context<Shell>) -> Div {
    let named = r.terms.is_some();
    let ticks = r.chats().into_iter().map(|c| {
        let (r, id, on) = (r.clone(), c.id.clone(), a.chats.contains(&c.id));
        clickable(id_for("tick", &c.id), cx, move |this, cx| {
            let a = this.adjust.entry(r.id.clone()).or_insert_with(|| Adjust::of(&r));
            if !a.chats.remove(&id) {
                a.chats.insert(id.clone());
            }
            cx.notify();
        })
        .flex()
        .gap(chars(1.))
        .child(checkbox(t, on))
        .child(c.name.clone())
    });
    let fors: Vec<u64> = FOR.into_iter().filter(|&f| f < r.for_).chain([r.for_]).collect();
    let horizons: Vec<u64> = HORIZON
        .into_iter()
        .filter(|&h| r.horizon == 0 || (h != 0 && h < r.horizon))
        .chain((r.horizon != 0).then_some(r.horizon))
        .collect();
    let choices = |field: &'static str,
                   values: Vec<u64>,
                   current: u64,
                   word: fn(u64) -> String,
                   cx: &mut Context<Shell>| {
        div().flex().flex_wrap().gap(chars(1.)).children(values.into_iter().map(|v| {
            let r = r.clone();
            chip(t, (field, v as usize), &word(v), v == current, cx, move |this, cx| {
                let a = this.adjust.entry(r.id.clone()).or_insert_with(|| Adjust::of(&r));
                if field == "for" {
                    a.for_ = v;
                } else {
                    a.horizon = v;
                }
                cx.notify();
            })
        }))
    };
    column()
        .gap(lines(1.))
        .border_t_2()
        .border_color(t.alt)
        .pt(lines(1.))
        .when(named, |d| {
            d.child(div().font_weight(MEDIUM).child("CHATS")).child(column().children(ticks))
        })
        .when(r.kind == "once", |d| {
            d.child(div().font_weight(MEDIUM).child("FOR"))
                .child(choices("for", fors, a.for_, span, cx))
        })
        .child(div().font_weight(MEDIUM).child("MESSAGES FROM"))
        .child(choices("horizon", horizons, a.horizon, horizon, cx))
}

fn login_card(t: Theme, ex: &Exchange, cx: &mut Context<Shell>) -> Div {
    let what = if ex.kind == "device" { "wants to join as a device" } else { "wants to log in" };
    let mut rows = Vec::new();
    if !ex.address.is_empty() {
        rows.push(("From".to_string(), div().child(ex.address.clone()).into_any_element()));
    }
    rows.push(("Since".into(), div().child(words::clock(&ex.started_at)).into_any_element()));
    let ex2 = ex.clone();
    panel(t).child(who(&ex.name, what)).child(table(t, rows)).child(button(
        t,
        id_for("code", &ex.id),
        "Type the code",
        cx,
        move |this, cx| this.go(Screen::Code { exchange: ex2.clone(), code: String::new() }, cx),
    ))
}

fn send_card(t: Theme, p: &Proposal, cx: &mut Context<Shell>) -> Div {
    let to = p.field("To").map(|to| format!(" → {to}")).unwrap_or_default();
    let (a, b, deny, name) =
        (p.clone(), p.clone(), format!("proposals/{}/deny", p.id), p.identity_name.clone());
    let verb =
        if p.op == "send" { "wants to send".to_string() } else { format!("wants to {}", p.op) };
    panel(t)
        .child(
            column()
                .child(who(&p.identity_name, &verb))
                .child(div().font_weight(MEDIUM).child(format!("{}{to}", connector(&p.connector)))),
        )
        .children(p.field("Text").map(|text| text.to_string()))
        .children(reason(t, &p.note))
        .child(approve(
            t,
            id_for("send", &p.id),
            "Approve and send".into(),
            true,
            cx,
            move |this, cx| this.approve_send(&a, cx),
        ))
        .child(
            div()
                .flex()
                .gap(chars(1.))
                .child(
                    button(t, id_for("review", &p.id), "Review", cx, move |this, cx| {
                        this.review(b.clone(), cx)
                    })
                    .flex_1(),
                )
                .child(
                    button(t, id_for("deny", &p.id), "Deny", cx, move |this, cx| {
                        let notice = format!("You denied {name}'s send.");
                        this.post("POST", deny.clone(), cx, move |_| Some(notice))
                    })
                    .flex_1(),
                ),
        )
}

fn chat_list(g: &Grant) -> String {
    if g.chats.is_empty() {
        return "every chat".into();
    }
    let names: Vec<&str> =
        g.chats.iter().map(|id| g.chat_names.get(id).map_or(id.as_str(), String::as_str)).collect();
    names.join(", ")
}

fn open_grant(t: Theme, g: &Grant, cx: &mut Context<Shell>) -> Div {
    let until =
        g.until.as_deref().map(|u| format!("[until {} · {}]", words::clock(u), words::left(u)));
    let (id, conn) = (g.id.clone(), g.connector.clone());
    let close = format!("grants/{id}/close");
    panel(t)
        .border_color(t.open)
        .child(
            column()
                .child(bold(format!("{} · {}", g.identity_name, connector(&g.connector))))
                .children(until.map(|u| div().text_color(t.open).child(u))),
        )
        .child(chat_list(g))
        .child(t.alt(horizon(g.horizon)))
        .child(
            div()
                .flex()
                .gap(chars(1.))
                .child(button(t, id_for("extend", &id), "+30m", cx, move |this, cx| {
                    let conn = conn.clone();
                    let check =
                        move |r: &Record| matches!(r, Record::Grant(g) if g.connector == conn);
                    this.sign(
                        format!("grants/{id}/extend/challenge"),
                        json!({"for": 1800}),
                        check,
                        cx,
                    )
                }))
                .child(button(t, id_for("close", &g.id), "Close", cx, move |this, cx| {
                    this.post("POST", close.clone(), cx, |_| Some("Closed.".into()))
                })),
        )
}

fn routine(t: Theme, g: &Grant, stopped: bool, cx: &mut Context<Shell>) -> Div {
    let when = g.schedule.as_ref().map_or(String::new(), |s| {
        format!("{} at {} for {} · ", words::days(&s.days), s.start, span(s.length))
    });
    let id = g.id.clone();
    let running = g
        .schedule
        .as_ref()
        .filter(|_| !stopped)
        .and_then(words::running)
        .map(|end| format!("[running until {end}]"));
    let action = if stopped {
        button(t, id_for("resume", &id), "Resume", cx, move |this, cx| {
            let check = |r: &Record| matches!(r, Record::Grant(g) if g.schedule.is_some());
            this.sign(format!("grants/{id}/resume/challenge"), json!({}), check, cx)
        })
    } else {
        button(t, id_for("pause", &id), "Pause", cx, move |this, cx| {
            this.post("POST", format!("grants/{id}/close"), cx, |_| Some("Paused.".into()))
        })
    };
    panel(t)
        .when(stopped, |d| d.border_dashed())
        .when(!stopped, |d| d.border_color(t.routine))
        .child(
            column()
                .child(
                    div()
                        .flex()
                        .justify_between()
                        .child(bold(format!("{} · {}", g.identity_name, connector(&g.connector))))
                        .when(stopped, |d| d.child(t.alt("[paused]"))),
                )
                .children(running.map(|r| div().text_color(t.routine).child(r))),
        )
        .child(chat_list(g))
        .child(t.alt(format!("{when}{}", horizon(g.horizon))))
        .child(div().flex().child(action))
}

fn checkbox(t: Theme, on: bool) -> Div {
    div()
        .w(chars(2.))
        .h(lines(1.))
        .flex_none()
        .flex()
        .items_center()
        .justify_center()
        .border_2()
        .border_color(t.text)
        .when(on, |d| d.child(div().w(chars(1.)).h(lines(0.5)).bg(t.text)))
}

fn chip(
    t: Theme,
    id: (&'static str, usize),
    label: &str,
    on: bool,
    cx: &mut Context<Shell>,
    f: impl Fn(&mut Shell, &mut Context<Shell>) + 'static,
) -> Stateful<Div> {
    clickable(id, cx, f)
        .px(chars(1.))
        .py(lines(0.5))
        .border_2()
        .border_color(t.text)
        .when(on, |d| d.bg(t.text).text_color(t.bg))
        .child(label.to_string())
}
