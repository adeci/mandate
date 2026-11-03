//! The frame every screen shares, the screens outside the tabs, and the
//! few widgets they are built from.

use crate::api::Exchange;
use crate::device::hex;
use crate::theme::{BOLD, FONT, MEDIUM, NORMAL, SIZE, Theme, bold, chars, lines};
use crate::{Screen, Shell, Tab, words};
use gpui::{
    AnyElement, ClickEvent, Context, Div, ElementId, Hsla, SharedString, Stateful, Svg, Window,
    div, prelude::*, px, svg,
};
use mandate_core::{Envelope, PairLink, Record};

impl Render for Shell {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let t = Theme::of(window);
        let pad = if window.viewport_size().width < px(480.) { 1. } else { 2. };
        let tab = match self.screen {
            Screen::Tab(tab) => Some(tab),
            _ => None,
        };
        let title = match &self.screen {
            Screen::Pair { .. } => "Pair",
            Screen::Tab(Tab::Now) => "Now",
            Screen::Tab(Tab::Agents) => "Agents",
            Screen::Tab(Tab::Connections) => "Connections",
            Screen::Tab(Tab::Activity) => "Activity",
            Screen::Settings => "Settings",
            Screen::Code { .. } => "Code",
            Screen::Admit { .. } => "Let in",
            Screen::Review { .. } => "Review",
            Screen::Link { .. } => "Link",
            Screen::Hide { .. } => "Never share",
            Screen::Grant(_) => "New grant",
        };
        let screen = match &self.screen {
            Screen::Pair { text } => pair(t, text, cx),
            Screen::Tab(Tab::Now) => self.now_tab(t, cx),
            Screen::Tab(Tab::Agents) => self.agents_tab(t, cx),
            Screen::Tab(Tab::Connections) => self.connections_tab(t, cx),
            Screen::Tab(Tab::Activity) => self.activity_tab(t, cx),
            Screen::Link { .. } => self.link_screen(t, cx),
            Screen::Hide { .. } => self.hide_screen(t, cx),
            Screen::Grant(_) => self.grant_screen(t, cx),
            Screen::Settings => self.settings(t, cx),
            Screen::Code { exchange, code } => {
                code_entry(t, exchange, code, self.error.is_none(), cx)
            }
            Screen::Admit { exchange, envelope } => admit(t, exchange, envelope, cx),
            Screen::Review { proposal, envelope } => {
                let fields: Vec<(String, AnyElement)> = match envelope.record() {
                    Ok(Record::Write(w)) => w
                        .display
                        .into_iter()
                        .map(|f| (f.label, div().child(f.value).into_any_element()))
                        .collect(),
                    _ => vec![(
                        "Error".into(),
                        div().child("This is not a write record.").into_any_element(),
                    )],
                };
                let (envelope, deny) =
                    (envelope.clone(), format!("proposals/{}/deny", proposal.id));
                column()
                    .child(column().child(who(&proposal.identity_name, "wants to send")).child(
                        div().font_weight(MEDIUM).child(words::connector(&proposal.connector)),
                    ))
                    .child(table(t, fields))
                    .children(reason(t, &proposal.note))
                    .child(approve(
                        t,
                        "approve",
                        "Approve and send".into(),
                        true,
                        cx,
                        move |this, cx| this.approve(envelope.clone(), cx),
                    ))
                    .child(button(t, "deny", "Deny", cx, move |this, cx| {
                        this.post("POST", deny.clone(), cx, |_| Some("You denied the send.".into()))
                    }))
            }
        };
        #[cfg(target_os = "android")]
        let ((top, bottom), typing) = (crate::android::insets(), crate::android::keyboard_up());
        #[cfg(not(target_os = "android"))]
        let ((top, bottom), typing) = ((0., 0.), false);
        div()
            .size_full()
            .flex()
            .flex_col()
            .track_focus(&self.focus)
            .on_key_down(cx.listener(Self::key))
            .font_family(FONT)
            .font_weight(NORMAL)
            .text_size(px(SIZE))
            .line_height(lines(1.))
            .bg(t.bg)
            .text_color(t.text)
            .pt(px(top))
            .pb(px(bottom))
            .child(self.top_bar(t, title, tab.is_some(), pad, cx))
            .child(
                div().id("page").flex_1().min_h_0().overflow_y_scroll().child(
                    page(pad)
                        .pt(lines(1.))
                        .pb(lines(2.))
                        .gap(lines(1.))
                        .when_some(self.error.clone(), |d, e| d.child(flash(t, e)))
                        .when_some(self.notice.clone(), |d, n| d.child(flash(t, n)))
                        .child(screen.gap(lines(1.))),
                ),
            )
            // The soft keyboard sits where the tab bar would; it returns as the keyboard goes.
            .when_some(tab.filter(|_| !typing), |d, tab| d.child(self.tab_bar(t, tab, cx)))
    }
}

/// The page's column: the screen's width on phones, a readable measure,
/// centered, on wider windows.
fn page(pad: f32) -> Div {
    column().w_full().max_w(chars(80.)).mx_auto().px(chars(pad))
}

pub fn icon(name: &str, color: Hsla) -> Svg {
    svg().path(format!("icons/{name}.svg")).size(px(20.)).flex_none().text_color(color)
}

fn icon_button(
    t: Theme,
    id: &'static str,
    name: &str,
    cx: &mut Context<Shell>,
    f: impl Fn(&mut Shell, &mut Context<Shell>) + 'static,
) -> Stateful<Div> {
    clickable(id, cx, f)
        .size(lines(2.5))
        .flex()
        .items_center()
        .justify_center()
        .active(|s| s.bg(t.bg_alt))
        .child(icon(name, t.text))
}

impl Shell {
    /// The page's name, a way back off the tabs, and search and settings on
    /// them.
    fn top_bar(&self, t: Theme, title: &str, tabs: bool, pad: f32, cx: &mut Context<Self>) -> Div {
        let paired = self.device.is_some();
        let row = page(pad)
            .flex_row()
            .items_center()
            .h(lines(3.))
            .gap(chars(1.))
            .when(!tabs && paired, |d| {
                d.child(icon_button(t, "back", "arrow-left", cx, |this, cx| {
                    let back = match this.screen {
                        Screen::Link { .. } => Screen::Tab(Tab::Connections),
                        Screen::Hide { .. } => Screen::Settings,
                        Screen::Grant(_) => Screen::Tab(Tab::Agents),
                        _ => Screen::Tab(Tab::Now),
                    };
                    this.go(back, cx)
                }))
            })
            .child(
                div()
                    .flex_1()
                    .font_weight(BOLD)
                    .text_size(px(SIZE * 1.25))
                    .child(title.to_uppercase()),
            )
            .when(tabs, |d| {
                d.child(icon_button(t, "search", "search", cx, |this, cx| {
                    this.search = if this.search.is_some() { None } else { Some(String::new()) };
                    crate::keyboard(this.search.is_some());
                    cx.notify();
                }))
                .child(icon_button(t, "settings", "settings", cx, |this, cx| {
                    this.go(Screen::Settings, cx)
                }))
            });
        let search = self.search.as_ref().filter(|_| tabs).map(|q| {
            page(pad)
                .flex_row()
                .items_center()
                .gap(chars(1.))
                .h(lines(2.5))
                .child(icon("search", t.alt))
                .child(if q.is_empty() {
                    t.alt("Search this page").flex_1()
                } else {
                    div().flex_1().child(q.clone())
                })
                .child(icon_button(t, "clear", "x", cx, |this, cx| {
                    this.search = None;
                    crate::keyboard(false);
                    cx.notify();
                }))
        });
        column()
            .border_b_2()
            .border_color(t.text)
            .child(row)
            .children(search.map(|s| div().border_t_2().border_color(t.alt).child(s)))
    }

    /// Thumb-reach tabs: icon over label, the current one in full weight.
    fn tab_bar(&self, t: Theme, current: Tab, cx: &mut Context<Self>) -> Div {
        let waiting = self.now.requests.len() + self.now.proposals.len() + self.exchanges.len();
        let attention = self.connectors.iter().filter(|c| c.health.state != "ok").count();
        let items = [
            (Tab::Now, "bell", "Now", waiting),
            (Tab::Agents, "bot", "Agents", 0),
            (Tab::Connections, "plug", "Connections", attention),
            (Tab::Activity, "activity", "Activity", 0),
        ];
        let cells = items.into_iter().enumerate().map(|(i, (tab, name, label, count))| {
            let on = tab == current;
            let color = if on { t.text } else { t.alt };
            clickable(("tab", i), cx, move |this, cx| this.go(Screen::Tab(tab), cx))
                .flex_1()
                .flex()
                .flex_col()
                .items_center()
                .py(lines(0.5))
                .text_color(color)
                .when(on, |d| d.font_weight(BOLD))
                .child(div().flex().items_center().gap(chars(0.5)).child(icon(name, color)).when(
                    count > 0,
                    |d| {
                        d.child(
                            div()
                                .px(chars(0.5))
                                .bg(t.info)
                                .text_color(t.bg)
                                .child(count.to_string()),
                        )
                    },
                ))
                .child(div().text_size(px(SIZE * 0.85)).child(label))
        });
        div().flex().border_t_2().border_color(t.text).children(cells)
    }
}

fn pair(t: Theme, text: &str, cx: &mut Context<Shell>) -> Div {
    let link: Option<PairLink> = text.parse().ok();
    let have = link.is_some();
    let shown = if text.is_empty() { "mandate://pair?…" } else { text };
    column()
        .child("Scan the QR code on the setup page with your phone's camera.")
        .when_some(link, |d, link| {
            let key = hex(&link.spki_sha256);
            let groups: Vec<&str> = (0..key.len()).step_by(4).map(|i| &key[i..i + 4]).collect();
            d.child(table(
                t,
                vec![
                    ("Server".into(), div().child(link.url.clone()).into_any_element()),
                    ("Key".into(), div().child(groups.join(" ")).into_any_element()),
                ],
            ))
            .child("Pair only if this key matches what mandate setup printed.")
            .child(button(t, "pair", "Pair", cx, move |this, cx| this.pair(link.clone(), cx)))
        })
        .when(!have, |d| {
            d.child(t.alt("Or paste the link here:").mt(lines(1.))).child(
                div()
                    .border_2()
                    .border_color(t.text)
                    .px(chars(1.))
                    .py(lines(0.5))
                    .when(text.is_empty(), |d| d.text_color(t.alt))
                    .child(shown.to_string()),
            )
        })
}

fn code_entry(t: Theme, ex: &Exchange, code: &str, quiet: bool, cx: &mut Context<Shell>) -> Div {
    let mut typed = code.chars();
    let cells = (0..6).map(|_| {
        div()
            .w(chars(4.))
            .h(lines(3.))
            .flex()
            .items_center()
            .justify_center()
            .border_2()
            .border_color(t.text)
            .text_size(px(SIZE * 1.5))
            .font_weight(BOLD)
            .child(typed.next().map(String::from).unwrap_or_default())
    });
    let keys = [["1", "2", "3"], ["4", "5", "6"], ["7", "8", "9"], ["", "0", "Del"]];
    let rows = keys.map(|row| {
        div().flex().gap(chars(1.)).children(row.map(|label| {
            if label.is_empty() {
                return div()
                    .flex_1()
                    .h(lines(2.))
                    .px(chars(1.))
                    .border_2()
                    .border_color(t.bg)
                    .into_any_element();
            }
            button(t, label, label, cx, move |this, cx| match label {
                "Del" => {
                    if let Screen::Code { code, .. } = &mut this.screen {
                        code.pop();
                    }
                    cx.notify();
                }
                d => this.digit(d.chars().next().unwrap_or('0'), cx),
            })
            .flex_1()
            .into_any_element()
        }))
    });
    let what = if ex.kind == "device" { "wants to join as a device" } else { "wants to log in" };
    column()
        .child(who(&ex.name, what))
        .child(format!("Type the 6 digits {} shows.", ex.name))
        .child(div().flex().gap(chars(1.)).children(cells))
        .child(if code.len() == 6 && quiet {
            t.alt("Checking the code…").into_any_element()
        } else {
            column().gap(lines(0.5)).children(rows).into_any_element()
        })
}

fn admit(t: Theme, ex: &Exchange, envelope: &Envelope, cx: &mut Context<Shell>) -> Div {
    let (kind, after) = if ex.kind == "device" {
        ("device", format!("Once in, {} can approve like this phone.", ex.name))
    } else {
        (
            "agent",
            format!(
                "Once in, {} can ask to read your chats. It reads nothing until you approve.",
                ex.name
            ),
        )
    };
    let envelope = envelope.clone();
    let mut rows = vec![("Kind".to_string(), div().child(kind).into_any_element())];
    if !ex.address.is_empty() {
        rows.push(("From".into(), div().child(ex.address.clone()).into_any_element()));
    }
    column()
        .child(bold(format!("Let {} in?", ex.name)))
        .child(format!("The code matches. {after}"))
        .child(table(t, rows))
        .child(approve(
            t,
            "admit",
            format!("Let {} in", ex.name).into(),
            true,
            cx,
            move |this, cx| this.approve(envelope.clone(), cx),
        ))
        .child(button(t, "later", "Not now", cx, |this, cx| this.go(Screen::Tab(Tab::Now), cx)))
}

pub fn column() -> Div {
    div().flex().flex_col()
}

/// "laptop wants to read": the name stands out.
pub fn who(name: &str, rest: &str) -> Div {
    div().flex().flex_wrap().gap_x(chars(1.)).child(bold(name.to_string())).child(rest.to_string())
}

/// The agent's own words, set apart.
pub fn reason(t: Theme, text: &str) -> Option<Div> {
    (!text.is_empty()).then(|| {
        column()
            .border_l_2()
            .border_color(t.alt)
            .pl(chars(1.))
            .child(t.alt("agent's reason"))
            .child(text.to_string())
    })
}

pub fn flash(t: Theme, text: String) -> Div {
    div().border_2().border_color(t.info).px(chars(1.)).py(lines(0.5)).child(text)
}

/// A panel like the site's details box.
pub fn panel(t: Theme) -> Div {
    column().gap(lines(1.)).border_2().border_color(t.text).px(chars(1.)).py(lines(1.))
}

/// A fully ruled two-column table, like the site's.
pub fn table(t: Theme, rows: Vec<(String, AnyElement)>) -> Div {
    // The longest label, its padding and the rule, so no label wraps.
    let width = rows.iter().map(|(l, _)| l.chars().count()).max().unwrap_or(0) as f32 + 2.;
    column().border_2().border_color(t.text).children(rows.into_iter().enumerate().map(
        |(i, (label, value))| {
            div()
                .flex()
                .when(i > 0, |d| d.border_t_2().border_color(t.text))
                .child(
                    div()
                        .w(chars(width) + px(4.))
                        .flex_none()
                        .px(chars(1.))
                        .py(lines(0.5))
                        .border_r_2()
                        .border_color(t.text)
                        .font_weight(MEDIUM)
                        .child(label.to_uppercase()),
                )
                .child(div().flex_1().min_w_0().px(chars(1.)).py(lines(0.5)).child(value))
        },
    ))
}

pub fn clickable(
    id: impl Into<ElementId>,
    cx: &mut Context<Shell>,
    f: impl Fn(&mut Shell, &mut Context<Shell>) + 'static,
) -> Stateful<Div> {
    div()
        .id(id.into())
        .cursor_pointer()
        .on_click(cx.listener(move |this, _: &ClickEvent, _, cx| f(this, cx)))
}

/// A 2px block that inverts while pressed.
pub fn button(
    t: Theme,
    id: impl Into<ElementId>,
    label: &str,
    cx: &mut Context<Shell>,
    f: impl Fn(&mut Shell, &mut Context<Shell>) + 'static,
) -> Stateful<Div> {
    clickable(id, cx, f)
        .h(lines(2.))
        .px(chars(1.))
        .flex()
        .items_center()
        .justify_center()
        .border_2()
        .border_color(t.text)
        .font_weight(MEDIUM)
        .active(|s| s.bg(t.text).text_color(t.bg))
        .hover(|s| s.bg(t.bg_alt))
        .child(label.to_uppercase())
}

/// The emerald action that grants access, saying what it grants.
pub fn approve(
    t: Theme,
    id: impl Into<ElementId>,
    label: SharedString,
    enabled: bool,
    cx: &mut Context<Shell>,
    f: impl Fn(&mut Shell, &mut Context<Shell>) + 'static,
) -> Stateful<Div> {
    let block = |d: Stateful<Div>| {
        d.min_h(lines(2.)).px(chars(1.)).py(lines(0.5)).text_center().border_2().font_weight(MEDIUM)
    };
    if !enabled {
        return block(div().id(id.into())).border_color(t.alt).text_color(t.alt).child(label);
    }
    block(clickable(id, cx, f))
        .border_color(t.grant)
        .bg(t.grant)
        .text_color(t.on_grant)
        .active(|s| s.bg(t.grant_hi).border_color(t.grant_hi))
        .child(label)
}

pub fn link(
    t: Theme,
    id: impl Into<ElementId>,
    label: &str,
    cx: &mut Context<Shell>,
    f: impl Fn(&mut Shell, &mut Context<Shell>) + 'static,
) -> Stateful<Div> {
    clickable(id, cx, f).text_color(t.alt).underline().child(label.to_string())
}

/// An element ID for one of several alike actions, by the item's ID.
pub fn id_for(action: &str, item: &str) -> ElementId {
    ElementId::Name(format!("{action}-{item}").into())
}
