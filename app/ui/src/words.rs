//! The owner's words for durations, days and times, as the console said
//! them; times in the device's local zone and its 12- or 24-hour clock.

use mandate_core::Time;
#[cfg(target_os = "android")]
use std::sync::atomic::{AtomicBool, Ordering::Relaxed};

/// Whether the phone's clock reads 24-hour; Main.java sets it on resume.
#[cfg(target_os = "android")]
pub static H24: AtomicBool = AtomicBool::new(true);

#[cfg(target_os = "android")]
fn h24() -> bool {
    H24.load(Relaxed)
}

/// Whether LC_TIME's time format is 24-hour, read without changing the
/// process's locale.
#[cfg(not(target_os = "android"))]
fn h24() -> bool {
    static H24: std::sync::LazyLock<bool> = std::sync::LazyLock::new(|| {
        // SAFETY: the locale is made, read and freed here; nl_langinfo_l's
        // string lives until freelocale.
        unsafe {
            let locale = libc::newlocale(libc::LC_TIME_MASK, c"".as_ptr(), std::ptr::null_mut());
            if locale.is_null() {
                return true;
            }
            let format = std::ffi::CStr::from_ptr(libc::nl_langinfo_l(libc::T_FMT, locale));
            let twelve =
                [b"%I", b"%l", b"%r"].iter().any(|f| format.to_bytes().windows(2).any(|w| w == *f));
            libc::freelocale(locale);
            !twelve
        }
    });
    *H24
}

/// A time of day: "22:49", or "10:49 PM" on a 12-hour clock.
fn hm(h: i64, m: i64) -> String {
    if h24() {
        format!("{h:02}:{m:02}")
    } else {
        format!("{}:{m:02} {}", (h + 11) % 12 + 1, if h < 12 { "AM" } else { "PM" })
    }
}

/// A routine's "HH:MM" start, on the device's clock.
pub fn time_of_day(hhmm: &str) -> String {
    let parsed = hhmm.split_once(':').and_then(|(h, m)| Some(hm(h.parse().ok()?, m.parse().ok()?)));
    parsed.unwrap_or_else(|| hhmm.into())
}

/// The local hour and minute of a Unix time.
fn local_hm(unix: i64) -> (i64, i64) {
    let s = strftime(unix, c"%H %M");
    let mut parts = s.split(' ').filter_map(|p| p.parse().ok());
    (parts.next().unwrap_or(0), parts.next().unwrap_or(0))
}

pub fn span(secs: u64) -> String {
    let unit =
        |n: u64, name: &str| if n == 1 { format!("1 {name}") } else { format!("{n} {name}s") };
    match secs {
        s if s > 0 && s % 86400 == 0 => unit(s / 86400, "day"),
        s if s > 0 && s % 3600 == 0 => unit(s / 3600, "hour"),
        s => format!("{} min", s.div_ceil(60)),
    }
}

pub fn horizon(secs: u64) -> String {
    match secs {
        0 => "all history".into(),
        86400 => "last day".into(),
        s => format!("last {}", span(s)),
    }
}

pub fn days(days: &[u8]) -> String {
    const NAMES: [&str; 7] = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
    match days {
        [] | [0, 1, 2, 3, 4, 5, 6] => "every day".into(),
        [1, 2, 3, 4, 5] => "weekdays".into(),
        [0, 6] => "weekends".into(),
        d => d.iter().map(|&d| NAMES[usize::from(d) % 7]).collect::<Vec<_>>().join(", "),
    }
}

pub fn connector(name: &str) -> String {
    let known = [
        ("whatsapp", "WhatsApp"),
        ("signal", "Signal"),
        ("discord", "Discord"),
        ("fastmail", "Fastmail"),
        ("gmessages", "Google Messages"),
        ("matrix", "Matrix"),
    ];
    known.iter().find(|(k, _)| *k == name).map_or(name, |(_, v)| v).to_string()
}

/// The bundled mark of a connector the app knows, else the plug.
pub fn connector_icon(name: &str) -> &'static str {
    match name {
        "whatsapp" => "whatsapp",
        "signal" => "signal",
        "discord" => "discord",
        "fastmail" => "fastmail",
        "gmessages" => "gmessages",
        "matrix" => "matrix",
        _ => "plug",
    }
}

/// The end of a long ID, for showing beside a chat's name.
pub fn short_id(id: &str) -> String {
    let chars: Vec<char> = id.chars().collect();
    if chars.len() > 8 {
        format!("…{}", chars[chars.len() - 6..].iter().collect::<String>())
    } else {
        id.into()
    }
}

/// How many characters a one-line chat name may take on this screen: its
/// width, less a card's padding, rules and a checkbox. Set every frame.
static CHAT_COLS: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(36);

/// Takes the window's width in pixels, for `chat`.
pub fn set_width(width: f32) {
    let cols = (width / f32::from(crate::theme::chars(1.))) as usize;
    CHAT_COLS.store(cols.min(80).saturating_sub(10).max(16), std::sync::atomic::Ordering::Relaxed);
}

/// A chat's name on one line of this screen; see `fit`.
pub fn chat(name: &str) -> String {
    fit(name, CHAT_COLS.load(std::sync::atomic::Ordering::Relaxed))
}

/// A chat's name in a card's table, whose value column is about eight
/// characters narrower than a one-line row.
pub fn chat_narrow(name: &str) -> String {
    let cols = CHAT_COLS.load(std::sync::atomic::Ordering::Relaxed);
    fit(name, cols.saturating_sub(8).max(12))
}

/// How many columns a character takes: an emoji, drawn by Noto Emoji for
/// what the monospace lacks, is about two; what joins or varies one, none.
fn columns(c: char) -> usize {
    match u32::from(c) {
        0x200D | 0x20E3 | 0xFE00..=0xFE0F | 0x1F3FB..=0x1F3FF | 0xE0020..=0xE007F => 0,
        0x231A..=0x23FF | 0x25FB..=0x25FE | 0x2600..=0x27BF | 0x2B00..=0x2BFF => 2,
        0x1F000..=0x1F1E5 | 0x1F200..=0x1FAFF => 2,
        _ => 1,
    }
}

/// How many columns s takes on screen.
pub fn width(s: &str) -> usize {
    s.chars().map(columns).sum()
}

/// s in at most k columns, ending in "…" when cut.
fn cut(s: &str, k: usize) -> String {
    if width(s) <= k {
        return s.to_string();
    }
    let mut used = 0;
    let kept: String = s
        .chars()
        .take_while(|&c| {
            used += columns(c);
            used < k
        })
        .collect();
    format!("{kept}…")
}

/// name in at most n columns. A "Parent / Child" name, such as a
/// Discord "server / channel" or a mailbox's thread, keeps both: the parent
/// gives way first, down to two columns if that keeps the child whole,
/// else to six, and the child keeps the rest. What is cut ends in "…".
pub fn fit(name: &str, n: usize) -> String {
    if width(name) <= n {
        return name.into();
    }
    let Some((parent, child)) = name.split_once(" / ") else { return cut(name, n) };
    let keep = if width(child) + 5 <= n { 2 } else { 6 };
    let child = cut(child, n.saturating_sub(3 + width(parent).min(keep)));
    let parent = cut(parent, n.saturating_sub(3 + width(&child)));
    format!("{parent} / {child}")
}

/// The columns `chat` fits a name to on this screen.
pub fn chat_cols() -> usize {
    CHAT_COLS.load(std::sync::atomic::Ordering::Relaxed)
}

/// A path, its parents outermost first and then the chat's own name, in at
/// most n columns. The outer parents give way first, each to "…", then
/// the nearest, down to two columns if that keeps the own name whole,
/// else to six, then the own name: "… / #releases / v2 rollout",
/// "Recei… / Your order has ship…".
pub fn fit_path(parents: &[&str], own: &str, n: usize) -> String {
    let join = |ps: &[String], own: &str| {
        ps.iter().map(String::as_str).chain([own]).collect::<Vec<_>>().join(" / ")
    };
    let mut ps: Vec<String> = parents.iter().map(|p| p.to_string()).collect();
    for i in 0..ps.len().saturating_sub(1) {
        if width(&join(&ps, own)) <= n {
            break;
        }
        ps[i] = "…".into();
    }
    ps.dedup_by(|a, b| a == "…" && b == "…");
    let whole = join(&ps, own);
    let Some(nearest) = ps.pop() else { return cut(own, n) };
    if width(&whole) <= n {
        return whole;
    }
    let outer = ps.iter().map(|p| width(p) + 3).sum::<usize>();
    let room = n.saturating_sub(outer + 3 + width(own));
    let keep = if room >= 2 { 2 } else { 6 };
    let nearest = cut(&nearest, room.max(keep.min(width(&nearest))));
    let own = cut(own, n.saturating_sub(outer + 3 + width(&nearest)));
    ps.push(nearest);
    join(&ps, &own)
}

/// When a chat was last active, in four columns or fewer: `now`, `9m`,
/// `5h`, `3d`, `2w`, `Sep` within a year, else `2024`.
pub fn age(at: &str) -> String {
    // The listing's times may carry fractions of a second, which `unix`
    // doesn't take.
    let whole = match at.find('.') {
        Some(dot) => {
            let end =
                at[dot + 1..].find(|c: char| !c.is_ascii_digit()).map_or(at.len(), |e| dot + 1 + e);
            format!("{}{}", &at[..dot], &at[end..])
        }
        None => at.to_string(),
    };
    let Some(then) = unix(&whole) else { return String::new() };
    let ago = (Time::now().unix() - then).max(0);
    match ago {
        0..60 => "now".into(),
        60..3600 => format!("{}m", ago / 60),
        3600..86400 => format!("{}h", ago / 3600),
        86400..604800 => format!("{}d", ago / 86400),
        604800..2592000 => format!("{}w", ago / 604800),
        2592000..31536000 => month_short(then),
        _ => strftime(then, c"%Y"),
    }
}

pub fn unix(t: &str) -> Option<i64> {
    t.parse::<Time>().ok().map(Time::unix)
}

/// The local time of day of t.
pub fn clock(t: &str) -> String {
    unix(t).map(local_hm).map(|(h, m)| hm(h, m)).unwrap_or_default()
}

/// "3 Nov", in local time.
pub fn date(t: &str) -> String {
    unix(t).map(day).unwrap_or_default()
}

/// The local day of a Unix time, as date says it.
pub fn day(unix: i64) -> String {
    strftime(unix, c"%e %b").trim_start().to_string()
}

/// The local calendar date of a Unix time: year, month 1–12, day.
pub fn ymd(unix: i64) -> (i32, u32, u32) {
    let t = unix as libc::time_t;
    // SAFETY: tm is plain data that localtime_r fills.
    let tm = unsafe {
        let mut tm: libc::tm = std::mem::zeroed();
        libc::localtime_r(&t, &mut tm);
        tm
    };
    (tm.tm_year + 1900, tm.tm_mon as u32 + 1, tm.tm_mday as u32)
}

/// The start of a local day, as a Unix time. Months and days out of range
/// roll over, so day 0 is the last day of the month before.
pub fn midnight(year: i32, month: i32, day: i32) -> i64 {
    // SAFETY: mktime reads and normalises a tm it is given; -1 for isdst
    // lets it find whether summer time applies.
    unsafe {
        let mut tm: libc::tm = std::mem::zeroed();
        (tm.tm_year, tm.tm_mon, tm.tm_mday, tm.tm_isdst) = (year - 1900, month - 1, day, -1);
        libc::mktime(&mut tm) as i64
    }
}

/// "November 2026", in local time.
pub fn month(unix: i64) -> String {
    strftime(unix, c"%B %Y")
}

/// The weekday of a Unix time in local time, Monday 0 to Sunday 6.
pub fn weekday(unix: i64) -> u32 {
    (strftime(unix, c"%u").parse::<u32>().unwrap_or(1) + 6) % 7
}

/// "2026-11-03", the local date of a Unix time.
pub fn iso_day(unix: i64) -> String {
    strftime(unix, c"%Y-%m-%d")
}

/// "Nov", a Unix time's month, short, in local time.
pub fn month_short(unix: i64) -> String {
    strftime(unix, c"%b")
}

/// When a routine runs right now in local time, the clock time it ends.
pub fn running(s: &crate::api::Schedule) -> Option<String> {
    let now = Time::now().unix();
    let parts: Vec<i64> =
        strftime(now, c"%w %H %M").split(' ').filter_map(|p| p.parse().ok()).collect();
    let [day, h, m] = parts[..] else { return None };
    let (sh, sm) = s.start.split_once(':')?;
    let start = sh.parse::<i64>().ok()? * 60 + sm.parse::<i64>().ok()?;
    let minute = h * 60 + m;
    let today = s.days.is_empty() || s.days.contains(&(day as u8));
    let end = start + s.length.div_ceil(60) as i64;
    (today && (start..end).contains(&minute)).then(|| {
        let (h, m) = local_hm(now + (end - minute) * 60);
        hm(h, m)
    })
}

/// "39m left" or "2h 5m left" until t.
pub fn left(t: &str) -> String {
    let secs = unix(t).unwrap_or(0) - Time::now().unix();
    match secs.max(0) / 60 {
        m if m >= 60 => format!("{}h {}m left", m / 60, m % 60),
        m => format!("{m}m left"),
    }
}

fn strftime(unix: i64, format: &std::ffi::CStr) -> String {
    let t = unix as libc::time_t;
    let mut buf = [0u8; 32];
    // SAFETY: tm is plain data that localtime_r fills; strftime writes at
    // most buf.len() bytes and returns how many.
    let n = unsafe {
        let mut tm: libc::tm = std::mem::zeroed();
        libc::localtime_r(&t, &mut tm);
        libc::strftime(buf.as_mut_ptr().cast(), buf.len(), format.as_ptr(), &tm)
    };
    String::from_utf8_lossy(&buf[..n]).into_owned()
}

/// The device's IANA zone, which a routine's grant runs in.
pub fn zone() -> String {
    if let Ok(tz) = std::env::var("TZ") {
        return tz.trim_start_matches(':').into();
    }
    #[cfg(target_os = "android")]
    {
        let mut buf = [0u8; 92]; // PROP_VALUE_MAX
        // SAFETY: the property value is at most PROP_VALUE_MAX bytes.
        let n = unsafe {
            libc::__system_property_get(c"persist.sys.timezone".as_ptr(), buf.as_mut_ptr().cast())
        };
        if n > 0 {
            return String::from_utf8_lossy(&buf[..n as usize]).into_owned();
        }
    }
    std::fs::read_link("/etc/localtime")
        .ok()
        .and_then(|p| p.to_str()?.split_once("zoneinfo/").map(|(_, z)| z.to_string()))
        .unwrap_or_else(|| "UTC".into())
}

#[cfg(test)]
mod tests {
    use super::{fit, fit_path, width};

    #[test]
    fn fit_keeps_both_parts() {
        assert_eq!(fit("Book club", 20), "Book club");
        assert_eq!(fit(&"x".repeat(30), 10), format!("{}…", "x".repeat(9)));
        let long = format!("{} / {}", "Inbox".repeat(10), "rewindvm selected for the corpus hub");
        let short = fit(&long, 30);
        assert_eq!(short.chars().count(), 30);
        assert!(short.starts_with("Inbox…") && short.contains(" / rewindvm"));
        // A short parent stays whole; the child gives way.
        let both = fit(&format!("main / {}", "general".repeat(10)), 20);
        assert_eq!((both.chars().count(), &both[..7]), (20, "main / "));
        // The parent gives way further if that keeps the child whole.
        assert_eq!(fit("Inbox / Weekly update 5", 22), "Inb… / Weekly update 5");
    }

    #[test]
    fn emoji_take_two_columns() {
        assert_eq!(width("🎉 Party photos"), 15);
        // The variation selector and the box drawing take none and one.
        assert_eq!(width("🏔️ Hiking crew 🥾"), 17);
        assert_eq!(width("#💬│general"), 11);
        assert_eq!(fit("Mum ❤️ and the 🏔️ crew", 10), "Mum ❤️ an…");
        // A wide emoji that would overrun is left out whole.
        assert_eq!(fit("abcdefgh🎉", 9), "abcdefgh…");
        assert_eq!(fit_path(&["Trash"], "🎉 Party photos", 20), "T… / 🎉 Party photos");
    }

    #[test]
    fn fit_path_cuts_outer_parents_first() {
        assert_eq!(fit_path(&["main"], "#general", 30), "main / #general");
        assert_eq!(fit_path(&[], "Book club", 4), "Boo…");
        // The outer parents become "…" before the nearest gives way.
        assert_eq!(
            fit_path(&["main", "#releases"], "v2 rollout", 26),
            "… / #releases / v2 rollout"
        );
        // Then the nearest, down to six, then the own name.
        let cut = fit_path(&["Receipts"], "Your order has shipped today", 26);
        assert_eq!(cut.chars().count(), 26);
        assert!(cut.starts_with("Recei… / Your order"), "{cut}");
        // A subject with " / " in it stays the own name.
        assert_eq!(fit_path(&["Inbox"], "a / b", 20), "Inbox / a / b");
    }
}
