//! Linking a connector: the steps it describes as data (connector.md,
//! Setup), shown one at a time, and unlinking.

use crate::api::{SetupField, Step};
use crate::screens::{button, clickable, column, icon, id_for};
use crate::theme::{BOLD, MEDIUM, SIZE, Theme, chars, lines};
use crate::words::{connector, connector_icon};
use crate::{Screen, Shell, keyboard, segment};
use gpui::{Context, Div, div, prelude::*, px, rgb};
use qrcode::{Color, QrCode};
use serde_json::json;
use std::collections::BTreeMap;
use std::time::Duration;

impl Shell {
    pub(crate) fn open_link(&mut self, name: String, cx: &mut Context<Self>) {
        let client = self.device().client;
        let path = format!("connectors/{}/setup", segment(&name));
        self.go(Screen::Link { name, step: None, values: BTreeMap::new(), focus: None }, cx);
        self.call(cx, move || client.get::<Step>(&path), |this, step, cx| this.took(step, cx));
    }

    /// Sends the current step with the values typed into it.
    pub(crate) fn submit_step(&mut self, cx: &mut Context<Self>) {
        if let Screen::Link { step: Some(step), values, .. } = &self.screen {
            let values = values.clone();
            let id = step.step.clone();
            self.answer(id, values, cx);
        }
    }

    fn answer(&mut self, id: String, values: BTreeMap<String, String>, cx: &mut Context<Self>) {
        let Screen::Link { name, .. } = &self.screen else { return };
        let client = self.device().client;
        let path = format!("connectors/{}/setup", segment(name));
        self.error = None;
        self.call(
            cx,
            move || client.send::<Step>("POST", &path, &json!({"step": id, "values": values})),
            |this, step, cx| this.took(step, cx),
        );
    }

    /// Shows the step the connector answered, and re-sends it once its
    /// poll is due while it still shows.
    fn took(&mut self, next: Step, cx: &mut Context<Self>) {
        let Screen::Link { name, step, values, focus } = &mut self.screen else { return };
        if next.done {
            let notice = format!("{} is linked.", connector(name));
            self.up(cx);
            self.notice = Some(notice);
            return self.refresh(cx);
        }
        let same = step.as_ref().is_some_and(|s| s.step == next.step);
        if !same {
            values.clear();
            *focus = next.fields.iter().find(|f| f.kind != "choice").map(|f| f.name.clone());
            keyboard(focus.is_some());
        }
        let (poll, id, name) = (next.poll, next.step.clone(), name.clone());
        *step = Some(next);
        if poll == 0 {
            return;
        }
        cx.spawn(async move |this, cx| {
            cx.background_executor().timer(Duration::from_secs(poll)).await;
            this.update(cx, |this, cx| {
                let showing = matches!(&this.screen, Screen::Link { name: n, step: Some(s), .. }
                    if *n == name && s.step == id);
                if showing && this.error.is_none() {
                    this.answer(id, BTreeMap::new(), cx);
                }
            })
        })
        .detach();
    }

    pub(crate) fn unlink(&mut self, name: String, cx: &mut Context<Self>) {
        let notice = format!("{} unlinked.", connector(&name));
        self.post("POST", format!("connectors/{}/unlink", segment(&name)), cx, move |_| {
            Some(notice)
        });
    }

    pub(crate) fn link_screen(&self, t: Theme, cx: &mut Context<Self>) -> Div {
        let Screen::Link { name, step, values, focus } = &self.screen else { return column() };
        let Some(step) = step else {
            return column().child(t.alt("Loading…"));
        };
        let filled = step
            .fields
            .iter()
            .all(|f| !f.required || values.get(&f.name).is_some_and(|v| !v.is_empty()));
        column()
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(chars(1.))
                    .font_weight(BOLD)
                    .child(icon(connector_icon(name), t.text))
                    .child(if step.title.is_empty() {
                        format!("Link {}", connector(name))
                    } else {
                        step.title.clone()
                    }),
            )
            .when(!step.text.is_empty(), |d| d.child(step.text.clone()))
            .children(step.fields.iter().map(|f| field(t, f, values, focus.as_deref(), cx)))
            .when(!step.qr.is_empty(), |d| d.child(qr(&step.qr)))
            .when(!step.code.is_empty(), |d| {
                d.child(
                    div().flex().child(
                        div()
                            .border_2()
                            .border_color(t.text)
                            .px(chars(2.))
                            .py(lines(0.5))
                            .text_size(px(SIZE * 1.5))
                            .font_weight(BOLD)
                            .child(step.code.clone()),
                    ),
                )
            })
            .when(!step.confirm.is_empty(), |d| {
                let (yes, no) = (step.step.clone(), step.step.clone());
                d.child(step.confirm.clone()).child(
                    div()
                        .flex()
                        .gap(chars(1.))
                        .child(
                            button(t, "yes", "Yes", cx, move |this, cx| {
                                let values = BTreeMap::from([("confirm".into(), "yes".into())]);
                                this.answer(yes.clone(), values, cx)
                            })
                            .flex_1(),
                        )
                        .child(
                            button(t, "no", "No", cx, move |this, cx| {
                                let values = BTreeMap::from([("confirm".into(), "no".into())]);
                                this.answer(no.clone(), values, cx)
                            })
                            .flex_1(),
                        ),
                )
            })
            .when(step.poll > 0, |d| d.child(t.alt("Waiting for the other device…")))
            .when(!step.fields.is_empty() && filled, |d| {
                d.child(button(t, "continue", "Continue", cx, |this, cx| this.submit_step(cx)))
            })
    }
}

/// A labelled field: a box to type into, or the choices as chips.
fn field(
    t: Theme,
    f: &SetupField,
    values: &BTreeMap<String, String>,
    focus: Option<&str>,
    cx: &mut Context<Shell>,
) -> Div {
    let value = values.get(&f.name).cloned().unwrap_or_default();
    let label = column()
        .child(div().font_weight(MEDIUM).child(f.label.to_uppercase()))
        .when(!f.required, |d| d.child(t.alt("optional")));
    if f.kind == "choice" {
        let chips = f.options.iter().map(|o| {
            let (name, v) = (f.name.clone(), o.value.clone());
            clickable(id_for(&f.name, &o.value), cx, move |this, cx| {
                if let Screen::Link { values, .. } = &mut this.screen {
                    values.insert(name.clone(), v.clone());
                }
                cx.notify();
            })
            .px(chars(1.))
            .py(lines(0.5))
            .border_2()
            .border_color(t.text)
            .when(value == o.value, |d| d.bg(t.text).text_color(t.bg))
            .child(o.label.clone())
        });
        return column()
            .gap(lines(0.5))
            .child(label)
            .child(div().flex().flex_wrap().gap(chars(1.)).children(chips));
    }
    let on = focus == Some(f.name.as_str());
    let shown = if f.kind == "secret" { "•".repeat(value.chars().count()) } else { value };
    let name = f.name.clone();
    column().gap(lines(0.5)).child(label).child(
        clickable(id_for("field", &f.name), cx, move |this, cx| {
            if let Screen::Link { focus, .. } = &mut this.screen {
                *focus = Some(name.clone());
            }
            keyboard(true);
            cx.notify();
        })
        .border_2()
        .border_color(if on { t.text } else { t.alt })
        .px(chars(1.))
        .py(lines(0.5))
        .min_h(lines(2.))
        .child(format!("{shown}{}", if on { "▏" } else { "" })),
    )
}

/// The text as a QR code, dark on light in either theme so the service's
/// own app can scan it, with its quiet zone.
fn qr(text: &str) -> Div {
    let Ok(code) = QrCode::new(text.as_bytes()) else {
        return div().child(text.to_string());
    };
    let width = code.width();
    let colors = code.to_colors();
    let module = px((240. / (width + 8) as f32).floor().max(2.));
    let rows = colors.chunks(width).map(|row| {
        // Runs of one color, so a row is a few boxes, not one per module.
        let mut runs: Vec<(Color, usize)> = Vec::new();
        for &c in row {
            match runs.last_mut() {
                Some((last, n)) if *last == c => *n += 1,
                _ => runs.push((c, 1)),
            }
        }
        div().flex().children(runs.into_iter().map(move |(c, n)| {
            div().w(module * n as f32).h(module).when(c == Color::Dark, |d| d.bg(rgb(0x000000)))
        }))
    });
    div().flex().child(column().bg(rgb(0xffffff)).p(module * 4.).children(rows))
}
