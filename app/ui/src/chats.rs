//! Sets of chats as the cards, Agents and Activity show them: each outermost
//! parent (a server, space or mailbox) once, with its picture, and its chats
//! beneath, one per line and indented; a chat with no parent on a line of its
//! own. A set longer than `LINES` lines folds into "+N more".

use crate::api::{Chat, Grant};
use crate::screens::{id_for, link};
use crate::theme::{MEDIUM, Theme, chars};
use crate::{Shell, words};
use gpui::{AnyElement, Context, Div, div, prelude::*};

/// The lines a folded set shows.
const LINES: usize = 8;

/// What a tick on a holder covers, by its kind: everything under it.
pub(crate) fn whole(kind: &str) -> Option<&'static str> {
    match kind {
        "guild" => Some("all channels"),
        "space" => Some("all rooms"),
        "forum" | "media" => Some("all posts"),
        _ => None,
    }
}

/// One line of a set.
enum Line<'a> {
    /// An outermost parent, or a holder granted whole.
    Head { id: &'a str, name: &'a str, icon: Option<&'a str> },
    /// A chat under the head above it, by its path below that head.
    Under { chat: &'a Chat, path: Vec<&'a str> },
    /// What a holder above covers: "all channels".
    Whole(&'static str),
    /// A chat with no parent.
    Alone(&'a Chat),
}

/// How a holder granted whole shows: under its name, or after it on the
/// same line, as a request card's one cell has room for.
#[derive(Clone, Copy, PartialEq)]
pub(crate) enum Whole {
    Beneath,
    Inline,
}

impl Shell {
    /// A grant's chats as a set, or "every chat".
    pub(crate) fn grant_chats(
        &self,
        t: Theme,
        g: &Grant,
        cols: usize,
        cx: &mut Context<Self>,
    ) -> Div {
        if g.chats.is_empty() {
            return div().child("every chat");
        }
        let chats: Vec<&Chat> = g.chat_refs.iter().collect();
        let key = format!("grant/{}", g.id);
        self.chat_set(t, &g.connector, &chats, &key, cols, Whole::Beneath, None, cx)
    }

    /// chats as a set on connector, each line at most cols columns. key
    /// tells sets apart, for unfolding one. remove, if given, puts a Remove
    /// after each chat, and after a holder granted whole, taking its ID.
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn chat_set(
        &self,
        t: Theme,
        connector: &str,
        chats: &[&Chat],
        key: &str,
        cols: usize,
        whole_shows: Whole,
        remove: Option<fn(&mut Shell, &str)>,
        cx: &mut Context<Self>,
    ) -> Div {
        let cols = cols.saturating_sub(if remove.is_some() { 7 } else { 0 });
        // Groups in the order their first chat comes.
        let mut groups: Vec<(Option<&str>, Vec<&Chat>)> = Vec::new();
        for c in chats {
            let parent = match (c.parents.first(), whole(&c.kind)) {
                (_, Some(_)) => Some(c.id.as_str()),
                (Some(p), None) => Some(p.id.as_str()),
                (None, None) => None,
            };
            match groups.iter_mut().find(|(k, _)| k.is_some() && *k == parent) {
                Some((_, cs)) => cs.push(c),
                None => groups.push((parent, vec![c])),
            }
        }
        let mut lines: Vec<Line> = Vec::new();
        for (parent, cs) in &groups {
            let Some(parent) = parent else {
                lines.extend(cs.iter().map(|c| Line::Alone(c)));
                continue;
            };
            // A holder granted whole heads its group, and covers the rest.
            if let Some(h) = cs.iter().find(|c| c.id == *parent) {
                lines.push(Line::Head { id: &h.id, name: h.own(), icon: h.icon.as_deref() });
                lines.push(Line::Whole(whole(&h.kind).unwrap_or_default()));
                continue;
            }
            let p = &cs[0].parents[0];
            lines.push(Line::Head { id: &p.id, name: &p.name, icon: p.icon.as_deref() });
            lines.extend(cs.iter().map(|c| {
                let path = c.parents[1..].iter().map(|p| p.name.as_str()).chain([c.own()]);
                Line::Under { chat: c, path: path.collect() }
            }));
        }
        let open = self.sets_open.contains(key);
        let fold = !open && lines.len() > LINES + 1;
        let hidden = if fold {
            lines[LINES..]
                .iter()
                .filter(|l| matches!(l, Line::Under { .. } | Line::Alone(_)))
                .count()
        } else {
            0
        };
        if fold {
            lines.truncate(LINES);
            // A head with nothing beneath it waits for the unfolded set.
            if matches!(lines.last(), Some(Line::Head { .. })) {
                lines.pop();
            }
        }
        // Lines named alike under one head tell themselves apart by their
        // IDs' ends.
        let mut head = "";
        let labels: Vec<String> = lines
            .iter()
            .map(|l| match l {
                Line::Head { id, .. } => {
                    head = id;
                    String::new()
                }
                Line::Under { path, .. } => format!("{head}\n{}", path.join(" / ")),
                Line::Alone(c) => c.own().to_string(),
                Line::Whole(_) => String::new(),
            })
            .collect();
        let pictures = connector != "fastmail";
        let mut rows: Vec<AnyElement> = Vec::new();
        let mut inline = false;
        for (i, line) in lines.iter().enumerate() {
            let twin =
                !labels[i].is_empty() && labels.iter().filter(|l| **l == labels[i]).count() > 1;
            // A holder's breadth already said on its own line.
            if matches!(line, Line::Whole(_)) && std::mem::take(&mut inline) {
                continue;
            }
            let removes = match line {
                Line::Head { id, .. } if matches!(lines.get(i + 1), Some(Line::Whole(_))) => {
                    Some(id.to_string())
                }
                Line::Under { chat, .. } => Some(chat.id.clone()),
                Line::Alone(c) => Some(c.id.clone()),
                _ => None,
            };
            let row = match line {
                Line::Head { id, name, icon } => {
                    let next = lines.get(i + 1);
                    let said = match next {
                        Some(Line::Whole(w)) if whole_shows == Whole::Inline => {
                            inline = true;
                            Some(*w)
                        }
                        _ => None,
                    };
                    let room = cols.saturating_sub(if pictures { 3 } else { 0 });
                    let room = room.saturating_sub(said.map_or(0, |w| w.len() + 3));
                    div()
                        .flex()
                        .items_center()
                        .gap(chars(1.))
                        .when(pictures, |d| {
                            d.child(self.picture(t, connector, id, *icon, name, cx))
                        })
                        .child(div().min_w_0().font_weight(MEDIUM).child(words::fit(name, room)))
                        .children(said.map(|w| div().font_weight(MEDIUM).child(format!("· {w}"))))
                }
                Line::Whole(w) => div().pl(chars(2.)).child(*w),
                Line::Under { chat, path } => {
                    let room = cols.saturating_sub(2 + if twin { 9 } else { 0 });
                    let (own, above) = path.split_last().unwrap_or((&"", &[]));
                    div()
                        .flex()
                        .gap_x(chars(1.))
                        .pl(chars(2.))
                        .child(div().min_w_0().child(words::fit_path(above, own, room)))
                        .when(twin, |d| d.child(t.alt(words::short_id(&chat.id))))
                }
                Line::Alone(c) => {
                    let room = cols
                        .saturating_sub(if pictures { 3 } else { 0 } + if twin { 9 } else { 0 });
                    div()
                        .flex()
                        .items_center()
                        .gap(chars(1.))
                        .when(pictures, |d| {
                            d.child(self.picture(
                                t,
                                connector,
                                &c.id,
                                c.icon.as_deref(),
                                c.own(),
                                cx,
                            ))
                        })
                        .child(div().min_w_0().child(words::fit(c.own(), room)))
                        .when(twin, |d| d.child(t.alt(words::short_id(&c.id))))
                }
            };
            let row = match (remove, removes) {
                (Some(f), Some(id)) => {
                    div().flex().items_center().gap(chars(1.)).child(row.flex_1().min_w_0()).child(
                        link(t, id_for("unpick", &id), "Remove", cx, move |this, cx| {
                            f(this, &id);
                            cx.notify();
                        }),
                    )
                }
                _ => row,
            };
            rows.push(row.into_any_element());
        }
        let key = key.to_string();
        let more = (hidden > 0).then(|| {
            link(t, id_for("more-chats", &key), &format!("+{hidden} more"), cx, move |this, cx| {
                this.sets_open.insert(key.clone());
                cx.notify();
            })
        });
        div().flex().flex_col().children(rows).children(more)
    }
}

#[cfg(test)]
mod tests {
    use super::whole;

    #[test]
    fn holders_say_what_they_cover() {
        assert_eq!(whole("guild"), Some("all channels"));
        assert_eq!(whole("space"), Some("all rooms"));
        assert_eq!(whole("forum"), Some("all posts"));
        assert_eq!(whole("channel"), None);
    }
}
