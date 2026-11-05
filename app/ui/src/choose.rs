//! Picking this computer's device key as it pairs or joins: a FIDO2
//! authenticator, or a P-256 key on a PKCS#11 token. There is no software
//! key on desktop.

use crate::desktop::Key;
use crate::desktop::fido::{self, Authenticator, Fido};
use crate::desktop::pkcs11::{self, Found, Login, Pkcs11, Token};
use crate::screens::{button, clickable, column, id_for};
use crate::theme::{BOLD, Theme, chars, lines};
use crate::{Next, Screen, Shell, device, type_into};
use gpui::{Context, Div, KeyDownEvent, div, prelude::*};

pub struct Choose {
    then: Next,
    kind: Option<Kind>,
    field: Field,
    pin: String,
    module: String,
    authenticators: Vec<Authenticator>,
    /// The authenticator picked, by index.
    picked: usize,
    tokens: Vec<Token>,
    token: Option<Token>,
    login: Option<Login>,
    keys: Vec<Found>,
}

#[derive(Clone, Copy, PartialEq)]
enum Kind {
    Fido,
    Pkcs11,
}

#[derive(Clone, Copy, PartialEq)]
enum Field {
    Pin,
    Module,
}

impl Choose {
    pub fn new(then: Next) -> Choose {
        Choose {
            then,
            kind: None,
            field: Field::Pin,
            pin: String::new(),
            module: String::new(),
            authenticators: Vec::new(),
            picked: 0,
            tokens: Vec::new(),
            token: None,
            login: None,
            keys: Vec::new(),
        }
    }
}

impl Shell {
    fn choosing(&mut self) -> Option<&mut Choose> {
        match &mut self.screen {
            Screen::Choose(c) => Some(c),
            _ => None,
        }
    }

    pub(crate) fn choose_screen(&self, t: Theme, c: &Choose, cx: &mut Context<Self>) -> Div {
        match c.kind {
            None => kinds(t, &self.methods_left(), cx),
            Some(Kind::Fido) => fido_step(t, c, cx),
            Some(Kind::Pkcs11) => pkcs11_step(t, c, cx),
        }
    }

    pub(crate) fn choose_key(&mut self, ev: &KeyDownEvent, cx: &mut Context<Self>) {
        let Some(c) = self.choosing() else { return };
        let (enter, field) = (ev.keystroke.key == "enter", c.field);
        match (enter, field) {
            (true, Field::Pin) if c.kind == Some(Kind::Fido) => self.create_fido(cx),
            (true, Field::Pin) => self.log_in(cx),
            (true, Field::Module) => self.find_tokens(cx),
            (false, Field::Pin) => type_into(&mut c.pin, ev, cx),
            (false, Field::Module) => type_into(&mut c.module, ev, cx),
        }
        cx.notify();
    }

    fn pick_kind(&mut self, kind: Kind, cx: &mut Context<Self>) {
        let Some(c) = self.choosing() else { return };
        c.kind = Some(kind);
        c.field = if kind == Kind::Fido { Field::Pin } else { Field::Module };
        if kind == Kind::Fido {
            c.authenticators = fido::authenticators();
            c.picked = 0;
        }
        cx.notify();
    }

    fn create_fido(&mut self, cx: &mut Context<Self>) {
        let Some(c) = self.choosing() else { return };
        let Some(a) = c.authenticators.get(c.picked) else { return };
        let a = Authenticator { path: a.path.clone(), name: a.name.clone() };
        let (pin, then) = (std::mem::take(&mut c.pin), c.then.clone());
        self.error = None;
        self.prompt = Some("Touch your security key.");
        self.call(
            cx,
            move || Ok(Key::fido(Fido::create(&a, &pin, &device::name())?, &a.name)),
            move |this, key, cx| {
                this.prompt = None;
                this.with_key(then, key, cx);
            },
        );
    }

    fn find_tokens(&mut self, cx: &mut Context<Self>) {
        let Some(c) = self.choosing() else { return };
        let module = c.module.trim().to_string();
        self.error = None;
        self.call(
            cx,
            move || pkcs11::tokens(module.as_ref()),
            |this, tokens, _| {
                let Some(c) = this.choosing() else { return };
                c.token = match &tokens[..] {
                    [only] => Some(only.clone()),
                    _ => None,
                };
                (c.tokens, c.login, c.keys, c.field) = (tokens, None, Vec::new(), Field::Pin);
                if c.tokens.is_empty() {
                    this.error = Some("No token found. Plug it in and try again.".into());
                }
            },
        );
    }

    fn log_in(&mut self, cx: &mut Context<Self>) {
        let Some(c) = self.choosing() else { return };
        let Some(token) = &c.token else { return };
        let (module, serial) = (c.module.trim().to_string(), token.serial.clone());
        let pin = std::mem::take(&mut c.pin);
        self.error = None;
        self.call(
            cx,
            move || {
                let login = Login::open(module.as_ref(), &serial, &pin)?;
                let keys = login.keys()?;
                Ok((login, keys))
            },
            |this, (login, keys), _| {
                if let Some(c) = this.choosing() {
                    (c.login, c.keys) = (Some(login), keys);
                }
            },
        );
    }

    /// Uses a key found on the token, or with found None, makes one.
    fn use_pkcs11(&mut self, found: Option<Found>, cx: &mut Context<Self>) {
        let Some(c) = self.choosing() else { return };
        let (Some(login), Some(token)) = (c.login.take(), c.token.clone()) else { return };
        let (module, then) = (c.module.trim().to_string(), c.then.clone());
        self.error = None;
        self.call(
            cx,
            move || {
                let found = match found {
                    Some(f) => f,
                    None => login.create("mandate")?,
                };
                let key = Pkcs11::new(module.as_ref(), &token.serial, &found, Some(login));
                Ok(Key::pkcs11(key, &token.label))
            },
            move |this, key, cx| this.with_key(then, key, cx),
        );
    }
}

fn kinds(t: Theme, left: &[&str], cx: &mut Context<Shell>) -> Div {
    let mut choice = |id: &'static str, kind: Kind, label: &str, name: &str, text: &str| {
        left.contains(&label).then(|| {
            clickable(id, cx, move |this, cx| this.pick_kind(kind, cx))
                .border_2()
                .border_color(t.text)
                .px(chars(1.))
                .py(lines(0.5))
                .hover(|s| s.bg(t.bg_alt))
                .child(div().font_weight(BOLD).child(name.to_uppercase()))
                .child(text.to_string())
        })
    };
    column()
        .child("Which key signs approvals on this computer?")
        .children(choice(
            "fido",
            Kind::Fido,
            "security key",
            "Security key (FIDO2)",
            "A YubiKey, Nitrokey, SoloKey or other FIDO2 key. Recommended.",
        ))
        .children(choice(
            "pkcs11",
            Kind::Pkcs11,
            "token",
            "PKCS#11 token",
            "A P-256 key on a smart card, a TPM or a YubiKey's PIV.",
        ))
}

fn fido_step(t: Theme, c: &Choose, cx: &mut Context<Shell>) -> Div {
    if c.authenticators.is_empty() {
        return column().child("No security key found. Plug one in.").child(div().flex().child(
            button(t, "look", "Look again", cx, |this, cx| this.pick_kind(Kind::Fido, cx)),
        ));
    }
    let keys = c.authenticators.iter().enumerate().map(|(i, a)| {
        let on = i == c.picked;
        clickable(("authenticator", i), cx, move |this, cx| {
            if let Some(c) = this.choosing() {
                c.picked = i;
            }
            cx.notify();
        })
        .px(chars(1.))
        .py(lines(0.5))
        .when(i > 0, |d| d.border_t_2().border_color(t.text))
        .when(on, |d| d.font_weight(BOLD))
        .child(format!("{} {}", if on { "(•)" } else { "( )" }, a.name))
    });
    column()
        .child("Your security key:")
        .child(column().border_2().border_color(t.text).children(keys))
        .child(field(t, "pin", "PIN", &c.pin, true, true, cx, |c, _| c.field = Field::Pin))
        .child(
            div()
                .flex()
                .child(button(t, "use-fido", "Use this key", cx, |this, cx| this.create_fido(cx))),
        )
}

fn pkcs11_step(t: Theme, c: &Choose, cx: &mut Context<Shell>) -> Div {
    let mut step = column()
        .child(field(t, "module", "Module", &c.module, false, c.field == Field::Module, cx, |c, _| {
            c.field = Field::Module
        }))
        .child(t.alt("e.g. libykcs11.so (YubiKey PIV), libtpm2_pkcs11.so (TPM), opensc-pkcs11.so (smart card)"))
        .child(div().flex().child(button(t, "tokens", "Find tokens", cx, |this, cx| {
            this.find_tokens(cx)
        })));
    if c.tokens.len() > 1 {
        let tokens = c.tokens.iter().enumerate().map(|(i, tk)| {
            let on = c.token.as_ref().is_some_and(|x| x.serial == tk.serial);
            let picked = tk.clone();
            clickable(("token", i), cx, move |this, cx| {
                if let Some(c) = this.choosing() {
                    (c.token, c.login, c.keys) = (Some(picked.clone()), None, Vec::new());
                }
                cx.notify();
            })
            .px(chars(1.))
            .py(lines(0.5))
            .when(i > 0, |d| d.border_t_2().border_color(t.text))
            .when(on, |d| d.font_weight(BOLD))
            .child(format!("{} {}", if on { "(•)" } else { "( )" }, tk.label))
        });
        step = step.child(column().border_2().border_color(t.text).children(tokens));
    }
    let Some(token) = &c.token else { return step };
    if c.login.is_none() {
        let label = format!("{} PIN", token.label);
        return step
            .child(field(t, "pin", &label, &c.pin, true, c.field == Field::Pin, cx, |c, _| {
                c.field = Field::Pin
            }))
            .child(
                div().flex().child(button(t, "log-in", "Log in", cx, |this, cx| this.log_in(cx))),
            );
    }
    let keys: Vec<Div> = c
        .keys
        .iter()
        .enumerate()
        .map(|(i, k)| {
            let found = k.clone();
            let name = if k.label.is_empty() { "unnamed key" } else { k.label.as_str() };
            div()
                .flex()
                .items_center()
                .gap(chars(1.))
                .px(chars(1.))
                .py(lines(0.5))
                .when(i > 0, |d| d.border_t_2().border_color(t.text))
                .child(div().flex_1().child(name.to_string()))
                .child(button(t, id_for("use", &device::hex(&k.id)), "Use", cx, move |this, cx| {
                    this.use_pkcs11(Some(found.clone()), cx)
                }))
        })
        .collect();
    step.child(if c.keys.is_empty() {
        t.alt(format!("{} has no P-256 key yet.", token.label))
    } else {
        column()
            .child(format!("P-256 keys on {}:", token.label))
            .child(column().border_2().border_color(t.text).children(keys))
    })
    .child(
        div()
            .flex()
            .child(button(t, "create", "Make a new key", cx, |this, cx| this.use_pkcs11(None, cx))),
    )
}

/// A one-line field that takes typing while focused; a PIN shows as dots.
#[allow(clippy::too_many_arguments)]
fn field(
    t: Theme,
    id: &'static str,
    label: &str,
    value: &str,
    secret: bool,
    focused: bool,
    cx: &mut Context<Shell>,
    focus: impl Fn(&mut Choose, &mut Context<Shell>) + 'static,
) -> Div {
    let shown = if secret { "•".repeat(value.chars().count()) } else { value.to_string() };
    column().child(t.alt(label.to_uppercase())).child(
        clickable(id, cx, move |this, cx| {
            if let Some(c) = this.choosing() {
                focus(c, cx);
            }
            cx.notify();
        })
        .border_2()
        .border_color(if focused { t.text } else { t.alt })
        .px(chars(1.))
        .py(lines(0.5))
        .child(format!("{shown}{}", if focused { "_" } else { "" })),
    )
}
