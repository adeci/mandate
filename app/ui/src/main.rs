//! The Linux desktop app. `mandate-app [mandate://pair?…]` opens the window,
//! or hands the link to the one already open; `--item ID` opens it on a
//! waiting item; `--background` notifies of what waits.

use futures::StreamExt;
use gpui::{Bounds, WindowBounds, WindowOptions, px, size};
use mandate_app::desktop::{self, APP_ID, Opened};
use std::env::var_os;
use std::path::PathBuf;

fn main() {
    let dir = var_os("XDG_DATA_HOME")
        .map(PathBuf::from)
        .or_else(|| var_os("HOME").map(|h| PathBuf::from(h).join(".local/share")))
        .expect("HOME or XDG_DATA_HOME")
        .join("mandate");
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.first().is_some_and(|a| a == "--background") {
        if let Err(e) = desktop::background(&dir) {
            eprintln!("mandate: {e}");
            std::process::exit(1);
        }
        return;
    }
    let opened = Opened::from_args(&args);
    if desktop::forward(&dir, &opened) {
        return;
    }
    let mut later = desktop::listen(&dir);
    gpui_platform::application().with_assets(mandate_app::Assets).run(move |cx| {
        let bounds = Bounds::centered(None, size(px(960.), px(720.)), cx);
        let options = WindowOptions {
            window_bounds: Some(WindowBounds::Windowed(bounds)),
            app_id: Some(APP_ID.into()),
            ..Default::default()
        };
        let shell = mandate_app::open(cx, options, dir);
        cx.on_window_closed(|cx, _| cx.quit()).detach();
        take(&shell, opened, cx);
        cx.spawn(async move |cx| {
            while let Some(o) = later.next().await {
                cx.update(|cx| take(&shell, o, cx));
            }
        })
        .detach();
        cx.activate(true);
    });
}

/// What a launch asked of the app, and the window to the front.
fn take(shell: &gpui::Entity<mandate_app::Shell>, opened: Opened, cx: &mut gpui::App) {
    shell.update(cx, |shell, cx| match opened {
        Opened::Link(link) => shell.take_link(link, cx),
        Opened::Item(id) => shell.take_item(id, cx),
        Opened::Show => {}
    });
    if let Some(w) = cx.windows().first() {
        let _ = w.update(cx, |_, window, _| window.activate_window());
    }
}
