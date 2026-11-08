//! The Monospace Web's look (owickstrom.github.io/the-monospace-web): one
//! monospace, a grid of lines and characters, 2px rules in the text color,
//! and emerald only for what grants access.

use gpui::{
    Div, FontWeight, Hsla, Pixels, SharedString, Window, WindowAppearance, div, prelude::*, px, rgb,
};

pub const FONT: &str = "JetBrains Mono NL";
/// What draws the emoji the monospace lacks, as outlines in the text's colour.
/// NotoEmoji-Medium.ttf is Noto Emoji 3.000's variable font instanced at
/// weight 500 with fontTools, with "m" mapped to its space glyph: gpui's
/// cosmic-text system drops any font without an "m". The monospace has
/// one, so that glyph is never drawn.
pub const EMOJI: &str = "Noto Emoji";
pub const FONTS: [&[u8]; 4] = [
    include_bytes!("../assets/JetBrainsMonoNL-Medium.ttf"),
    include_bytes!("../assets/JetBrainsMonoNL-SemiBold.ttf"),
    include_bytes!("../assets/JetBrainsMonoNL-ExtraBold.ttf"),
    include_bytes!("../assets/NotoEmoji-Medium.ttf"),
];
pub const NORMAL: FontWeight = FontWeight(500.);
pub const MEDIUM: FontWeight = FontWeight(600.);
pub const BOLD: FontWeight = FontWeight(800.);

pub const SIZE: f32 = 14.;
/// One line; vertical space comes in lines.
const LINE: f32 = 17.;
/// One character's width at SIZE.
const CH: f32 = 8.4;

pub fn lines(n: f32) -> Pixels {
    px(LINE * n)
}

pub fn chars(n: f32) -> Pixels {
    px(CH * n)
}

/// Black and white, one grey, and a few related greens, each with one role.
#[derive(Clone, Copy)]
pub struct Theme {
    pub text: Hsla,
    pub alt: Hsla,
    pub bg: Hsla,
    pub bg_alt: Hsla,
    /// Deep emerald: approving and admitting, the actions that grant.
    pub grant: Hsla,
    pub grant_hi: Hsla,
    /// Mint: text on the deep emerald.
    pub on_grant: Hsla,
    /// Teal: access open right now.
    pub open: Hsla,
    /// Sage: routines.
    pub routine: Hsla,
    /// Green-tinted slate: counts and quiet notices.
    pub info: Hsla,
    /// Red, once only: Stop all, the break-glass button.
    pub danger: Hsla,
}

impl Theme {
    pub fn of(window: &Window) -> Theme {
        let dark =
            matches!(window.appearance(), WindowAppearance::Dark | WindowAppearance::VibrantDark);
        let c = |light: u32, dark_: u32| rgb(if dark { dark_ } else { light }).into();
        Theme {
            text: c(0x000000, 0xffffff),
            alt: c(0x666666, 0xaaaaaa),
            bg: c(0xffffff, 0x000000),
            bg_alt: c(0xeeeeee, 0x111111),
            grant: rgb(0x064e3b).into(),
            grant_hi: rgb(0x2f7d63).into(),
            on_grant: rgb(0xecfdf5).into(),
            open: c(0x0f766e, 0x2a9d8f),
            routine: c(0x4d7c5f, 0x8fb39a),
            info: c(0x3f5a52, 0x9db5ad),
            danger: c(0xb91c1c, 0xef4444),
        }
    }

    pub fn alt(&self, text: impl Into<SharedString>) -> Div {
        div().text_color(self.alt).child(text.into())
    }

    /// A section heading, like the site's h2.
    pub fn heading(&self, text: &str) -> Div {
        div().mt(lines(1.)).font_weight(BOLD).child(text.to_uppercase())
    }
}

pub fn bold(text: impl Into<SharedString>) -> Div {
    div().font_weight(BOLD).child(text.into())
}
