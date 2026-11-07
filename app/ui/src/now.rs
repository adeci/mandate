//! Now: what waits for the owner, open grants and routines.

use crate::api::{Exchange, Grant, Proposal, Request};
use crate::pick::Pick;
use crate::screens::{
    DEVICE_JOIN, approve, button, clickable, column, connector_label, deny_exchange, device_rows,
    device_warning, headline, id_for, link, panel, reason, slider, table, who,
};
use crate::theme::{BOLD, MEDIUM, Theme, bold, chars, lines};
use crate::words::{self, connector, horizon, span};
use crate::{Adjust, Shell};
use gpui::{AnyElement, Context, Div, ElementId, Stateful, div, hsla, prelude::*, px, relative};
use mandate_core::Record;
use serde_json::json;

/// The windows and the history a grant can give, as slider stops.
pub(crate) const FOR: [u64; 7] = [900, 1800, 3600, 7200, 14400, 28800, 43200];
pub(crate) const HORIZON: [u64; 6] = [86400, 259200, 604800, 2592000, 7776000, 0];

impl Shell {
    pub(crate) fn now_tab(&self, t: Theme, cx: &mut Context<Self>) -> Div {
        let mut waiting: Vec<AnyElement> = Vec::new();
        // An exchange shows here once its party's code matched.
        let confirmed = self.exchanges.iter().filter(|e| e.state == "confirmed");
        for ex in confirmed.filter(|e| self.shown(&e.name)) {
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
                let empty = match &self.search {
                    Some(q) => format!("Nothing matches \"{q}\"."),
                    None => "Nothing waiting.".into(),
                };
                d.child(
                    column()
                        .border_2()
                        .border_dashed()
                        .border_color(t.alt)
                        .px(chars(1.))
                        .py(lines(1.))
                        .child(empty),
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
    }

    fn request_card(&self, t: Theme, r: &Request, cx: &mut Context<Self>) -> Div {
        let a = self.adjust.get(&r.id).cloned().unwrap_or_else(|| Adjust::of(r));
        let named = r.terms.is_some();
        // The terms matched nothing, and the owner has picked nothing yet.
        let nothing = named && a.names.is_empty();
        let (verb, window) = match (r.kind.as_str(), &r.schedule) {
            ("extend", _) => {
                (format!("asks for {} more", span(r.for_)), format!("{} more", span(r.for_)))
            }
            (_, Some(s)) => (
                "wants to read".into(),
                format!(
                    "{} {} for {}",
                    words::days(&s.days),
                    words::time_of_day(&s.start),
                    span(s.length)
                ),
            ),
            _ => ("wants to read".into(), span(a.for_)),
        };
        let chat_cell: AnyElement = if !named {
            div().child("every chat").into_any_element()
        } else {
            let unmatched =
                r.terms.iter().flatten().filter(|t| t.chats.as_ref().is_none_or(Vec::is_empty));
            // A term that matched nothing is one tap from the picker, its
            // words already searched.
            let unmatched = unmatched.enumerate().map(|(i, u)| {
                let (r, term) = (r.clone(), u.term.clone());
                div()
                    .flex()
                    .flex_wrap()
                    .gap_x(chars(1.))
                    .child(
                        t.alt(format!("Nothing matched \"{}\"", words::chat_narrow(&u.term)))
                            .min_w_0(),
                    )
                    .child(
                        link(
                            t,
                            id_for("choose", &format!("{}/{i}", r.id)),
                            "Choose",
                            cx,
                            move |this, cx| this.choose_for(&r, &term, cx),
                        )
                        .font_weight(MEDIUM),
                    )
            });
            let unmatched: Vec<_> = unmatched.collect();
            column()
                .children(a.names.iter().map(|(id, name)| {
                    // An ID only tells apart chats that share a name.
                    let twin = a.names.iter().any(|(other, n)| other != id && n == name);
                    div()
                        .flex()
                        .flex_wrap()
                        .gap_x(chars(1.))
                        .child(div().min_w_0().child(words::chat_narrow(name)))
                        .when(twin, |d| d.child(t.alt(words::short_id(id))))
                }))
                .children(unmatched)
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
            1 => a.chats.first().and_then(|id| a.names.get(id)).map(|n| words::chat(n)),
            n => Some(format!("{n} chats")),
        };
        let label = match (&chosen, r.kind.as_str()) {
            (_, "extend") => format!("Approve · {window}"),
            (Some(c), _) => format!("Approve · {window} · {c} · {}", horizon(a.horizon)),
            (None, _) => "Approve".into(),
        };
        let ready = chosen.is_some() || r.kind == "extend";
        let id = r.id.clone();
        let deny = format!("requests/{id}/deny");
        let name = r.identity_name.clone();
        let (ra, rb) = (r.clone(), r.clone());
        let adjust = (r.kind != "extend").then(|| {
            let label = if nothing { "Pick chats" } else { "Adjust" };
            div().flex().justify_end().child(
                link(t, id_for("adjust", &id), label, cx, move |this, cx| {
                    this.open_adjust(&rb, cx)
                })
                .font_weight(MEDIUM),
            )
        });
        // Deny sits above, so that Approve's full width at the bottom is
        // where the thumb lands, and nothing else is.
        panel(t)
            .child(headline(t, &r.identity_name, &verb, &r.connector))
            .child(column().gap(lines(0.25)).child(table(t, rows)).children(adjust))
            .children(reason(t, &r.reason))
            .child(button(t, id_for("deny", &id), "Deny", cx, move |this, cx| {
                let notice = format!("You denied {name}'s request.");
                this.post("POST", deny.clone(), cx, move |_| Some(notice))
            }))
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
    }

    /// Opens Adjust for one request, closing it on any other; with nothing
    /// matched, the chat picker opens with it.
    fn open_adjust(&mut self, r: &Request, cx: &mut Context<Self>) {
        for a in self.adjust.values_mut() {
            a.open = false;
        }
        let a = self.adjust.entry(r.id.clone()).or_insert_with(|| Adjust::of(r));
        a.open = true;
        // Named chats are picked in the one list that searches them all.
        if r.terms.is_some() {
            self.open_picker(Pick::Request(r.id.clone()), r.connector.clone(), cx);
        } else {
            self.close_picker(cx);
        }
    }

    /// Opens the chat picker of a request with a term that matched nothing
    /// already searched, for the owner to pick from.
    fn choose_for(&mut self, r: &Request, term: &str, cx: &mut Context<Self>) {
        self.open_adjust(r, cx);
        if let Some(p) = &mut self.picker {
            p.q = term.to_string();
        }
        self.find_chats(cx);
    }

    /// Closes Adjust, keeping what the owner chose; false if none was open.
    pub(crate) fn close_adjust(&mut self, cx: &mut Context<Self>) -> bool {
        let Some(a) = self.adjust.values_mut().find(|a| a.open) else { return false };
        a.open = false;
        self.close_picker(cx);
        true
    }

    /// Adjust, as a sheet from the bottom over Now: chats, window and
    /// history, then Done. bottom is the inset the window draws under.
    pub(crate) fn adjust_sheet(
        &self,
        t: Theme,
        bottom: f32,
        cx: &mut Context<Self>,
    ) -> Option<Div> {
        let r =
            self.now.requests.iter().find(|r| self.adjust.get(&r.id).is_some_and(|a| a.open))?;
        let a = &self.adjust[&r.id];
        let picking = self.picker.as_ref().is_some_and(|p| p.pick == Pick::Request(r.id.clone()));
        let picker = picking.then(|| self.request_picker(t, &r.id, a, cx));
        // Modal, as every sheet: the scrim takes each touch outside it.
        let scrim = div().id("scrim").occlude().flex_1().bg(hsla(0., 0., 0., 0.4)).on_click(
            cx.listener(|this, _, _, cx| {
                this.close_adjust(cx);
            }),
        );
        let sheet = crate::scroll::view(
            "sheet",
            div()
                .occlude()
                .min_h_0()
                .bg(t.bg)
                .border_t_2()
                .border_color(t.text)
                .pt(lines(1.))
                .pb(px(bottom) + lines(1.))
                .px(chars(1.)),
            column()
                .w_full()
                .max_w(chars(80.))
                .mx_auto()
                .gap(lines(1.))
                .child(headline(t, &r.identity_name, "wants to read", &r.connector))
                .child(adjust_panel(t, r, a, picker, cx))
                .child(button(t, "done", "Done", cx, |this, cx| {
                    this.close_adjust(cx);
                })),
        );
        Some(
            div().absolute().top_0().left_0().size_full().flex().flex_col().child(scrim).child(
                div()
                    .relative()
                    .flex()
                    .flex_col()
                    .max_h(relative(0.85))
                    .child(sheet)
                    .child(crate::screens::modal()),
            ),
        )
    }
}

/// The narrowing controls; picker, when open, lists the connector's chats
/// under the ticked ones.
fn adjust_panel(
    t: Theme,
    r: &Request,
    a: &Adjust,
    picker: Option<Div>,
    cx: &mut Context<Shell>,
) -> Div {
    let named = r.terms.is_some();
    let ticks = a.names.iter().map(|(cid, name)| {
        let (r, id, on) = (r.clone(), cid.clone(), a.chats.contains(cid));
        clickable(id_for("tick", cid), cx, move |this, cx| {
            let a = this.adjust.entry(r.id.clone()).or_insert_with(|| Adjust::of(&r));
            if !a.chats.remove(&id) {
                a.chats.insert(id.clone());
            }
            cx.notify();
        })
        .flex()
        .gap(chars(1.))
        .child(checkbox(t, on))
        .child(div().min_w_0().truncate().child(words::chat(name)))
    });
    let ticks: Vec<_> = ticks.collect();
    let (rf, rh) = (r.clone(), r.clone());
    column()
        .gap(lines(1.))
        .when(named, |d| {
            d.child(div().font_weight(MEDIUM).child("CHATS")).child(match picker {
                Some(p) => p.into_any_element(),
                None => column().children(ticks).into_any_element(),
            })
        })
        .when(r.kind == "once", |d| {
            d.child(div().font_weight(MEDIUM).child("WINDOW")).child(slider(
                t,
                "window",
                stops(&FOR, r.for_, span),
                a.for_,
                cx,
                move |this, v| {
                    this.adjust.entry(rf.id.clone()).or_insert_with(|| Adjust::of(&rf)).for_ = v
                },
            ))
        })
        .child(div().font_weight(MEDIUM).child("HISTORY"))
        .child(slider(
            t,
            "history",
            stops(&HORIZON, r.horizon, horizon),
            a.horizon,
            cx,
            move |this, v| {
                this.adjust.entry(rh.id.clone()).or_insert_with(|| Adjust::of(&rh)).horizon = v
            },
        ))
}

/// A slider's stops: the usual ones and the one asked for, in order, all
/// history (0) last, each with its words.
pub(crate) fn stops(usual: &[u64], asked: u64, word: fn(u64) -> String) -> Vec<(u64, String)> {
    let mut v: Vec<u64> = usual.iter().copied().chain([asked]).collect();
    v.sort_unstable_by_key(|&s| if s == 0 { u64::MAX } else { s });
    v.dedup();
    v.into_iter().map(|s| (s, word(s))).collect()
}

/// A party whose code matched, waiting to be let in.
fn login_card(t: Theme, ex: &Exchange, cx: &mut Context<Shell>) -> Div {
    let ex2 = ex.clone();
    let review = button(t, id_for("review", &ex.id), "Review", cx, move |this, cx| {
        this.admit_exchange(ex2.clone(), cx)
    });
    let since = ("Since".to_string(), div().child(words::clock(&ex.started_at)).into_any_element());
    let deny = deny_exchange(t, ex, cx);
    if ex.kind == "device" {
        let mut rows = device_rows(ex);
        rows.push(since);
        return panel(t)
            .child(bold(DEVICE_JOIN.to_string()))
            .child(table(t, rows))
            .child(device_warning(t))
            .child(deny)
            .child(review);
    }
    let mut rows = Vec::new();
    if !ex.address.is_empty() {
        rows.push(("From".to_string(), div().child(ex.address.clone()).into_any_element()));
    }
    rows.push(since);
    panel(t)
        .child(who(&ex.name, "wants to join as an agent"))
        .child(table(t, rows))
        .child(deny)
        .child(review)
}

fn send_card(t: Theme, p: &Proposal, cx: &mut Context<Shell>) -> Div {
    let to = p.field("To").map(|to| format!(" → {to}")).unwrap_or_default();
    let (a, b, deny, name) =
        (p.clone(), p.clone(), format!("proposals/{}/deny", p.id), p.identity_name.clone());
    let verb =
        if p.op == "send" { "wants to send".to_string() } else { format!("wants to {}", p.op) };
    panel(t)
        .child(
            headline(t, &p.identity_name, &verb, &p.connector).children(
                to.split(' ').filter(|w| !w.is_empty()).map(|w| div().child(w.to_string())),
            ),
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

/// "laptop · [mark] Signal", in bold.
fn grant_title(t: Theme, g: &Grant) -> Div {
    div()
        .flex()
        .flex_wrap()
        .items_center()
        .gap_x(chars(1.))
        .font_weight(BOLD)
        .child(g.identity_name.clone())
        .child("·")
        .child(connector_label(&g.connector, t.text))
}

fn chat_list(g: &Grant) -> String {
    if g.chats.is_empty() {
        return "every chat".into();
    }
    let name = |id: &String| words::chat(g.chat_names.get(id).unwrap_or(id));
    g.chats.iter().map(name).collect::<Vec<_>>().join(", ")
}

fn open_grant(t: Theme, g: &Grant, cx: &mut Context<Shell>) -> Div {
    let until =
        g.until.as_deref().map(|u| format!("[until {} · {}]", words::clock(u), words::left(u)));
    let (id, id2, conn) = (g.id.clone(), g.id.clone(), g.connector.clone());
    let what = format!("{}'s grant on {}", g.identity_name, connector(&g.connector));
    panel(t)
        .border_color(t.open)
        .child(
            column()
                .child(grant_title(t, g))
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
                    let (id, what) = (id2.clone(), what.clone());
                    this.ask(crate::Confirm::Close { id, what }, cx)
                })),
        )
}

fn routine(t: Theme, g: &Grant, stopped: bool, cx: &mut Context<Shell>) -> Div {
    let when = g.schedule.as_ref().map_or(String::new(), |s| {
        let start = words::time_of_day(&s.start);
        format!("{} at {start} for {} · ", words::days(&s.days), span(s.length))
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
                        .child(grant_title(t, g))
                        .when(stopped, |d| d.child(t.alt("[paused]"))),
                )
                .children(running.map(|r| div().text_color(t.routine).child(r))),
        )
        .child(chat_list(g))
        .child(t.alt(format!("{when}{}", horizon(g.horizon))))
        .child(div().flex().child(action))
}

pub(crate) fn checkbox(t: Theme, on: bool) -> Div {
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

pub(crate) fn chip(
    t: Theme,
    id: impl Into<ElementId>,
    label: &str,
    on: bool,
    cx: &mut Context<Shell>,
    f: impl Fn(&mut Shell, &mut Context<Shell>) + 'static,
) -> Stateful<Div> {
    chip_frame(t, id, on, cx, f).child(label.to_string())
}

/// A chip without its label, for chips that hold more than text.
pub(crate) fn chip_frame(
    t: Theme,
    id: impl Into<ElementId>,
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
}
