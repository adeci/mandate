//! Agents, Connections, Activity and Settings, as the console laid them out.

use crate::api::{Activity, Chat, Days, Grant, History};
use crate::chats::Whole;
use crate::now::checkbox;
use crate::pick::NewGrant;
use crate::screens::{
    bottom_sheet, button, clickable, column, connector_label, icon, icon_button, id_for, link,
    panel, shade, table,
};
use crate::theme::{BOLD, MEDIUM, Theme, bold, chars, lines};
use crate::words::{self, connector, horizon};
use crate::{Confirm, Next, Screen, Shell, segment};
use gpui::{
    Animation, AnimationExt, AnyElement, Context, Div, ElementId, Pixels, SharedString, canvas,
    div, prelude::*, px,
};
use mandate_core::{Time, Trust};
use serde_json::{Value, json};
use std::collections::HashMap;
use std::rc::Rc;
use std::time::Duration;

impl Shell {
    pub(crate) fn agents_tab(&self, t: Theme, cx: &mut Context<Self>) -> Div {
        let add = button(t, "add-agent", "Add an agent", cx, |this, cx| this.invite("agent", cx));
        let empty = self.identities.is_empty().then(|| t.alt("No agents yet."));
        let agents = self.identities.iter().filter(|a| self.shown(&a.name)).map(|a| {
            let seen = a.last_seen.as_deref().map_or("not connected yet".into(), |s| {
                format!("last seen {} {}", words::date(s), words::clock(s))
            });
            let (id, name, grantee) = (a.id.clone(), a.name.clone(), a.id.clone());
            let open: Vec<Div> = a
                .grants
                .iter()
                .filter(|g| g.closed_at.is_none())
                .map(|g| self.grant_line(t, g, cx))
                .collect();
            panel(t)
                .child(
                    column()
                        .child(bold(a.name.clone()))
                        .child(t.alt(format!("created {} · {seen}", words::date(&a.created_at)))),
                )
                .when(open.is_empty(), |d| d.child(t.alt("No grants.")))
                .children(open)
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
                    div()
                        .flex()
                        .justify_between()
                        .child(connector_label(&c.name, t.text).font_weight(BOLD))
                        .child(
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
    /// happened: kinds to narrow it, a day to start at, and scrolling near
    /// the end loads further back.
    pub(crate) fn activity_tab(&self, t: Theme, cx: &mut Context<Self>) -> Div {
        let changing = self.history.as_ref().is_some_and(|h| h.query != self.wanted);
        let top = self.history_filters(t, changing, cx);
        let Some(history) = &self.history else {
            return column().gap(lines(1.)).child(top).child(t.alt("Loading…"));
        };
        let today = words::day(Time::now().unix());
        let mut days: Vec<(String, Vec<&Rc<Activity>>)> = Vec::new();
        for a in &history.entries {
            let day = words::date(&a.at);
            match days.last_mut() {
                Some((d, rows)) if *d == day => rows.push(a),
                _ => days.push((day, vec![a])),
            }
        }
        let names: Rc<HashMap<String, String>> =
            Rc::new(self.identities.iter().map(|i| (i.id.clone(), i.name.clone())).collect());
        let shell = cx.entity().downgrade();
        self.day_tops.borrow_mut().clear();
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
                // A row keeps its laid-out size by when it happened (and its
                // place among entries of the same moment), so newer entries
                // arriving on top don't disturb it; it is built only while
                // it shows.
                let mut same = (None, 0usize);
                let rows: Vec<_> = rows
                    .into_iter()
                    .map(|a| {
                        same = (Some(&a.at), if same.0 == Some(&a.at) { same.1 + 1 } else { 0 });
                        let id: ElementId = (SharedString::from(a.at.clone()), same.1).into();
                        let (a, names, shell) = (a.clone(), names.clone(), shell.clone());
                        crate::scroll::lazy_with(id.clone(), move |_, _| {
                            let who = names.get(&a.identity).map_or("an agent", String::as_str);
                            // A run of reads shows its start; its line says
                            // it is a run, and the entry its span.
                            let (time, text) = (words::clock(&a.at), Shell::line(&a, who));
                            // Device and key events name no connector: a blank, so times align.
                            let mark = (!a.connector.is_empty())
                                .then(|| words::connector_icon(&a.connector));
                            div()
                                .id(id)
                                .cursor_pointer()
                                .on_click(move |_, _, cx| {
                                    let a = a.clone();
                                    let _ =
                                        shell.update(cx, |this, cx| this.go(Screen::Entry(a), cx));
                                })
                                .flex()
                                .gap(chars(2.))
                                .py(lines(0.5))
                                .border_t_1()
                                .border_color(t.bg_alt)
                                .child(shade())
                                .child(
                                    div()
                                        .flex()
                                        .flex_none()
                                        .gap(chars(1.))
                                        .child(match mark {
                                            Some(m) => icon(m, t.alt).into_any_element(),
                                            None => div()
                                                .w(gpui::px(20.))
                                                .flex_none()
                                                .into_any_element(),
                                        })
                                        .child(t.alt(time).w(chars(9.))),
                                )
                                .child(div().flex_1().min_w_0().child(text))
                                .into_any_element()
                        })
                    })
                    .collect();
                column()
                    .child(div().relative().child(day_heading(&title)).child(mark))
                    .children(rows)
            })
            .collect();
        let query = &history.query;
        let narrowed = !query.q.is_empty() || query.kinds.is_some() || query.days.is_some();
        let end = match (&history.next, history.entries.is_empty()) {
            (Some(_), _) => Some(t.alt("Loading more…")),
            (None, true) if narrowed => Some(t.alt("Nothing matches.")),
            (None, true) => Some(t.alt("Nothing yet.")),
            (None, false) => None,
        };
        // One change, one swap: while a change loads, the rows it replaces
        // stay and, past a moment, dim a little; the new rows fade in.
        let list = column().gap(lines(1.)).children(sections).children(end);
        let list = if changing {
            let key = format!("dim {:?}", self.wanted);
            list.with_animation(ElementId::Name(key.into()), Animation::new(SLOW + FADE), |d, v| {
                let late = SLOW.as_secs_f32() / (SLOW + FADE).as_secs_f32();
                d.opacity(1. - 0.35 * ((v - late) / (1. - late)).max(0.))
            })
            .into_any_element()
        } else {
            let key = format!("fresh {:?}", history.query);
            list.with_animation(ElementId::Name(key.into()), Animation::new(FADE), |d, v| {
                d.opacity(0.65 + 0.35 * v)
            })
            .into_any_element()
        };
        column().gap(lines(1.)).child(top).child(list)
    }

    /// Two equal controls, the kinds shown and the days shown; under them a
    /// rule that turns emerald when a change takes a while to load.
    fn history_filters(&self, t: Theme, changing: bool, cx: &mut Context<Self>) -> Div {
        let kind = KINDS.iter().find(|(_, k)| *k == self.wanted.kinds).map_or("", |(l, _)| *l);
        let shown = self.wanted.days.map_or(Time::now().unix(), |(_, last)| last);
        let (year, month, _) = words::ymd(shown);
        let days = HistorySheet::Days { year, month: month as i32, months: false };
        let open = |sheet: HistorySheet| {
            move |this: &mut Shell, cx: &mut Context<Shell>| {
                // A sheet never rides on the keyboard.
                crate::keyboard(false);
                this.history_sheet = Some(sheet);
                cx.notify();
            }
        };
        let control =
            |id: &'static str, label: String, sheet: HistorySheet, cx: &mut Context<Self>| {
                clickable(id, cx, open(sheet))
                    .flex_1()
                    .min_w_0()
                    .h_full()
                    .flex()
                    .items_center()
                    .justify_between()
                    .gap(chars(1.))
                    .px(chars(1.))
                    .child(div().min_w_0().truncate().child(label))
                    .child(icon("chevron-down", t.text))
            };
        let frame = || div().flex_1().min_w_0().h(TARGET).flex().border_2().border_color(t.text);
        let day = match self.wanted.days {
            Some(range) => {
                frame().child(control("day-filter", range_label(range), days, cx)).child(
                    icon_button(t, "day-clear", "x", cx, |this, cx| {
                        this.wanted.days = None;
                        cx.notify();
                    })
                    .h_full(),
                )
            }
            None => frame().child(control("day-filter", "Latest".into(), days, cx)),
        };
        // The rule shows only once a load has taken a moment.
        let rule = div().h(px(2.)).when(changing, |d| d.bg(t.grant_hi));
        let rule = match changing {
            true => rule
                .with_animation(
                    ElementId::Name(format!("rule {:?}", self.wanted).into()),
                    Animation::new(SLOW),
                    |d, v| d.opacity(if v < 1. { 0. } else { 1. }),
                )
                .into_any_element(),
            false => rule.into_any_element(),
        };
        column()
            .gap(lines(0.5))
            .child(
                div()
                    .flex()
                    .gap(chars(1.))
                    .child(frame().child(control(
                        "kind-filter",
                        kind.into(),
                        HistorySheet::Kinds,
                        cx,
                    )))
                    .child(day),
            )
            .child(rule)
    }

    /// Picks a day for Activity: a first tap shows that day alone, a second
    /// one makes the range between the two and closes the sheet.
    fn pick_day(&mut self, day: i64, cx: &mut Context<Self>) {
        match self.wanted.days {
            Some((first, last)) if first == last && first != day => {
                self.wanted.days = Some((first.min(day), first.max(day)));
                self.history_sheet = None;
            }
            _ => self.wanted.days = Some((day, day)),
        }
        cx.notify();
    }

    /// Activity's open sheet: the kinds to show, or a day to start at.
    pub(crate) fn history_sheet(
        &self,
        t: Theme,
        bottom: f32,
        cx: &mut Context<Self>,
    ) -> Option<Div> {
        let content = match self.history_sheet? {
            HistorySheet::Kinds => {
                let rows = KINDS.iter().enumerate().map(|(i, (label, kinds))| {
                    clickable(("kind", i), cx, move |this, cx| {
                        (this.wanted.kinds, this.history_sheet) = (*kinds, None);
                        cx.notify();
                    })
                    .h(TARGET)
                    .flex()
                    .items_center()
                    .gap(chars(1.))
                    .child(checkbox(t, self.wanted.kinds == *kinds))
                    .child(label.to_string())
                });
                column().child(bold("Show").pb(lines(0.5))).children(rows)
            }
            HistorySheet::Days { year, month, months: false } => self.days(t, year, month, cx),
            HistorySheet::Days { year, month, months: true } => self.months(t, year, month, cx),
        };
        let content = column().gap(lines(1.)).child(content);
        Some(bottom_sheet(t, bottom, cx, |this| this.history_sheet = None, content))
    }

    /// A month of days to start the list at, those with entries marked and
    /// the rest dim; the title opens the year's months.
    fn days(&self, t: Theme, year: i32, month: i32, cx: &mut Context<Self>) -> Div {
        let first = words::midnight(year, month, 1);
        let (_, _, days) = words::ymd(words::midnight(year, month + 1, 0));
        let now = Time::now().unix();
        let today = words::iso_day(now);
        let marks = self.marks.as_ref().and_then(|m| m.days.as_ref());
        let later = words::midnight(year, month + 1, 1) <= now;
        let head = sheet_head(
            t,
            words::month(first),
            days_of(year, month - 1),
            later.then(|| days_of(year, month + 1)),
            HistorySheet::Days { year, month, months: true },
            cx,
        );
        let lead = (0..words::weekday(first)).map(|_| div().into_any_element());
        let cells = lead.chain((1..=days as i32).map(|d| {
            let start = words::midnight(year, month, d);
            let day = words::iso_day(start);
            let count = marks.map(|m| m.get(&day).copied().unwrap_or(0));
            let (has, future) = (count.is_some_and(|n| n > 0), start > now);
            let (first, last) = self.wanted.days.unwrap_or((i64::MAX, i64::MIN));
            let picked = start == first || start == last;
            let inside = start > first && start < last;
            let number = div()
                .size(lines(1.6))
                .flex()
                .items_center()
                .justify_center()
                .when(day == today, |d| d.border_2().border_color(t.text))
                .when(has, |d| d.font_weight(BOLD))
                .when((future || count == Some(0)) && !picked, |d| d.text_color(t.alt))
                .when(picked, |d| d.bg(t.text).text_color(t.bg))
                .when(inside, |d| d.bg(t.bg_alt))
                .child(d.to_string());
            mark_cell(t, ("day", d as usize), number, has, future, TARGET, cx, move |this, cx| {
                this.pick_day(start, cx)
            })
        }));
        let letters =
            ["M", "T", "W", "T", "F", "S", "S"].map(|l| t.alt(l).text_center().into_any_element());
        let grid = grid(7, letters.into_iter().chain(cells));
        column().child(head).child(grid.h(picker_height()))
    }

    /// A year's months, those with entries marked and the rest dim; the
    /// title goes back to the month it came from.
    fn months(&self, t: Theme, year: i32, month: i32, cx: &mut Context<Self>) -> Div {
        let now = Time::now().unix();
        let (this_year, this_month, _) = words::ymd(now);
        let marks = self.marks.as_ref().and_then(|m| m.days.as_ref());
        let year_of = |year| HistorySheet::Days { year, month, months: true };
        let head = sheet_head(
            t,
            year.to_string(),
            year_of(year - 1),
            (year < this_year).then(|| year_of(year + 1)),
            HistorySheet::Days { year, month, months: false },
            cx,
        );
        let cells = (1..=12).map(|m| {
            let start = words::midnight(year, m, 1);
            let prefix = format!("{}-", &words::iso_day(start)[..7]);
            let has = marks.map(|d| d.iter().any(|(day, n)| *n > 0 && day.starts_with(&prefix)));
            let future = (year, m as u32) > (this_year, this_month);
            let label = div()
                .when(has == Some(true), |d| d.font_weight(BOLD))
                .when(future || has == Some(false), |d| d.text_color(t.alt))
                .child(words::month_short(start));
            mark_cell(
                t,
                ("month", m as usize),
                label,
                has == Some(true),
                future,
                picker_height() / 4.,
                cx,
                move |this, cx| {
                    this.history_sheet = Some(days_of(year, m));
                    cx.notify();
                },
            )
        });
        column().child(head).child(grid(3, cells).h(picker_height()))
    }

    /// The span of days the day picker shows, and asks marks for.
    fn marks_span(&self) -> Option<(String, String)> {
        let HistorySheet::Days { year, month, months } = self.history_sheet? else { return None };
        let (from, to) = match months {
            false => (words::midnight(year, month, 1), words::midnight(year, month + 1, 0)),
            true => (words::midnight(year, 1, 1), words::midnight(year, 13, 0)),
        };
        Some((words::iso_day(from), words::iso_day(to)))
    }

    /// Asks which days have entries, for the span the day picker shows and
    /// what the list is filtered by, unless asked already.
    pub(crate) fn load_marks(&mut self, cx: &mut Context<Self>) {
        let Some(span) = self.marks_span() else { return };
        let key = (span, self.wanted.kinds, self.wanted.q.clone());
        if self.marks.as_ref().is_some_and(|m| m.key == key) {
            return;
        }
        let ((from, to), kinds, q) = key.clone();
        let zone = segment(&words::zone());
        let mut path = format!("activity/days?from={from}&to={to}&tz={zone}");
        if let Some(kinds) = kinds {
            path += &format!("&kinds={kinds}");
        }
        if !q.is_empty() {
            path += &format!("&q={}", segment(&q));
        }
        self.marks = Some(Marks { key: key.clone(), days: None });
        let client = self.device().client;
        self.call(
            cx,
            move || Ok(client.get::<Days>(&path)),
            move |this, days, cx| {
                // A failure leaves the days unmarked, all plain.
                if let (Some(m), Ok(days)) = (&mut this.marks, days)
                    && m.key == key
                {
                    m.days = Some(days.days.into_iter().map(|d| (d.day, d.count)).collect());
                }
                cx.notify();
            },
        );
    }

    /// An entry in full: who, what, the agent's words, the chats and the
    /// terms, what was approved or sent, when, and on which device.
    pub(crate) fn entry_screen(&self, t: Theme, a: &Activity, cx: &mut Context<Self>) -> Div {
        let who = self.identities.iter().find(|i| i.id == a.identity).map(|i| i.name.clone());
        let chats: Vec<&Chat> = a.chats.iter().collect();
        let cols = words::chat_cols().saturating_sub(8).max(12);
        let key = format!("entry/{}/{}", a.at, a.kind);
        let mut chats = (!chats.is_empty())
            .then(|| self.chat_set(t, &a.connector, &chats, &key, cols, Whole::Beneath, None, cx));
        let terms: Vec<String> = a.terms.iter().map(|t| format!("\"{}\"", t.term)).collect();
        let when = match &a.until {
            Some(u) => words::clock_span(&a.at, u),
            None => words::clock(&a.at),
        };
        let mut rows: Vec<(String, String)> =
            vec![("When".into(), format!("{} {when}", words::date(&a.at)))];
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
        // A mark that the row shows: the set takes its place below.
        add("Chats", if chats.is_some() { "-".into() } else { String::new() });
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
        add("From", a.address.clone());
        add("Device", a.device.clone());
        let rows = rows
            .into_iter()
            .map(|(l, v)| {
                let value = match l.as_str() {
                    "Account" => connector_label(&a.connector, t.text),
                    "Chats" => chats.take().unwrap_or_else(div),
                    _ => div().child(v),
                };
                (l, value.into_any_element())
            })
            .collect();
        // A run of reads says what they did, quietly, under its line.
        let did = (a.merged() && !a.verbs.is_empty()).then(|| t.alt(verbs(&a.verbs)));
        column()
            .gap(lines(1.))
            .child(column().child(bold(self.entry(a))).children(did))
            .child(table(t, rows))
    }

    /// One plain line for an entry: who did what, and how it ended.
    fn entry(&self, a: &Activity) -> String {
        let who = self.identities.iter().find(|i| i.id == a.identity);
        Shell::line(a, who.map_or("an agent", |i| i.name.as_str()))
    }

    /// An entry's line, given its agent's name.
    fn line(a: &Activity, who: &str) -> String {
        let on = connector(&a.connector);
        // One chat by name, more by their number: the entry shows them all.
        let chats = match &a.chats[..] {
            [c] => words::chat(if c.name.is_empty() { &c.id } else { &c.name }),
            cs => format!("{} chats", cs.len()),
        };
        let n = |what: &str| {
            let n = a.count.unwrap_or(0);
            format!("{n} {what}{}", if n == 1 { "" } else { "s" })
        };
        let d = &a.detail;
        match a.kind.as_str() {
            // Reads in one run: what they reached, in sum.
            "read" if a.merged() => {
                let mut parts = vec![format!("{who} read {on}")];
                if !a.chats.is_empty() {
                    parts.push(chats);
                }
                if a.verbs.iter().any(|v| v == "messages") {
                    parts.push(n("message"));
                }
                if parts.len() == 1 {
                    parts.push(verbs(&a.verbs));
                }
                parts.join(" · ")
            }
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
            "agent.denied" => format!("you didn't let {d} in"),
            "device.denied" => format!("you didn't let {d} join"),
            "device.paired" => format!("{d} paired"),
            "device.joined" => format!("{d} joined"),
            "key.added" | "key.retired" => d.to_string(),
            "key.removed" => format!("you removed {d}"),
            "never_share.added" => format!("you hid {d}"),
            "never_share.removed" => format!("you shared {d} again"),
            "connector.unlinked" => format!("you unlinked {on}"),
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

    /// Takes the search text as typed, and applies it once typing has
    /// paused for a moment; the list shown meanwhile stays.
    pub(crate) fn searched(&mut self, text: String, cx: &mut Context<Self>) {
        if text == self.typed {
            return;
        }
        self.typed = text.clone();
        cx.spawn(async move |this, cx| {
            cx.background_executor().timer(SEARCH_PAUSE).await;
            this.update(cx, |this, cx| {
                if this.typed == text && this.wanted.q != text {
                    this.wanted.q = text;
                    cx.notify();
                }
            })
        })
        .detach();
    }

    /// Loads the newest page of what Activity asks for: ahead of what is
    /// loaded if it answers the same, else in its place.
    pub(crate) fn load_history(&mut self, cx: &mut Context<Self>) {
        self.page_history(None, cx);
    }

    /// Loads the page before the last loaded, while there is one.
    pub(crate) fn more_history(&mut self, cx: &mut Context<Self>) {
        if let Some(next) = self.history.as_ref().and_then(|h| h.next.clone()) {
            self.page_history(Some(next), cx);
        }
    }

    fn page_history(&mut self, cursor: Option<String>, cx: &mut Context<Self>) {
        if self.history_loading {
            return;
        }
        self.history_loading = true;
        let query = self.wanted.clone();
        let mut path = "activity?limit=100".to_string();
        // The days shown run from the first's midnight (after the second
        // before it, as after= is exclusive) to the midnight after the last.
        let end = query.days.map(|(_, last)| Time::from_unix(day_after(last)).to_string());
        if let Some(before) = cursor.clone().or(end) {
            path += &format!("&before={}", segment(&before));
        }
        if let Some((first, _)) = query.days {
            path += &format!("&after={}", segment(&Time::from_unix(first - 1).to_string()));
        }
        if let Some(kinds) = query.kinds {
            path += &format!("&kinds={kinds}");
        }
        if !query.q.is_empty() {
            path += &format!("&q={}", segment(&query.q));
        }
        let client = self.device().client;
        self.call(
            cx,
            move || Ok(client.get::<Value>(&path)),
            move |this, page, cx| {
                this.history_loading = false;
                // Asked for something else meanwhile; the next frame asks again.
                if this.wanted != query {
                    return cx.notify();
                }
                let page = match page {
                    Ok(page) => page,
                    // Out of reach, the list stays as it was, and the retry
                    // after a backoff loads it again.
                    Err(e @ mandate_core::Error::Io(_)) => return this.fail(e, cx),
                    // No retry on every frame: the next refresh tries the
                    // newest page again.
                    Err(e) => {
                        let h = this.history.get_or_insert_with(Loaded::default);
                        (h.query, h.next) = (query, None);
                        return this.error = Some(e.to_string());
                    }
                };
                // The newest page of everything shows at the next start
                // until the server answers.
                if cursor.is_none() && query == HistoryQuery::default() {
                    this.keep([("activity".to_string(), page.clone())]);
                }
                let page: History = serde_json::from_value(page).unwrap_or_default();
                let entries = page.entries.into_iter().map(Rc::new);
                match (&mut this.history, cursor) {
                    // Older entries go after what is loaded.
                    (Some(h), Some(_)) if h.query == query => {
                        h.entries.extend(entries);
                        h.next = page.next;
                    }
                    // Newer ones go before it, up to the first already there,
                    // which a run of reads may have grown: its newest copy stands.
                    (Some(h), None) if h.query == query && !h.entries.is_empty() => {
                        let first = (h.entries[0].at.clone(), h.entries[0].kind.clone());
                        let mut fresh = Vec::new();
                        for e in entries {
                            if (&e.at, &e.kind) == (&first.0, &first.1) {
                                h.entries[0] = e;
                                break;
                            }
                            fresh.push(e);
                        }
                        h.entries.splice(0..0, fresh);
                    }
                    // A new search, kind or day: the list starts over at its top.
                    _ => {
                        this.history =
                            Some(Loaded { entries: entries.collect(), next: page.next, query });
                        this.scroll.set_offset(gpui::point(gpui::px(0.), gpui::px(0.)));
                    }
                }
                cx.notify();
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
        let Some(key) = self.keys.devices.iter().find(|d| d.current).and_then(|d| d.keys.first())
        else {
            return cx.notify();
        };
        let (client, path) = (self.device().client, format!("keys/{}", segment(&key.id)));
        self.call(
            cx,
            move || client.send::<serde_json::Value>("PATCH", &path, &json!({ "name": name })),
            |this, _, cx| this.refresh(cx),
        );
    }

    /// This device and the others, what is never shared, and the server.
    pub(crate) fn settings(&self, t: Theme, cx: &mut Context<Self>) -> Div {
        let removable = self.keys.devices.len() > 1;
        let devices = self.keys.devices.iter().enumerate().map(|(i, d)| {
            let keys: Vec<String> = d.keys.iter().map(|k| k.id.clone()).collect();
            let (name, name2) = (d.name.clone(), d.name.clone());
            let this_one = d.current;
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
            let methods: Vec<&str> =
                d.keys.iter().map(|k| k.method.as_str()).filter(|m| !m.is_empty()).collect();
            row(t, i)
                .child(
                    column()
                        .flex_1()
                        .child(div().child(name.clone()).when(this_one, |d| {
                            d.child(div().text_color(t.info).child("this device"))
                        }))
                        .when(!methods.is_empty(), |c| c.child(t.alt(methods.join(", ")))),
                )
                .when(this_one, |d| {
                    d.child(link(t, "rename", "Rename", cx, move |this, cx| {
                        this.renaming = Some(name.clone());
                        crate::keyboard(true);
                        cx.notify();
                    }))
                })
                .when(removable, |row| {
                    row.child(link(t, id_for("remove", &d.id), "Remove", cx, move |this, cx| {
                        let (keys, name) = (keys.clone(), name2.clone());
                        this.ask(Confirm::RemoveDevice { keys, name }, cx)
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
                .child(
                    column().flex_1().child(name.clone()).child(
                        div()
                            .flex()
                            .items_center()
                            .gap(chars(1.))
                            .text_color(t.alt)
                            .child(format!("{} on", h.kind))
                            .child(connector_label(&h.connector, t.alt)),
                    ),
                )
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
        // Each way this device approves, the one that signs ticked: another
        // takes over once the ticked one approves, and one not ticked can go.
        let mine = self.device.as_ref().map(|d| (d.keys.clone(), crate::device::id(&d.key)));
        let (mine, signing) = mine.unwrap_or_default();
        let several = mine.len() > 1;
        let methods = mine.iter().enumerate().map(|(i, k)| {
            let id = crate::device::id(k);
            let (on, title) = (id == signing, k.title());
            let (use_id, drop_id, drop_title) = (id.clone(), id.clone(), title.clone());
            row(t, i)
                .child(
                    clickable(id_for("method", &id), cx, move |this, cx| {
                        if !on {
                            this.use_method(use_id.clone(), cx)
                        }
                    })
                    .flex_1()
                    .flex()
                    .items_center()
                    .gap(chars(1.))
                    .child(crate::now::checkbox(t, on))
                    .child(title),
                )
                .when(several && !on, |row| {
                    row.child(link(t, id_for("drop", &id), "Remove", cx, move |this, cx| {
                        let (id, title) = (drop_id.clone(), drop_title.clone());
                        this.ask(Confirm::DropMethod { id, title }, cx)
                    }))
                })
        });
        let methods: Vec<Div> = methods.collect();
        let add_method = (!self.methods_left().is_empty()).then(|| {
            div().flex().child(button(t, "add-method", "Add a method", cx, |this, cx| {
                this.new_key(Next::Add, cx)
            }))
        });
        // Android may end the stream in the background, and then nothing
        // that waits reaches the owner: say so while it may.
        #[cfg(target_os = "android")]
        let background =
            (!crate::android::KEPT.load(std::sync::atomic::Ordering::Relaxed)).then(|| {
                column()
                    .gap(lines(0.5))
                    .child(div().text_color(t.danger).child("Requests may not reach you."))
                    .child(div().flex().child(button(t, "keep", "Keep running", cx, |_, _| {
                        crate::android::keep_running()
                    })))
            });
        #[cfg(not(target_os = "android"))]
        let background: Option<Div> = None;
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
            .child(column().border_2().border_color(t.text).children(methods))
            .children(add_method)
            .children(background)
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

    /// An agent's grant: its account, its chats as a set, when it is open
    /// and how far back it reads.
    fn grant_line(&self, t: Theme, g: &Grant, cx: &mut Context<Self>) -> Div {
        // No brackets: a bracket's glyph sits right of its cell, off the
        // text's edge.
        let state = match (&g.until, &g.schedule) {
            (_, Some(s)) => {
                format!("{} at {}", words::days(&s.days), words::time_of_day(&s.start))
            }
            (Some(u), None) => format!("until {}", words::clock(u)),
            _ => String::new(),
        };
        column()
            .child(connector_label(&g.connector, t.text))
            .child(self.grant_chats(t, g, words::chat_cols(), cx))
            .child(
                div()
                    .text_color(if g.schedule.is_some() { t.routine } else { t.open })
                    .child(state),
            )
            .child(t.alt(horizon(g.horizon)))
    }
}

/// A run of reads' verbs, as one quiet line: "listed chats · read messages".
fn verbs(vs: &[String]) -> String {
    let said: Vec<String> = vs
        .iter()
        .map(|v| match v.as_str() {
            "chats" => "listed chats".to_string(),
            "messages" => "read messages".to_string(),
            "search" => "searched".to_string(),
            "media" => "opened files".to_string(),
            op => format!("used {op}"),
        })
        .collect();
    said.join(" · ")
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

/// A day's header in Activity, in the list and stuck on top.
pub(crate) fn day_heading(day: &str) -> Div {
    bold(day.to_uppercase()).py(lines(0.5))
}

/// What Activity asks the server for.
#[derive(Clone, Debug, Default, PartialEq)]
pub(crate) struct HistoryQuery {
    /// Search text, "" for everything.
    pub q: String,
    /// A named group of kinds, None for all.
    pub kinds: Option<&'static str>,
    /// The days shown, by the starts of the first and the last; None for
    /// the latest.
    pub days: Option<(i64, i64)>,
}

/// History loaded so far for a query, newest first.
#[derive(Default)]
pub(crate) struct Loaded {
    pub entries: Vec<Rc<Activity>>,
    pub next: Option<String>,
    pub query: HistoryQuery,
}

impl Loaded {
    /// The newest page of everything, as kept from before.
    pub fn from_page(page: Value) -> Loaded {
        let page: History = serde_json::from_value(page).unwrap_or_default();
        let entries = page.entries.into_iter().map(Rc::new).collect();
        Loaded { entries, next: page.next, query: HistoryQuery::default() }
    }
}

/// Activity's sheets: the kinds, or the days of a month (or the months of
/// its year).
#[derive(Clone, Copy, PartialEq)]
pub(crate) enum HistorySheet {
    Kinds,
    Days { year: i32, month: i32, months: bool },
}

/// The days with entries in a span (first and last day), for the kinds and
/// search they were counted for; None while they load or if they failed.
pub(crate) struct Marks {
    key: MarksKey,
    days: Option<HashMap<String, u32>>,
}

type MarksKey = ((String, String), Option<&'static str>, String);

/// How long a change may load before it shows as loading, and how long the
/// new rows take to fade in.
const SLOW: Duration = Duration::from_millis(300);
const FADE: Duration = Duration::from_millis(150);

/// The start of the day after the one starting at day.
fn day_after(day: i64) -> i64 {
    let (y, m, d) = words::ymd(day);
    words::midnight(y, m as i32, d as i32 + 1)
}

/// "3 Nov" for a day, "1–3 Nov" or "28 Oct – 3 Nov" for a range.
fn range_label((first, last): (i64, i64)) -> String {
    let ((y1, m1, d1), (y2, m2, _)) = (words::ymd(first), words::ymd(last));
    match (first == last, (y1, m1) == (y2, m2)) {
        (true, _) => words::day(first),
        (false, true) => format!("{d1}–{}", words::day(last)),
        (false, false) => format!("{} – {}", words::day(first), words::day(last)),
    }
}

/// The kinds Activity can show, and the group of kinds each asks for.
const KINDS: [(&str, Option<&str>); 5] = [
    ("All activity", None),
    ("Reads", Some("reads")),
    ("Sends", Some("sends")),
    ("Requests", Some("requests")),
    ("Logins & devices", Some("access")),
];

/// A tap target's height, as Material asks: 48 dp.
const TARGET: gpui::Pixels = gpui::px(48.);

/// A month's days in the day picker, the month rolled into its year.
fn days_of(year: i32, month: i32) -> HistorySheet {
    let (year, month, _) = words::ymd(words::midnight(year, month, 1));
    HistorySheet::Days { year, month: month as i32, months: false }
}

/// A day or month of the day picker: its label over a dot when it has
/// entries; it picks unless it is still to come.
#[allow(clippy::too_many_arguments)]
fn mark_cell(
    t: Theme,
    id: (&'static str, usize),
    label: Div,
    has: bool,
    future: bool,
    height: Pixels,
    cx: &mut Context<Shell>,
    pick: impl Fn(&mut Shell, &mut Context<Shell>) + 'static,
) -> AnyElement {
    let dot = div().size(px(4.)).when(has, |d| d.bg(t.grant_hi));
    let cell = div().h(height).flex().flex_col().items_center().justify_center().gap(px(2.));
    let cell = cell.child(label).child(dot);
    // Days and months still to come are fainter than empty ones past.
    if future {
        return cell.opacity(0.3).into_any_element();
    }
    clickable(id, cx, pick).child(cell).into_any_element()
}

/// The day picker's height below its head, the same for a month's days
/// (weekday letters over up to six weeks) and a year's months.
fn picker_height() -> Pixels {
    lines(1.) + TARGET * 6.
}

/// The day picker's head: back, the title that opens the wider view, and
/// on, where there is a later span to go to.
fn sheet_head(
    t: Theme,
    title: String,
    back: HistorySheet,
    on: Option<HistorySheet>,
    wider: HistorySheet,
    cx: &mut Context<Shell>,
) -> Div {
    let go = |sheet: HistorySheet| {
        move |this: &mut Shell, cx: &mut Context<Shell>| {
            this.history_sheet = Some(sheet);
            cx.notify();
        }
    };
    let on = match on {
        Some(on) => icon_button(t, "sheet-on", "chevron-right", cx, go(on)).into_any_element(),
        None => div().size(lines(2.5)).into_any_element(),
    };
    div()
        .flex()
        .items_center()
        .justify_between()
        .child(icon_button(t, "sheet-back", "chevron-left", cx, go(back)))
        .child(
            clickable("sheet-title", cx, go(wider))
                .h(TARGET)
                .flex()
                .items_center()
                .gap(chars(0.5))
                .px(chars(1.))
                .font_weight(MEDIUM)
                .child(title)
                .child(icon("chevron-down", t.text)),
        )
        .child(on)
}

/// Cells in rows of n, each an equal share of the width.
fn grid(n: usize, cells: impl Iterator<Item = AnyElement>) -> Div {
    let mut rows = Vec::new();
    let mut cells = cells.peekable();
    while cells.peek().is_some() {
        let mut row = div().flex();
        for _ in 0..n {
            row = row.child(div().flex_1().children(cells.next()));
        }
        rows.push(row);
    }
    column().children(rows)
}

/// How long typing pauses before a search goes to the server.
const SEARCH_PAUSE: Duration = Duration::from_millis(250);
