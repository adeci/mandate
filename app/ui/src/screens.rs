//! The frame every screen shares, the screens outside the tabs, and the
//! few widgets they are built from.

use crate::api::{Exchange, Opened};
use crate::device::hex;
use crate::theme::{BOLD, FONT, MEDIUM, NORMAL, SIZE, Theme, bold, chars, lines};
use crate::{Screen, Shell, Tab, words};
use gpui::{
    Animation, AnimationExt, AnyElement, BoxShadow, ClickEvent, Context, DispatchPhase, Div,
    ElementId, Hsla, LongPressEvent, MouseDownEvent, MouseMoveEvent, MouseUpEvent, Pixels, Point,
    ScrollWheelEvent, SharedString, Stateful, Svg, TouchDragEvent, TouchPhase, Window, canvas, div,
    ease_out_quint, fill, hsla, point, prelude::*, px, relative, rgb, svg,
};
use mandate_core::{Envelope, PairLink, Record};
use std::cell::Cell;
use std::rc::Rc;
use std::time::Duration;

/// How long the wordmark shows alone at launch, and how long its glow
/// takes to bloom and settle.
pub const LAUNCH: Duration = Duration::from_millis(340);
const WORDMARK_IN: Duration = Duration::from_millis(300);
/// A new page fades in this fast: enough to see the change, never enough
/// to wait for it.
const PAGE_IN: Duration = Duration::from_millis(90);

impl Render for Shell {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let t = Theme::of(window);
        // Back and typing reach the shell through its focus, which a system
        // prompt (fingerprint, screen lock) can take away.
        if window.focused(cx).is_none() {
            window.focus(&self.focus, cx);
        }
        #[cfg(target_os = "android")]
        let ((top, bottom), typing, pin) =
            (crate::android::insets(), crate::android::keyboard_up(), None::<Div>);
        #[cfg(not(target_os = "android"))]
        let ((top, bottom), typing, pin) = ((0., 0.), false, self.pin_prompt(t, cx));
        let root = div()
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
            .child(touches());
        if self.launching {
            // A quick emerald bloom behind the wordmark, which starts in
            // emerald too and settles to the text color.
            let (glow, text) = (t.grant_hi, t.text);
            let mark = wordmark().px(chars(1.)).rounded_full().with_animation(
                "launch",
                Animation::new(WORDMARK_IN),
                move |d, v| {
                    let bloom = (v * std::f32::consts::PI).sin();
                    d.opacity((v * 4.).min(1.)).text_color(mix(glow, text, v)).shadow(vec![
                        BoxShadow {
                            color: glow.opacity(0.5 * bloom),
                            offset: point(px(0.), px(0.)),
                            blur_radius: px(32. * bloom),
                            spread_radius: px(8. * bloom),
                            inset: false,
                        },
                    ])
                },
            );
            return root
                .items_center()
                .justify_center()
                .pb(lines(4.))
                .child(mark)
                .into_any_element();
        }
        if self.locked {
            if !self.unlocking {
                self.unlock_app(cx);
            }
            return root
                .items_center()
                .justify_center()
                .gap(lines(1.))
                .px(chars(2.))
                .pb(lines(4.))
                .child(wordmark())
                .children(pin)
                .when_some(self.error.clone(), |d, e| d.child(flash(t, e)))
                .child(button(t, "unlock-app", "Unlock", cx, |this, cx| this.unlock_app(cx)))
                .into_any_element();
        }
        let pad = if window.viewport_size().width < px(480.) { 1. } else { 2. };
        let tab = match self.screen {
            Screen::Tab(tab) => Some(tab),
            _ => None,
        };
        let shown = (std::mem::discriminant(&self.screen), tab);
        if self.page.0 != Some(shown) {
            self.page = (Some(shown), self.page.1 + 1);
        }
        // A pairing link, however it arrived (opened, scanned, typed or
        // pasted), is checked against its server once.
        if let Screen::Pair { text } = &self.screen
            && let Ok(link) = text.parse::<PairLink>()
            && self.pair_check.as_ref().is_none_or(|(l, _)| *l != link)
        {
            self.check_pair(link, cx);
        }
        // Activity loads when it first shows, again when what it asks for
        // changes, and further back as its end nears; the day scrolled past
        // keeps its header on top.
        let sticky = if tab == Some(Tab::Activity) {
            let (at, end) =
                (f32::from(self.scroll.offset().y), f32::from(self.scroll.max_offset().y));
            let near = -at >= end - 2. * f32::from(window.viewport_size().height);
            self.searched(self.search.clone().unwrap_or_default(), cx);
            match &self.history {
                Some(h) if h.query != self.wanted => self.load_history(cx),
                None => self.load_history(cx),
                Some(h) if h.next.is_some() && near => self.more_history(cx),
                Some(_) => {}
            }
            self.sticky_day()
        } else {
            None
        };
        let title = match &self.screen {
            Screen::Welcome => "",
            Screen::Pair { .. } => "Initial setup",
            Screen::Join { .. } | Screen::Joining => "Join",
            Screen::Invite { kind: "agent", .. } => "Add an agent",
            Screen::Invite { .. } => "Add a device",
            #[cfg(not(target_os = "android"))]
            Screen::Choose(_) => "Device key",
            Screen::Tab(Tab::Now) => "Now",
            Screen::Tab(Tab::Agents) => "Agents",
            Screen::Tab(Tab::Connections) => "Connections",
            Screen::Tab(Tab::Activity) => "Activity",
            Screen::Settings => "Settings",
            Screen::Admit { .. } => "Let in",
            Screen::Review { .. } => "Review",
            Screen::Link { .. } => "Link",
            Screen::Hide { .. } => "Never share",
            Screen::Grant(_) => "New grant",
            #[cfg(target_os = "android")]
            Screen::Method(_) => "How you approve",
            Screen::Entry(_) => "Activity",
        };
        let screen = match &self.screen {
            Screen::Welcome => welcome(t, cx),
            Screen::Pair { text } => {
                pair(t, text, self.pair_check.as_ref(), self.error.is_some(), cx)
            }
            Screen::Join { url, code, code_field } => join(t, url, code, *code_field, cx),
            Screen::Joining => joining(t),
            Screen::Invite { kind, opened } => self.invite_screen(t, kind, opened, cx),
            #[cfg(not(target_os = "android"))]
            Screen::Choose(c) => self.choose_screen(t, c, cx),
            Screen::Tab(Tab::Now) => self.now_tab(t, cx),
            Screen::Tab(Tab::Agents) => self.agents_tab(t, cx),
            Screen::Tab(Tab::Connections) => self.connections_tab(t, cx),
            Screen::Tab(Tab::Activity) => self.activity_tab(t, cx),
            Screen::Link { .. } => self.link_screen(t, cx),
            Screen::Hide { .. } => self.hide_screen(t, cx),
            Screen::Grant(_) => self.grant_screen(t, cx),
            #[cfg(target_os = "android")]
            Screen::Method(next) => self.method_screen(t, next, cx),
            Screen::Settings => self.settings(t, cx),
            Screen::Entry(a) => self.entry_screen(t, a),
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
        let notes = |d: Div| {
            d.when_some(self.error.clone(), |d, e| d.child(flash(t, e)))
                .when_some(self.notice.clone(), |d, n| d.child(flash(t, n)))
                .when_some(self.prompt, |d, p| d.child(flash(t, p.into())))
                .children(pin)
                .child(screen.gap(lines(1.)))
        };
        // Before pairing, each screen is a short column in the middle of the
        // window, a little above center; once paired, pages read from the top.
        let body = if self.device.is_none() {
            column()
                .min_h(relative(1.))
                .justify_center()
                .px(chars(pad))
                .pt(lines(1.))
                .pb(lines(4.))
                .child(notes(column().w_full().max_w(chars(46.)).mx_auto().gap(lines(1.))))
        } else {
            notes(page(pad).pt(lines(1.)).pb(lines(2.)).gap(lines(1.)))
        };
        // The page after the launch shows at once, as the wordmark already
        // led into it; later pages fade in.
        let body = if self.page.1 <= 1 {
            body.into_any_element()
        } else {
            body.with_animation(
                ("page", self.page.1),
                Animation::new(PAGE_IN).with_easing(ease_out_quint()),
                |d, v| d.opacity(v),
            )
            .into_any_element()
        };
        root.when(!matches!(self.screen, Screen::Welcome), |d| {
            d.child(self.top_bar(t, title, tab.is_some(), pad, cx))
        })
        .child(
            div()
                .relative()
                .flex_1()
                .min_h_0()
                .flex()
                .flex_col()
                .child(
                    crate::scroll::view("page", div().flex_1().min_h_0(), body).track(&self.scroll),
                )
                .children(sticky.map(|day| {
                    div()
                        .absolute()
                        .top_0()
                        .left_0()
                        .w_full()
                        .bg(t.bg)
                        .child(page(pad).child(crate::tabs::day_heading(&day)))
                })),
        )
        // The soft keyboard sits where the tab bar would; it returns as the keyboard goes.
        .when_some(tab.filter(|_| !typing), |d, tab| d.child(self.tab_bar(t, tab, pad, cx)))
        .when(tab == Some(Tab::Now), |d| d.children(self.adjust_sheet(t, bottom, cx)))
        .children(self.confirm_sheet(t, bottom, cx))
        .into_any_element()
    }
}

/// "mandate", as the launch shows it and the welcome greets with.
fn wordmark() -> Div {
    div().font_weight(BOLD).text_size(px(SIZE * 2.5)).line_height(px(SIZE * 3.)).child("mandate")
}

/// The color v of the way from a to b.
fn mix(a: Hsla, b: Hsla, v: f32) -> Hsla {
    let at = |x: f32, y: f32| x + (y - x) * v;
    Hsla { h: at(a.h, b.h), s: at(a.s, b.s), l: at(a.l, b.l), a: at(a.a, b.a) }
}

thread_local! {
    /// The slider a finger or the mouse is dragging, by ID.
    static SLIDING: Cell<Option<&'static str>> = const { Cell::new(None) };
}

/// A slider that snaps to stops, its value shown above it. A tap picks the
/// nearest stop; a drag follows the finger, stop by stop.
pub(crate) fn slider(
    t: Theme,
    id: &'static str,
    stops: Vec<(u64, String)>,
    current: u64,
    cx: &mut Context<Shell>,
    pick: impl Fn(&mut Shell, u64) + 'static,
) -> Div {
    let n = stops.len().max(1);
    let at = stops.iter().position(|(v, _)| *v == current).unwrap_or(0);
    let label = stops.get(at).map(|(_, l)| l.clone()).unwrap_or_default();
    let frac = |i: usize| if n > 1 { i as f32 / (n - 1) as f32 } else { 0. };
    let values: Rc<Vec<u64>> = Rc::new(stops.iter().map(|(v, _)| *v).collect());
    let (shell, pick) = (cx.entity().downgrade(), Rc::new(pick));
    let input = canvas(
        |_, _, _| (),
        move |bounds, _, window, _| {
            // The stop under x, and the shell moved to it.
            let set = move |x: Pixels, cx: &mut gpui::App| {
                let f = ((x - bounds.left()) / bounds.size.width).clamp(0., 1.);
                let i = (f * (values.len().max(1) - 1) as f32).round() as usize;
                let (v, pick) = (values[i], pick.clone());
                let _ = shell.update(cx, |this, cx| {
                    pick(this, v);
                    cx.notify();
                });
            };
            let set = Rc::new(set);
            let s = set.clone();
            window.on_mouse_event(move |e: &MouseDownEvent, phase, _, cx| {
                if phase == DispatchPhase::Bubble && bounds.contains(&e.position) {
                    SLIDING.set(Some(id));
                    s(e.position.x, cx);
                }
            });
            let s = set.clone();
            window.on_mouse_event(move |e: &MouseMoveEvent, phase, _, cx| {
                if phase == DispatchPhase::Bubble
                    && e.pressed_button.is_some()
                    && SLIDING.get() == Some(id)
                {
                    s(e.position.x, cx);
                }
            });
            window.on_mouse_event(move |_: &MouseUpEvent, phase, _, _| {
                if phase == DispatchPhase::Capture && SLIDING.get() == Some(id) {
                    SLIDING.set(None);
                }
            });
            // On touch, claiming the drag keeps the page from scrolling.
            window.on_mouse_event(move |e: &TouchDragEvent, phase, window, cx| {
                if phase != DispatchPhase::Bubble {
                    return;
                }
                match e.phase {
                    TouchPhase::Started if bounds.contains(&e.start_position) => {
                        window.prevent_default();
                        SLIDING.set(Some(id));
                        set(e.position.x, cx);
                    }
                    TouchPhase::Moved if SLIDING.get() == Some(id) => set(e.position.x, cx),
                    TouchPhase::Ended | TouchPhase::Cancelled if SLIDING.get() == Some(id) => {
                        SLIDING.set(None)
                    }
                    _ => {}
                }
            });
        },
    )
    .absolute()
    .top_0()
    .left_0()
    .size_full();
    let knob = px(18.);
    let track = div()
        .relative()
        .h(lines(2.))
        .mx(knob / 2.)
        .child(div().absolute().left_0().w_full().top(lines(1.) - px(1.)).h(px(2.)).bg(t.alt))
        .child(
            div()
                .absolute()
                .left_0()
                .w(relative(frac(at)))
                .top(lines(1.) - px(1.))
                .h(px(2.))
                .bg(t.text),
        )
        .children((0..n).map(|i| {
            div()
                .absolute()
                .left(relative(frac(i)))
                .ml(px(-1.))
                .top(lines(1.) - px(5.))
                .w(px(2.))
                .h(px(10.))
                .bg(if i <= at { t.text } else { t.alt })
        }))
        .child(
            div()
                .absolute()
                .left(relative(frac(at)))
                .ml(-knob / 2.)
                .top(lines(1.) - knob / 2.)
                .size(knob)
                .border_2()
                .border_color(t.text)
                .bg(t.text),
        )
        .child(input);
    column().child(div().font_weight(MEDIUM).child(label)).child(track)
}

thread_local! {
    /// Where a finger is down. gpui hands a tap to elements only as the
    /// finger lifts, a press and a release together, so `active` styles
    /// never show on touch; clickables shade themselves from this instead.
    static TOUCH: Cell<Option<Point<Pixels>>> = const { Cell::new(None) };
}

/// Follows a finger for the pressed shade: down, until it lifts as a tap,
/// a scroll or a long press.
fn touches() -> impl IntoElement {
    fn lift(window: &mut Window) {
        if TOUCH.take().is_some() {
            window.refresh();
        }
    }
    canvas(
        |_, _, _| (),
        |_, _, window, _| {
            window.on_mouse_event(|e: &TouchDragEvent, phase, window, _| {
                if phase == DispatchPhase::Capture && e.phase == TouchPhase::Started {
                    TOUCH.set(Some(e.start_position));
                    window.refresh();
                }
            });
            window.on_mouse_event(|_: &MouseUpEvent, phase, window, _| {
                if phase == DispatchPhase::Capture {
                    lift(window)
                }
            });
            window.on_mouse_event(|_: &ScrollWheelEvent, phase, window, _| {
                if phase == DispatchPhase::Capture {
                    lift(window)
                }
            });
            window.on_mouse_event(|_: &LongPressEvent, phase, window, _| {
                if phase == DispatchPhase::Capture {
                    lift(window)
                }
            });
        },
    )
    .absolute()
    .size_0()
}

/// A grey veil over a clickable while a finger is down on it, readable on
/// white, black and emerald alike.
pub(crate) fn shade() -> impl IntoElement {
    canvas(
        |_, _, _| (),
        |bounds, _, window, _| {
            let at = TOUCH.get().filter(|p| window.content_mask().bounds.contains(p));
            if at.is_some_and(|p| bounds.contains(&p)) {
                window.paint_quad(fill(bounds, hsla(0., 0., 0.5, 0.3)));
            }
        },
    )
    .absolute()
    .top_0()
    .left_0()
    .size_full()
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
    /// One step back: closes what is open on the page, else returns to the
    /// screen it came from. False at a root, where Android's Back leaves the
    /// app.
    pub(crate) fn go_back(&mut self, cx: &mut Context<Self>) -> bool {
        if self.picker.is_some() {
            self.close_picker(cx);
            return true;
        }
        if self.close_adjust(cx) {
            return true;
        }
        if self.search.is_some() || self.sheet.is_some() || self.renaming.is_some() {
            (self.search, self.sheet, self.renaming) = (None, None, None);
            crate::keyboard(false);
            cx.notify();
            return true;
        }
        self.pop(cx)
    }

    /// The sheet asking once more before a destructive or braking action:
    /// what happens, the action, and Cancel.
    fn confirm_sheet(&self, t: Theme, bottom: f32, cx: &mut Context<Self>) -> Option<Div> {
        use crate::Confirm::*;
        let (question, then, action) = match self.sheet.as_ref()? {
            DeleteAgent { name, .. } => {
                (format!("Delete {name}?"), "It loses all access.", "Delete")
            }
            RemoveDevice { name, .. } => {
                (format!("Remove {name}?"), "It can no longer approve.", "Remove")
            }
            Unlink { name } => (
                format!("Unlink {}?", words::connector(name)),
                "Its session is forgotten.",
                "Unlink",
            ),
            Close { what, .. } => (format!("Close {what}?"), "", "Close"),
            StopAll => (
                "Stop all access?".into(),
                "Every grant closes, routines pause and waiting requests are denied.",
                "Stop all",
            ),
        };
        let stop = matches!(self.sheet, Some(StopAll));
        let act = button(t, "sheet-act", action, cx, |this, cx| this.confirmed(cx))
            .when(stop, |b| b.bg(t.danger).border_color(t.danger).text_color(rgb(0xffffff)));
        let scrim = div().id("sheet-scrim").flex_1().bg(hsla(0., 0., 0., 0.4)).on_click(
            cx.listener(|this, _, _, cx| {
                this.sheet = None;
                cx.notify();
            }),
        );
        let sheet = div()
            .id("sheet")
            .occlude()
            .bg(t.bg)
            .border_t_2()
            .border_color(t.text)
            .pt(lines(1.))
            .pb(px(bottom) + lines(1.))
            .px(chars(1.))
            .child(
                column()
                    .w_full()
                    .max_w(chars(60.))
                    .mx_auto()
                    .gap(lines(1.))
                    .child(bold(question))
                    .when(!then.is_empty(), |d| d.child(then))
                    .child(act)
                    .child(button(t, "sheet-cancel", "Cancel", cx, |this, cx| {
                        this.sheet = None;
                        cx.notify();
                    })),
            );
        Some(
            div()
                .absolute()
                .top_0()
                .left_0()
                .size_full()
                .flex()
                .flex_col()
                .child(scrim)
                .child(sheet),
        )
    }

    /// How this device approves: fingerprint or face (the screen lock as
    /// backup), or the screen lock alone. When changing, the current way
    /// is marked.
    #[cfg(target_os = "android")]
    fn method_screen(&self, t: Theme, next: &crate::Next, cx: &mut Context<Self>) -> Div {
        use crate::android::Method;
        let current = matches!(next, crate::Next::Rotate)
            .then(|| self.device.as_ref().map(|d| d.key.method()))
            .flatten();
        let choice = |id: &'static str, method: Method, label: &str, cx: &mut Context<Self>| {
            let next = next.clone();
            let on = current == Some(method);
            clickable(id, cx, move |this, cx| {
                if !on {
                    this.make_key(next.clone(), method, cx)
                }
            })
            .flex()
            .items_center()
            .gap(chars(1.))
            .border_2()
            .border_color(t.text)
            .px(chars(1.))
            .py(lines(1.))
            .font_weight(MEDIUM)
            .child(crate::now::checkbox(t, on))
            .child(label.to_string())
        };
        column()
            .gap(lines(1.))
            .child(bold("How do you want to approve?"))
            .child(choice("biometric", Method::Biometric, "Fingerprint or face", cx))
            .child(choice("screen-lock", Method::ScreenLock, "Screen lock", cx))
    }

    /// The page's name, a way back off the tabs, and search and settings on
    /// them.
    fn top_bar(&self, t: Theme, title: &str, tabs: bool, pad: f32, cx: &mut Context<Self>) -> Div {
        // Welcome draws no bar, so every page with one but the tabs has a way back.
        let back = !tabs;
        let row = page(pad)
            .flex_row()
            .items_center()
            .h(lines(3.))
            .gap(chars(1.))
            .when(back, |d| {
                d.child(icon_button(t, "back", "arrow-left", cx, |this, cx| {
                    if !this.go_back(cx) {
                        this.up(cx);
                    }
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
                    t.alt("Search").flex_1()
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

    /// Thumb-reach tabs: icon over label, the current one in full weight. On
    /// a wide window they keep to the page's measure.
    fn tab_bar(&self, t: Theme, current: Tab, pad: f32, cx: &mut Context<Self>) -> Div {
        let confirmed = self.exchanges.iter().filter(|e| e.state == "confirmed").count();
        let waiting = self.now.requests.len() + self.now.proposals.len() + confirmed;
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
            clickable(("tab", i), cx, move |this, cx| this.root(tab, cx))
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
        div().border_t_2().border_color(t.text).child(page(pad).flex_row().children(cells))
    }
}

/// What mandate is, and the two ways in.
fn welcome(t: Theme, cx: &mut Context<Shell>) -> Div {
    column()
        .child(wordmark())
        .child("A self-hosted gateway to your personal accounts for AI agents.")
        .child(t.alt("Agents read and send only what you approve."))
        .child(button(t, "start", "Get started", cx, |this, cx| {
            this.go(Screen::Pair { text: String::new() }, cx)
        }))
        .child(div().flex().flex_wrap().gap_x(chars(1.)).child(t.alt("Already set up?")).child(
            link(t, "join", "Join as another device", cx, |this, cx| {
                this.go(
                    Screen::Join { url: String::new(), code: String::new(), code_field: false },
                    cx,
                )
            }),
        ))
}

/// A box the owner types or pastes into: a tap gives it the typing and
/// opens the keyboard, and Paste beside it takes the clipboard.
pub(crate) fn field(
    t: Theme,
    id: &'static str,
    text: &str,
    empty: &str,
    focused: bool,
    focus: impl Fn(&mut Shell) + 'static,
    cx: &mut Context<Shell>,
) -> Div {
    let shown = match (text.is_empty(), focused) {
        (true, _) => empty.to_string(),
        (false, true) => format!("{text}▏"),
        (false, false) => text.to_string(),
    };
    let paste_id = ElementId::Name(format!("paste-{id}").into());
    let take = std::rc::Rc::new(focus);
    let take2 = take.clone();
    div()
        .flex()
        .gap(chars(1.))
        .child(
            clickable(id, cx, move |this, cx| {
                take(this);
                crate::keyboard(true);
                cx.notify();
            })
            .flex_1()
            .min_w_0()
            .border_2()
            .border_color(if focused { t.text } else { t.alt })
            .px(chars(1.))
            .py(lines(0.5))
            .when(text.is_empty(), |d| d.text_color(t.alt))
            .child(shown),
        )
        .child(
            button(t, paste_id, "Paste", cx, move |this, cx| {
                take2(this);
                this.paste(cx)
            })
            .flex_none(),
        )
}

/// The Pair screen: a way to bring in the setup page's link, then, once the
/// app has connected to its server, either the verified hostname or the
/// short key to compare.
fn pair(
    t: Theme,
    text: &str,
    check: Option<&(PairLink, Option<bool>)>,
    failed: bool,
    cx: &mut Context<Shell>,
) -> Div {
    let Some(link) = text.parse::<PairLink>().ok() else {
        let open = if cfg!(target_os = "android") {
            "Scan the QR code on the setup page."
        } else {
            "Open the pairing link on the setup page."
        };
        #[cfg(target_os = "android")]
        let scan = Some(button(t, "scan", "Scan QR code", cx, |_, _| crate::android::scan()));
        #[cfg(not(target_os = "android"))]
        let scan = None::<Stateful<Div>>;
        return column()
            .child(open)
            .children(scan)
            .child(t.alt("Or paste the link").mt(lines(1.)))
            .child(field(t, "link", text, "mandate://pair?…", true, |_| {}, cx));
    };
    let host = link.url.strip_prefix("https://").unwrap_or(&link.url).to_string();
    let host =
        div().font_weight(BOLD).text_size(px(SIZE * 1.5)).line_height(lines(1.5)).child(host);
    let verified = check.filter(|(l, _)| *l == link).and_then(|(_, v)| *v);
    let Some(verified) = verified else {
        if failed {
            return column().child(host).child(button(t, "retry", "Try again", cx, |this, cx| {
                (this.pair_check, this.error) = (None, None);
                cx.notify();
            }));
        }
        return column().child(host).child(t.alt("Checking the server…"));
    };
    let short = fingerprint(&link.spki_sha256);
    let pair = button(t, "pair", "Pair", cx, move |this, cx| {
        this.new_key(crate::Next::Pair(link.clone()), cx)
    });
    if verified {
        return column()
            .child(host)
            .child(div().font_weight(MEDIUM).child("✓ Certificate verified"))
            .child(pair);
    }
    column()
        .child(host)
        .child(table(
            t,
            vec![("Key".into(), div().font_weight(BOLD).child(short).into_any_element())],
        ))
        .child(pair)
}

/// The first 80 bits of a key's SPKI SHA-256, as five groups of four hex
/// digits, as the setup page and `mandate setup` print them.
pub(crate) fn fingerprint(spki_sha256: &[u8; 32]) -> String {
    let key = hex(&spki_sha256[..10]);
    let groups: Vec<&str> = (0..key.len()).step_by(4).map(|i| &key[i..i + 4]).collect();
    groups.join(" ")
}

/// Joining as another device: the server's address and the code that a
/// device already in shows under Settings, Add a device.
fn join(t: Theme, url: &str, code: &str, code_field: bool, cx: &mut Context<Shell>) -> Div {
    let ready = url.trim().starts_with("https://") && code.len() == 6;
    let to = |code_field: bool| {
        move |this: &mut Shell| {
            if let Screen::Join { code_field: on, .. } = &mut this.screen {
                *on = code_field;
            }
        }
    };
    column()
        .child("From Settings › Add a device on your other device:")
        .child(t.alt("Server address"))
        .child(field(t, "url", url, "https://…", !code_field, to(false), cx))
        .child(t.alt("Code"))
        .child(field(t, "code", code, "6 digits", code_field, to(true), cx))
        .when(ready, |d| {
            d.child(button(t, "continue", "Continue", cx, |this, cx| this.submit_join(cx)))
        })
}

fn joining(t: Theme) -> Div {
    column()
        .child("Code accepted. Let this device in on your other device.")
        .child(t.alt("Waiting…"))
}

impl Shell {
    /// The code of an exchange the owner opened, to give an agent or a new
    /// device, with what to run or type there. It moves on by itself once
    /// the code matches.
    fn invite_screen(&self, t: Theme, kind: &str, opened: &Opened, cx: &mut Context<Self>) -> Div {
        let code = opened.code.clone();
        // What to run or enter there, in a ruled block with Copy at its end.
        let block = |text: String, id: &'static str, cx: &mut Context<Self>| {
            let copied = text.clone();
            div()
                .w_full()
                .flex()
                .items_stretch()
                .border_2()
                .border_color(t.text)
                .child(
                    div()
                        .flex_1()
                        .min_w_0()
                        .px(chars(1.))
                        .py(lines(0.5))
                        .font_weight(MEDIUM)
                        .child(text),
                )
                .child(
                    clickable(id, cx, move |this, cx| this.copy(copied.clone(), cx))
                        .flex()
                        .items_center()
                        .px(chars(1.))
                        .border_l_2()
                        .border_color(t.text)
                        .font_weight(MEDIUM)
                        .child("COPY"),
                )
        };
        let (first, there) = if kind == "agent" {
            let command = format!("mandate login {}", self.agent_url);
            ("On the agent's machine, run:", block(command, "copy-command", cx))
        } else {
            (
                "On the new device, tap Already set up? and enter:",
                block(self.server.clone(), "copy-url", cx),
            )
        };
        let copy_code = code.clone();
        column()
            .items_center()
            .text_center()
            .gap(lines(1.))
            .mt(lines(2.))
            .child(first)
            .child(there)
            .child(div().mt(lines(1.)).child("Then type this code:"))
            .child(
                div()
                    .text_size(px(SIZE * 3.5))
                    .line_height(px(SIZE * 4.))
                    .font_weight(BOLD)
                    .child(code),
            )
            .child(
                link(t, "copy-code", "Copy code", cx, move |this, cx| {
                    this.copy(copy_code.clone(), cx)
                })
                .font_weight(MEDIUM),
            )
            .child(t.alt(format!("Expires {}", words::clock(&opened.expires_at))))
            .child(link(t, "cancel-invite", "Cancel", cx, |this, cx| this.cancel_invite(cx)))
    }
}

fn admit(t: Theme, ex: &Exchange, envelope: &Envelope, cx: &mut Context<Shell>) -> Div {
    let envelope = envelope.clone();
    // Deny sits above, as on every card, so Let in's place is the thumb's.
    let deny = deny_exchange(t, ex, cx);
    if ex.kind == "device" {
        // A device can approve anything, so the warning sits on the button.
        let admit = approve(t, "admit", "Let this device in".into(), true, cx, move |this, cx| {
            this.approve(envelope.clone(), cx)
        });
        return column()
            .child(bold(DEVICE_JOIN.to_string()))
            .child(table(t, device_rows(ex)))
            .child(deny)
            .child(column().child(device_warning(t)).child(admit));
    }
    let mut rows = vec![("Kind".to_string(), div().child("agent").into_any_element())];
    if !ex.address.is_empty() {
        rows.push(("From".into(), div().child(ex.address.clone()).into_any_element()));
    }
    column().child(bold(format!("Let {} in?", ex.name))).child(table(t, rows)).child(deny).child(
        approve(t, "admit", format!("Let {} in", ex.name).into(), true, cx, move |this, cx| {
            this.approve(envelope.clone(), cx)
        }),
    )
}

/// Deny for a party whose code matched: the exchange ends, and the party is
/// told it was not let in.
pub fn deny_exchange(t: Theme, ex: &Exchange, cx: &mut Context<Shell>) -> Stateful<Div> {
    let (id, name) = (ex.id.clone(), ex.name.clone());
    button(t, id_for("deny-exchange", &ex.id), "Deny", cx, move |this, cx| {
        let notice = format!("You didn't let {name} in.");
        this.post("DELETE", format!("exchanges/{id}"), cx, move |_| Some(notice))
    })
}

/// The title of every device join, never the name the device gave itself.
pub const DEVICE_JOIN: &str = "A new device wants to join";

/// What a joining device calls itself and where it asks from.
pub fn device_rows(ex: &Exchange) -> Vec<(String, AnyElement)> {
    let from = if ex.address.is_empty() { "unknown" } else { ex.address.as_str() };
    vec![
        ("Name".to_string(), div().child(ex.name.clone()).into_any_element()),
        ("From".to_string(), div().child(from.to_string()).into_any_element()),
    ]
}

/// The warning shown before letting a device in: an agent can start a join
/// from its own machine and ask the owner to type the code.
pub fn device_warning(t: Theme) -> Div {
    div()
        .border_2()
        .border_color(t.text)
        .px(chars(1.))
        .py(lines(0.5))
        .font_weight(MEDIUM)
        .child("Only let it in if you started this yourself: a device can approve anything.")
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

/// Anything tapped: buttons, links, chips and rows. Each shades while a
/// finger is down on it.
pub fn clickable(
    id: impl Into<ElementId>,
    cx: &mut Context<Shell>,
    f: impl Fn(&mut Shell, &mut Context<Shell>) + 'static,
) -> Stateful<Div> {
    div()
        .id(id.into())
        .cursor_pointer()
        .on_click(cx.listener(move |this, _: &ClickEvent, _, cx| f(this, cx)))
        .child(shade())
}

/// Every button's shape: a 2px block at least two lines high, its label
/// in capitals, centered.
fn block(d: Stateful<Div>, t: Theme, label: &str) -> Stateful<Div> {
    d.min_h(lines(2.))
        .px(chars(1.))
        .py(lines(0.25))
        .flex()
        .items_center()
        .justify_center()
        .text_center()
        .border_2()
        .border_color(t.text)
        .font_weight(MEDIUM)
        .child(label.to_uppercase())
}

/// A button that inverts while pressed.
pub fn button(
    t: Theme,
    id: impl Into<ElementId>,
    label: &str,
    cx: &mut Context<Shell>,
    f: impl Fn(&mut Shell, &mut Context<Shell>) + 'static,
) -> Stateful<Div> {
    block(clickable(id, cx, f), t, label)
        .active(|s| s.bg(t.text).text_color(t.bg))
        .hover(|s| s.bg(t.bg_alt))
}

/// The button that grants access, saying what it grants: the same shape,
/// filled with the deep emerald.
pub fn approve(
    t: Theme,
    id: impl Into<ElementId>,
    label: SharedString,
    enabled: bool,
    cx: &mut Context<Shell>,
    f: impl Fn(&mut Shell, &mut Context<Shell>) + 'static,
) -> Stateful<Div> {
    if !enabled {
        return block(div().id(id.into()), t, &label).border_color(t.alt).text_color(t.alt);
    }
    block(clickable(id, cx, f), t, &label)
        .bg(t.grant)
        .text_color(t.on_grant)
        .active(|s| s.bg(t.grant_hi))
        .hover(|s| s.bg(t.grant_hi))
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
