mod device;
mod screens;

use device::{Device, Exchange, Now, Proposal, Request};
use futures::StreamExt;
use gpui::{App, AppContext, Context, FocusHandle, KeyDownEvent, Window, WindowOptions};
use mandate_core::{Envelope, Error, PairLink};
use serde_json::{Value, json};
use std::path::PathBuf;
use std::time::Duration;

enum Screen {
    Pair { text: String },
    Now,
    Code { exchange: Exchange, code: String },
    Admit { exchange: Exchange, envelope: Envelope },
    Request { request: Request, envelope: Envelope },
    Proposal { proposal: Proposal, envelope: Envelope },
}

pub struct Shell {
    dir: PathBuf,
    device: Option<Device>,
    screen: Screen,
    now: Now,
    exchanges: Vec<Exchange>,
    error: Option<String>,
    focus: FocusHandle,
}

impl Shell {
    fn new(dir: PathBuf, cx: &mut Context<Self>) -> Self {
        let device = Device::load(&dir);
        let screen =
            if device.is_some() { Screen::Now } else { Screen::Pair { text: String::new() } };
        let mut shell = Shell {
            dir,
            device,
            screen,
            now: Now::default(),
            exchanges: Vec::new(),
            error: None,
            focus: cx.focus_handle(),
        };
        shell.listen(cx);
        shell
    }

    /// The paired device; only the pairing screen runs without one.
    fn device(&self) -> Device {
        self.device.clone().expect("paired")
    }

    /// Runs a blocking call off the UI thread, then done with what it
    /// answered; an error shows instead.
    fn call<T: Send + 'static>(
        &mut self,
        cx: &mut Context<Self>,
        work: impl FnOnce() -> Result<T, Error> + Send + 'static,
        done: impl FnOnce(&mut Self, T, &mut Context<Self>) + 'static,
    ) {
        cx.spawn(async move |this, cx| {
            let result = cx.background_spawn(async move { work() }).await;
            this.update(cx, |this, cx| {
                match result {
                    Ok(v) => done(this, v, cx),
                    Err(e) => this.error = Some(e.to_string()),
                }
                cx.notify();
            })
        })
        .detach();
    }

    /// Holds the event stream open on its own thread, refetching on
    /// connecting and on every event.
    fn listen(&mut self, cx: &mut Context<Self>) {
        let Some(device) = self.device.clone() else { return };
        let (tx, mut rx) = futures::channel::mpsc::unbounded();
        std::thread::spawn(move || {
            while !tx.is_closed() {
                match device.client.events() {
                    Ok(events) => {
                        let _ = tx.unbounded_send(None);
                        for event in events {
                            if event.is_err() || tx.unbounded_send(None).is_err() {
                                break;
                            }
                        }
                    }
                    Err(e) => {
                        let _ = tx.unbounded_send(Some(e.to_string()));
                    }
                }
                std::thread::sleep(Duration::from_secs(5));
            }
        });
        cx.spawn(async move |this, cx| {
            while let Some(error) = rx.next().await {
                let update = this.update(cx, |this, cx| match error {
                    Some(e) => {
                        this.error = Some(e);
                        cx.notify();
                    }
                    None => this.refresh(cx),
                });
                if update.is_err() {
                    break;
                }
            }
        })
        .detach();
    }

    fn refresh(&mut self, cx: &mut Context<Self>) {
        let client = self.device().client;
        self.call(
            cx,
            move || Ok((client.now(None, None)?, client.get("exchanges")?)),
            |this, (now, exchanges), _| {
                this.now = now;
                this.exchanges = exchanges;
            },
        );
    }

    fn pair(&mut self, link: PairLink, cx: &mut Context<Self>) {
        let dir = self.dir.clone();
        self.error = None;
        self.call(
            cx,
            move || Device::pair(&dir, &link),
            |this, device, cx| {
                this.device = Some(device);
                this.screen = Screen::Now;
                this.listen(cx);
            },
        );
    }

    fn back(&mut self) {
        self.error = None;
        self.screen = Screen::Now;
    }

    /// Shows what a challenge route answers, for the owner to sign or not.
    fn challenge(
        &mut self,
        path: String,
        body: Value,
        cx: &mut Context<Self>,
        screen: impl FnOnce(Envelope) -> Screen + 'static,
    ) {
        let client = self.device().client;
        self.error = None;
        self.call(
            cx,
            move || client.challenge(&path, &body),
            |this, envelope, _| {
                this.screen = screen(envelope);
            },
        );
    }

    fn open_request(&mut self, request: Request, cx: &mut Context<Self>) {
        let path = format!("requests/{}/challenge", request.id);
        self.challenge(path, json!({}), cx, |envelope| Screen::Request { request, envelope });
    }

    fn open_proposal(&mut self, proposal: Proposal, cx: &mut Context<Self>) {
        let path = format!("proposals/{}/challenge", proposal.id);
        self.challenge(path, json!({"edits": {}}), cx, |envelope| Screen::Proposal {
            proposal,
            envelope,
        });
    }

    fn approve(&mut self, mut envelope: Envelope, cx: &mut Context<Self>) {
        let device = self.device();
        self.error = None;
        self.call(
            cx,
            move || {
                envelope.sign(&*device.key)?;
                device.client.signed(&envelope)
            },
            |this, _, cx| {
                this.screen = Screen::Now;
                this.refresh(cx);
            },
        );
    }

    /// POSTs a route that answers {}, such as a deny, then goes back to Now.
    fn post(&mut self, path: String, cx: &mut Context<Self>) {
        let client = self.device().client;
        self.error = None;
        self.call(
            cx,
            move || client.send::<Value>("POST", &path, &json!({})),
            |this, _, cx| {
                this.screen = Screen::Now;
                this.refresh(cx);
            },
        );
    }

    fn digit(&mut self, d: char, cx: &mut Context<Self>) {
        let Screen::Code { exchange, code } = &mut self.screen else { return };
        if code.len() == 6 {
            code.clear();
        }
        code.push(d);
        if code.len() < 6 {
            return;
        }
        let (id, code, name) = (exchange.id.clone(), code.clone(), exchange.name.clone());
        let client = self.device().client;
        self.error = None;
        self.call(
            cx,
            move || {
                let payload = client.exchange_code(&id, &code)?;
                let envelope =
                    client.challenge(&format!("exchanges/{id}/challenge"), &json!({}))?;
                if !payload.matches(&envelope.record()?) {
                    return Err(Error::Invalid(format!(
                        "the record to sign does not let {name} in"
                    )));
                }
                Ok(envelope)
            },
            |this, envelope, _| {
                if let Screen::Code { exchange, .. } = &this.screen {
                    this.screen = Screen::Admit { exchange: exchange.clone(), envelope };
                }
            },
        );
    }

    fn key(&mut self, ev: &KeyDownEvent, _: &mut Window, cx: &mut Context<Self>) {
        let k = &ev.keystroke;
        match &mut self.screen {
            Screen::Pair { text } if k.modifiers.secondary() && k.key == "v" => {
                let pasted = cx.read_from_clipboard().and_then(|c| c.text());
                text.push_str(pasted.unwrap_or_default().trim());
            }
            Screen::Pair { text } if k.key == "backspace" => {
                text.pop();
            }
            Screen::Pair { text } => text.push_str(k.key_char.as_deref().unwrap_or_default()),
            Screen::Code { code, .. } if k.key == "backspace" => {
                code.pop();
            }
            Screen::Code { .. } => {
                if let Some(d) = k.key_char.as_deref().and_then(|c| c.chars().next())
                    && d.is_ascii_digit()
                {
                    self.digit(d, cx);
                }
            }
            _ => return,
        }
        cx.notify();
    }
}

/// Opens the app's one window; dir keeps the paired device.
pub fn open(cx: &mut App, options: WindowOptions, dir: PathBuf) {
    cx.open_window(options, |window, cx| {
        let shell = cx.new(|cx| Shell::new(dir, cx));
        let focus = shell.read(cx).focus.clone();
        window.focus(&focus, cx);
        shell
    })
    .expect("open window");
}

#[cfg(target_os = "android")]
#[unsafe(no_mangle)]
fn android_main(app: android_activity::AndroidApp) {
    use gpui_mobile::android::jni;
    android_logger::init_once(
        android_logger::Config::default()
            .with_max_level(log::LevelFilter::Info)
            .with_tag("mandate"),
    );
    jni::install_panic_hook();
    let dir = app.internal_data_path().expect("an app data dir");
    jni::init_platform(&app);
    let platform = jni::shared_platform().expect("platform initialised");
    gpui::Application::with_platform(platform.into_rc())
        .run(move |cx| open(cx, WindowOptions::default(), dir));
}
