//! What each screen shows. Records are shown from their bytes, as decoded
//! from the envelope the owner signs.

use crate::device::{Request, hex};
use crate::{Screen, Shell};
use gpui::{
    AnyElement, ClickEvent, Context, Div, ElementId, Hsla, SharedString, Window, div, prelude::*,
    px, rgb,
};
use mandate_core::{Grant, PairLink, Record};

const DAYS: [&str; 7] = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];

impl Render for Shell {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let screen = match &self.screen {
            Screen::Pair { text } => pair(text, cx),
            Screen::Now => self.waiting(cx),
            Screen::Code { exchange, code } => {
                let checking = code.len() == 6 && self.error.is_none();
                with_back(cx)
                    .child(title(format!("Code from {}", exchange.name)))
                    .child(muted(format!("Type the 6 digits {} shows.", exchange.name)))
                    .child(digits(code))
                    .child(if checking {
                        muted("Checking…").into_any_element()
                    } else {
                        keypad(cx)
                    })
            }
            Screen::Admit { exchange, envelope } => {
                let what = if exchange.kind == "device" { "as a device" } else { "as an agent" };
                let envelope = envelope.clone();
                with_back(cx)
                    .child(title(format!("Let {} in?", exchange.name)))
                    .child(muted(what))
                    .child(button("admit", "Let in", cx, move |this, cx| {
                        this.approve(envelope.clone(), cx)
                    }))
            }
            Screen::Request { request, envelope } => {
                let body = match envelope.record() {
                    Ok(Record::Grant(g)) => grant(&g, request),
                    _ => muted("This is not a grant record.").into_any_element(),
                };
                let (envelope, deny) = (envelope.clone(), format!("requests/{}/deny", request.id));
                with_back(cx)
                    .child(title(format!(
                        "{} wants to read {}",
                        request.identity_name, request.connector
                    )))
                    .when(!request.reason.is_empty(), |d| d.child(muted(request.reason.clone())))
                    .child(body)
                    .child(decide(envelope, deny, cx))
            }
            Screen::Proposal { proposal, envelope } => {
                let body = match envelope.record() {
                    Ok(Record::Write(w)) => div()
                        .flex()
                        .flex_col()
                        .gap_2()
                        .children(w.display.into_iter().map(|f| field(f.label, f.value)))
                        .when(!w.note.is_empty(), |d| d.child(muted(w.note)))
                        .into_any_element(),
                    _ => muted("This is not a write record.").into_any_element(),
                };
                let (envelope, deny) =
                    (envelope.clone(), format!("proposals/{}/deny", proposal.id));
                with_back(cx)
                    .child(title(format!(
                        "{} wants to {} on {}",
                        proposal.identity_name, proposal.op, proposal.connector
                    )))
                    .child(body)
                    .child(decide(envelope, deny, cx))
            }
        };
        div()
            .id("shell")
            .size_full()
            .overflow_y_scroll()
            .track_focus(&self.focus)
            .on_key_down(cx.listener(Self::key))
            .p_6()
            .bg(rgb(0xffffff))
            .text_color(rgb(0x1f1f1f))
            .child(screen.gap_3())
            .when_some(self.error.clone(), |d, e| {
                d.child(div().pt_4().text_color(rgb(0xa83232)).child(e))
            })
    }
}

impl Shell {
    fn waiting(&self, cx: &mut Context<Self>) -> Div {
        let mut rows: Vec<AnyElement> = Vec::new();
        for (i, ex) in self.exchanges.iter().enumerate() {
            let what = if ex.kind == "device" { "join as a device" } else { "log in" };
            let ex = ex.clone();
            rows.push(row(
                ("exchange", i),
                format!("{} wants to {what}", ex.name),
                cx,
                move |this, cx| {
                    this.error = None;
                    this.screen = Screen::Code { exchange: ex.clone(), code: String::new() };
                    cx.notify();
                },
            ));
        }
        for (i, r) in self.now.requests.iter().enumerate() {
            let r = r.clone();
            let text = format!("{} wants to read {}", r.identity_name, r.connector);
            rows.push(row(("request", i), text, cx, move |this, cx| {
                this.open_request(r.clone(), cx)
            }));
        }
        for (i, p) in self.now.proposals.iter().enumerate() {
            let p = p.clone();
            let text = format!("{} wants to {} on {}", p.identity_name, p.op, p.connector);
            rows.push(row(("proposal", i), text, cx, move |this, cx| {
                this.open_proposal(p.clone(), cx)
            }));
        }
        let open = self.now.grants.iter().map(|g| {
            let until =
                g.until.as_deref().and_then(|u| u.get(11..16)).map(|t| format!(", until {t} UTC"));
            div().child(format!(
                "{} reads {}{}",
                g.identity_name,
                g.connector,
                until.unwrap_or_default()
            ))
        });
        column()
            .child(title("Now"))
            .when(rows.is_empty(), |d| d.child(muted("Nothing waits.")))
            .children(rows)
            .when(!self.now.grants.is_empty(), |d| {
                d.child(div().pt_4().child(muted("Open"))).children(open)
            })
            .child(div().pt_6().child(link("stop", "Stop all access", cx, |this, cx| {
                this.post("stop-all".into(), cx)
            })))
    }
}

fn pair(text: &str, cx: &mut Context<Shell>) -> Div {
    let link: Option<PairLink> = text.parse().ok();
    let shown = if text.is_empty() {
        muted("mandate://pair?…").into_any_element()
    } else {
        text.to_string().into_any_element()
    };
    column()
        .child(title("Pair"))
        .child(muted("Paste the pairing link from the setup page."))
        .child(
            div().p_2().border_1().border_color(line()).rounded_md().overflow_hidden().child(shown),
        )
        .when_some(link, |d, link| {
            let key = hex(&link.spki_sha256);
            let groups: Vec<&str> = (0..key.len()).step_by(4).map(|i| &key[i..i + 4]).collect();
            d.child(muted("Server key"))
                .child(groups.join(" "))
                .child(muted("Pair only if this matches what mandate setup printed."))
                .child(button("pair", "Pair", cx, move |this, cx| this.pair(link.clone(), cx)))
        })
}

fn grant(g: &Grant, request: &Request) -> AnyElement {
    let chats = if g.chats.is_empty() {
        "every chat".to_string()
    } else {
        let named = g.chats.iter().map(|id| match request.chat_name(id) {
            Some(name) => format!("{name} ({id})"),
            None => id.clone(),
        });
        named.collect::<Vec<_>>().join(", ")
    };
    let window = match (&g.schedule, g.from, g.until) {
        (Some(s), _, _) => {
            let days = if s.days.is_empty() {
                "every day".to_string()
            } else {
                s.days.iter().map(|&d| DAYS[usize::from(d) % 7]).collect::<Vec<_>>().join(" ")
            };
            format!("{days}, {} for {} ({})", s.start, span(u64::from(s.length)), s.zone)
        }
        (None, Some(from), Some(until)) => {
            let span = span(until.unix().saturating_sub(from.unix()).max(0) as u64);
            format!("{span}, until {} UTC", &until.to_string()[11..16])
        }
        _ => "none".into(),
    };
    let horizon =
        if g.horizon == 0 { "all history".into() } else { format!("{} back", span(g.horizon)) };
    div()
        .flex()
        .flex_col()
        .gap_2()
        .child(field("Connector", g.connector.clone()))
        .child(field("Chats", chats))
        .child(field("Window", window))
        .child(field("History", horizon))
        .into_any_element()
}

fn span(secs: u64) -> String {
    match secs {
        s if s >= 86400 && s % 86400 == 0 => format!("{} days", s / 86400),
        s if s >= 3600 && s % 3600 == 0 => format!("{} h", s / 3600),
        s => format!("{} min", s.div_ceil(60)),
    }
}

fn decide(envelope: mandate_core::Envelope, deny: String, cx: &mut Context<Shell>) -> Div {
    div()
        .flex()
        .gap_2()
        .pt_2()
        .child(button("approve", "Approve", cx, move |this, cx| this.approve(envelope.clone(), cx)))
        .child(button("deny", "Deny", cx, move |this, cx| this.post(deny.clone(), cx)))
}

fn digits(code: &str) -> Div {
    let cell = |c: Option<char>| {
        div()
            .w(px(36.))
            .h(px(44.))
            .flex()
            .items_center()
            .justify_center()
            .border_b_1()
            .border_color(line())
            .text_xl()
            .child(c.map(String::from).unwrap_or_default())
    };
    let mut chars = code.chars();
    div().flex().gap_2().children((0..6).map(|_| cell(chars.next())))
}

fn keypad(cx: &mut Context<Shell>) -> AnyElement {
    let rows = [["1", "2", "3"], ["4", "5", "6"], ["7", "8", "9"], ["", "0", "Del"]];
    let key = |label: &'static str, cx: &mut Context<Shell>| -> AnyElement {
        if label.is_empty() {
            return div().w(px(72.)).into_any_element();
        }
        button(label, label, cx, move |this, cx| match label {
            "Del" => {
                if let Screen::Code { code, .. } = &mut this.screen {
                    code.pop();
                }
                cx.notify();
            }
            d => this.digit(d.chars().next().unwrap_or('0'), cx),
        })
        .w(px(72.))
        .into_any_element()
    };
    let rows: Vec<Div> =
        rows.iter().map(|r| div().flex().gap_2().children(r.map(|l| key(l, cx)))).collect();
    div().flex().flex_col().gap_2().pt_2().children(rows).into_any_element()
}

fn column() -> Div {
    div().flex().flex_col()
}

fn with_back(cx: &mut Context<Shell>) -> Div {
    column().child(link("back", "Back", cx, |this, cx| {
        this.back();
        cx.notify();
    }))
}

fn title(text: impl Into<SharedString>) -> Div {
    div().text_xl().pb_2().child(text.into())
}

fn muted(text: impl Into<SharedString>) -> Div {
    div().text_color(rgb(0x707070)).child(text.into())
}

fn field(label: impl Into<SharedString>, value: impl Into<SharedString>) -> Div {
    div().flex().flex_col().child(muted(label).text_sm()).child(value.into())
}

fn line() -> Hsla {
    rgb(0xd8d8d8).into()
}

fn clickable(
    id: impl Into<ElementId>,
    cx: &mut Context<Shell>,
    f: impl Fn(&mut Shell, &mut Context<Shell>) + 'static,
) -> gpui::Stateful<Div> {
    div()
        .id(id.into())
        .cursor_pointer()
        .on_click(cx.listener(move |this, _: &ClickEvent, _, cx| f(this, cx)))
}

fn button(
    id: impl Into<ElementId>,
    label: &'static str,
    cx: &mut Context<Shell>,
    f: impl Fn(&mut Shell, &mut Context<Shell>) + 'static,
) -> gpui::Stateful<Div> {
    clickable(id, cx, f)
        .flex()
        .justify_center()
        .px_3()
        .py_2()
        .border_1()
        .border_color(line())
        .rounded_md()
        .child(label)
}

fn link(
    id: &'static str,
    label: &'static str,
    cx: &mut Context<Shell>,
    f: impl Fn(&mut Shell, &mut Context<Shell>) + 'static,
) -> gpui::Stateful<Div> {
    clickable(id, cx, f).text_color(rgb(0x707070)).underline().child(label)
}

fn row(
    id: (&'static str, usize),
    text: String,
    cx: &mut Context<Shell>,
    f: impl Fn(&mut Shell, &mut Context<Shell>) + 'static,
) -> AnyElement {
    clickable(id, cx, f).py_3().border_b_1().border_color(line()).child(text).into_any_element()
}
