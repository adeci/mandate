use gpui::{
    AnyElement, App, ClickEvent, Context, ElementId, Hsla, SharedString, Window, div, prelude::*,
    rgb,
};

#[derive(Clone, Copy)]
enum State {
    Waiting,
    Approved,
    Denied,
}

struct Waiting {
    text: SharedString,
    state: State,
}

/// The Now screen, with mocked items until mandate-core feeds it.
pub struct Now {
    items: Vec<Waiting>,
}

impl Now {
    pub fn mock() -> Self {
        let item = |text: &'static str| Waiting { text: text.into(), state: State::Waiting };
        Now {
            items: vec![
                item("praxis wants to read Signal"),
                item("praxis wants to send on WhatsApp"),
            ],
        }
    }

    fn row(&self, i: usize, cx: &mut Context<Self>) -> impl IntoElement + use<> {
        let item = &self.items[i];
        let action: AnyElement = match item.state {
            State::Waiting => div()
                .flex()
                .gap_2()
                .child(button(("approve", i), "Approve", cx, move |now| {
                    now.items[i].state = State::Approved
                }))
                .child(button(("deny", i), "Deny", cx, move |now| {
                    now.items[i].state = State::Denied
                }))
                .into_any_element(),
            State::Approved => muted("Approved").into_any_element(),
            State::Denied => muted("Denied").into_any_element(),
        };
        div()
            .flex()
            .flex_col()
            .gap_2()
            .py_3()
            .border_b_1()
            .border_color(line())
            .child(item.text.clone())
            .child(action)
    }
}

impl Render for Now {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let rows: Vec<_> = (0..self.items.len()).map(|i| self.row(i, cx)).collect();
        div()
            .size_full()
            .flex()
            .flex_col()
            .p_6()
            .bg(rgb(0xffffff))
            .text_color(rgb(0x1f1f1f))
            .child(div().text_xl().pb_2().child("Now"))
            .children(rows)
    }
}

fn button(
    id: (&'static str, usize),
    label: &'static str,
    cx: &mut Context<Now>,
    f: impl Fn(&mut Now) + 'static,
) -> impl IntoElement {
    div()
        .id(ElementId::from(id))
        .px_3()
        .py_1()
        .border_1()
        .border_color(line())
        .rounded_md()
        .cursor_pointer()
        .child(label)
        .on_click(cx.listener(move |now, _: &ClickEvent, _, cx| {
            f(now);
            cx.notify();
        }))
}

fn muted(text: &'static str) -> impl IntoElement {
    div().text_color(rgb(0x707070)).child(text)
}

fn line() -> Hsla {
    rgb(0xd8d8d8).into()
}

/// Opens the one window both platforms show.
pub fn open(cx: &mut App, options: gpui::WindowOptions) {
    cx.open_window(options, |_, cx| cx.new(|_| Now::mock())).expect("open window");
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
    jni::init_platform(&app);
    let platform = jni::shared_platform().expect("platform initialised");
    gpui::Application::with_platform(platform.into_rc())
        .run(|cx| open(cx, gpui::WindowOptions::default()));
}
