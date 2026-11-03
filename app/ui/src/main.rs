use gpui::{Bounds, WindowBounds, WindowOptions, px, size};

fn main() {
    gpui_platform::application().run(|cx| {
        let bounds = Bounds::centered(None, size(px(420.), px(640.)), cx);
        mandate_app::open(
            cx,
            WindowOptions {
                window_bounds: Some(WindowBounds::Windowed(bounds)),
                ..Default::default()
            },
        );
        cx.activate(true);
    });
}
