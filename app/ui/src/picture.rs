//! Chats' pictures (picker-design.md §2.9): a 20 dp square, the chat's own
//! picture in colour, fetched from the owner API when its row shows and kept
//! on disk by connector, chat and version; until then, and for a chat with
//! none, monochrome initials, or a person glyph for a name with no letters.

use crate::screens::icon;
use crate::theme::{BOLD, Theme};
use crate::{Shell, segment};
use gpui::{AnyElement, Context, Image, ImageFormat, div, img, prelude::*, px};
use mandate_core::Picture;
use std::cell::RefCell;
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

/// The pictures fetched or loading, by connector, chat and version.
pub(crate) struct Pictures {
    shown: RefCell<HashMap<Key, State>>,
    dir: PathBuf,
}

type Key = (String, String, String);

enum State {
    Loading,
    Ready(Arc<Image>),
    /// None, or none to be had now: the fallback shows.
    Missing,
}

/// A picture's side, as big as the app's icons.
const SIDE: f32 = 20.;

impl Pictures {
    pub fn new(dir: PathBuf) -> Pictures {
        Pictures { shown: RefCell::default(), dir }
    }

    /// Where a kept picture lives: names are hex, so any ID is a file name.
    fn file(&self, (connector, chat, version): &Key) -> PathBuf {
        let hex = |s: &str| crate::device::hex(s.as_bytes());
        self.dir.join(format!("{}-{}-{}.png", hex(connector), hex(chat), hex(version)))
    }
}

impl Shell {
    /// A chat's picture, by its ID and version, or the fallback for its name
    /// while it loads or when it has none.
    pub(crate) fn picture(
        &self,
        t: Theme,
        connector: &str,
        chat: &str,
        icon: Option<&str>,
        name: &str,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let fallback = || fallback(t, name).into_any_element();
        let Some(version) = icon.filter(|v| !v.is_empty()) else {
            return fallback();
        };
        let key = (connector.to_string(), chat.to_string(), version.to_string());
        let ready = match self.pictures.shown.borrow().get(&key) {
            Some(State::Ready(image)) => Some(image.clone()),
            Some(_) => return fallback(),
            None => None,
        };
        match ready {
            Some(image) => img(image).size(px(SIDE)).flex_none().into_any_element(),
            None => {
                self.load_picture(key, cx);
                fallback()
            }
        }
    }

    /// Fetches a picture once: from disk if kept, else from the owner API,
    /// keeping it for good when it is the listed version. A busy connector
    /// is asked again after the wait it names.
    fn load_picture(&self, key: Key, cx: &mut Context<Self>) {
        let Some(device) = self.device.clone() else { return };
        self.pictures.shown.borrow_mut().insert(key.clone(), State::Loading);
        let file = self.pictures.file(&key);
        let (connector, chat, version) = &key;
        let path = format!(
            "connectors/{}/icon?chat={}&v={}",
            segment(connector),
            segment(chat),
            segment(version)
        );
        cx.spawn(async move |this, cx| {
            let work = async move {
                if let Ok(bytes) = std::fs::read(&file) {
                    return Ok(Picture::Png { bytes, keep: false });
                }
                let picture = device.client.picture(&path)?;
                if let Picture::Png { bytes, keep: true } = &picture {
                    let _ = std::fs::create_dir_all(file.parent().unwrap_or(&file));
                    let _ = std::fs::write(&file, bytes);
                }
                Ok::<_, mandate_core::Error>(picture)
            };
            let fetched = cx.background_spawn(work).await;
            let wait = match &fetched {
                Ok(Picture::Busy(wait)) => Some(*wait),
                _ => None,
            };
            let state = match fetched {
                Ok(Picture::Png { bytes, .. }) => {
                    State::Ready(Arc::new(Image::from_bytes(ImageFormat::Png, bytes)))
                }
                // A busy connector leaves the row loading until the wait is over.
                Ok(Picture::Busy(_)) => State::Loading,
                // None, or out of reach: the fallback, until the app starts again.
                Ok(Picture::None) | Err(_) => State::Missing,
            };
            let shown = this.update(cx, |this, cx| {
                this.pictures.shown.borrow_mut().insert(key.clone(), state);
                cx.notify();
            });
            if let (Some(wait), Ok(())) = (wait, shown) {
                cx.background_executor().timer(wait.max(Duration::from_secs(1))).await;
                let _ = this.update(cx, |this, cx| {
                    this.pictures.shown.borrow_mut().remove(&key);
                    cx.notify();
                });
            }
        })
        .detach();
    }
}

/// A chat without a picture: up to two initials in a thin square, dim, or
/// the person glyph when its name has no letters, as a phone number.
fn fallback(t: Theme, name: &str) -> gpui::Div {
    let square = div()
        .size(px(SIDE))
        .flex_none()
        .flex()
        .items_center()
        .justify_center()
        .border_1()
        .border_color(t.alt)
        .text_color(t.alt);
    let letters = initials(name);
    if letters.is_empty() {
        return square.child(icon("user", t.alt).size(px(SIDE - 6.)));
    }
    square.font_weight(BOLD).text_size(px(SIDE * 0.45)).line_height(px(SIDE)).child(letters)
}

/// Up to two capitals: the first letters of the first two words, skipping
/// marks such as `~`, `#` and `@`. "main" gives "M", "Rust Language
/// Community" "RL", "~Sam Park" "SP", "Nix/NixOS (unofficial)" "NN".
pub(crate) fn initials(name: &str) -> String {
    name.split(|c: char| !c.is_alphanumeric())
        .filter(|w| w.chars().next().is_some_and(char::is_alphabetic))
        .take(2)
        .filter_map(|w| w.chars().next())
        .flat_map(char::to_uppercase)
        .collect()
}

#[cfg(test)]
mod tests {
    use super::initials;

    #[test]
    fn initials_as_the_design_has_them() {
        assert_eq!(initials("main"), "M");
        assert_eq!(initials("Rust Language Community"), "RL");
        assert_eq!(initials("~Sam Park"), "SP");
        assert_eq!(initials("Nix/NixOS (unofficial)"), "NN");
        assert_eq!(initials("#general"), "G");
        assert_eq!(initials("+1 555 0100"), "");
        assert_eq!(initials("🏔️ Hiking crew 🥾"), "HC");
    }
}
