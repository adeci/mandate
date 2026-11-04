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

/// The end of a long ID, for showing beside a chat's name.
pub fn short_id(id: &str) -> String {
    let chars: Vec<char> = id.chars().collect();
    if chars.len() > 8 {
        format!("…{}", chars[chars.len() - 6..].iter().collect::<String>())
    } else {
        id.into()
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
