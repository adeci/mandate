use gpui::{Bounds, WindowBounds, WindowOptions, px, size};
use std::env::var_os;
use std::path::PathBuf;

fn main() {
    let dir = var_os("XDG_DATA_HOME")
        .map(PathBuf::from)
        .or_else(|| var_os("HOME").map(|h| PathBuf::from(h).join(".local/share")))
        .expect("HOME or XDG_DATA_HOME")
        .join("mandate");
    gpui_platform::application().with_assets(mandate_app::Assets).run(|cx| {
        let bounds = Bounds::centered(None, size(px(960.), px(720.)), cx);
        let options = WindowOptions {
            window_bounds: Some(WindowBounds::Windowed(bounds)),
            ..Default::default()
        };
        mandate_app::open(cx, options, dir);
        cx.activate(true);
    });
}
