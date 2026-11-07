# The desktop app's journey against the demo server. The first run pairs by
# the setup page's link with the CanoKey (FIDO2), lets an agent in with the
# code it shows, and approves a read and, from its notification, a send. The
# second run joins with a SoftHSM2 key (PKCS#11) and the code the first shows,
# is let in by the first, and approves a read.
# Each effect is checked on the server. gpui draws no accessibility nodes,
# so the app's buttons are found by their text (OCR).
# desktop-journey.nix defines module.
import html
import re
import shlex
import time
from typing import Callable, TypeVar

T = TypeVar("T")

PIN = "246810"
HSM_PIN = "1234"
shots = "/tmp/shots"


def until(what: str, check: Callable[[], T | None], timeout: float = 120) -> T:
    deadline = time.monotonic() + timeout
    while True:
        if (v := check()) is not None and v is not False:
            return v
        if time.monotonic() > deadline:
            raise AssertionError(f"timed out waiting for {what}")
        time.sleep(1)


def grab(pattern: str, text: str) -> str:
    m = re.search(pattern, text)
    assert m, (pattern, text)
    return m[1]


# Each run has its display, session bus, data and runtime dir.
def env(n: int) -> str:
    return (
        f"DISPLAY=:{n} DBUS_SESSION_BUS_ADDRESS=unix:path=/tmp/bus{n} XDG_DATA_HOME=/tmp/data{n} "
        f"XDG_RUNTIME_DIR=/tmp/run{n} SOFTHSM2_CONF=/etc/softhsm2.conf HOME=/root "
        "PATH=/run/current-system/sw/bin XDG_DATA_DIRS=/run/current-system/sw/share"
    )


def sh(n: int, cmd: str) -> str:
    return desktop.succeed(f"export {env(n)}; {cmd}")


def spawn(n: int, name: str, cmd: str) -> None:
    desktop.succeed(f"systemd-run --unit={name} --collect /bin/sh -c {shlex.quote(f'export {env(n)}; {cmd}')}")


# The screen: lines of (word, left, top, right, bottom), words in lower case
# without punctuation. Shots are doubled for OCR, which reads them as they
# are, and inverted in black and white, as it misses light words on dark.
seen: list[str] = []


def lines(n: int) -> list[list[tuple[str, int, int, int, int]]]:
    found: dict[tuple[str, ...], list[tuple[str, int, int, int, int]]] = {}
    sh(n, "import -window root /tmp/ocr.png")
    # A boxed one-word button ("PAIR") only reads at 3x.
    passes = [(2, ""), (2, "-colorspace gray -threshold 50%"), (2, "-colorspace gray -negate -threshold 50%"), (3, "")]
    for scale, opts in passes:
        tsv = sh(n, f"magick /tmp/ocr.png -resize {scale}00% {opts} png:- | tesseract stdin stdout tsv 2>/dev/null")
        for row in tsv.splitlines()[1:]:
            f = row.split("\t")
            word = re.sub(r"\W", "", f[-1]).lower() if len(f) == 12 else ""
            if word:
                x, y, w, h = (int(v) // scale for v in f[6:10])
                found.setdefault((str(scale), opts, *f[1:5]), []).append((word, x, y, x + w, y + h))
    seen[:] = [" ".join(w[0] for w in line) for line in found.values()]
    return list(found.values())


# The centre of the label's lowest appearance ("Pair" is also the page's
# title): words that spell it, however OCR split them ("make anew key").
def find(n: int, label: str) -> tuple[int, int] | None:
    want = re.sub(r"\W", "", label).lower()
    at = None
    for line in lines(n):
        for i in range(len(line)):
            for j in range(i + 1, len(line) + 1):
                box = line[i:j]
                spelt = "".join(w[0] for w in box)
                if spelt == want:
                    top, bottom = min(w[2] for w in box), max(w[4] for w in box)
                    if at is None or (top + bottom) // 2 > at[1]:
                        at = ((box[0][1] + box[-1][3]) // 2, (top + bottom) // 2)
                if not want.startswith(spelt):
                    break
    return at


def see(n: int, label: str) -> tuple[int, int]:
    last: list[tuple[int, int]] = []

    def look() -> tuple[int, int] | None:
        at = find(n, label)
        settled = at and last and abs(at[0] - last[0][0]) + abs(at[1] - last[0][1]) < 8
        last[:] = [at] if at else []
        return at if settled else None

    try:
        return until(f"{label!r} on screen {n}", look)
    except AssertionError:
        dump(n)
        raise AssertionError(f"no {label!r} on screen {n}, which reads: {seen}")


# The app's log and screen n into the test's log, as SCREENn lines of base64.
def dump(n: int) -> None:
    desktop.execute(f"journalctl -u app{n} -u background --no-pager | tail -n 30 >&2")
    png = sh(n, "import -window root png:- | base64 -w0")
    for i in range(0, len(png), 1000):
        print(f"SCREEN{n} {png[i : i + 1000]}")


def tap(n: int, label: str) -> None:
    x, y = see(n, label)
    sh(n, f"xdotool mousemove {x} {y} click 1")


def gone(n: int, label: str) -> None:
    until(f"{label!r} to leave screen {n}", lambda: find(n, label) is None)


def typed(n: int, text: str, enter: bool = False) -> None:
    sh(n, f"xdotool type --delay 80 {shlex.quote(text)}" + ("; xdotool key Return" if enter else ""))


# The app's window, which opens centered at 960x720.
def shot(n: int, name: str) -> None:
    time.sleep(1)
    sh(n, f"import -window root -crop 960x720+160+90 +repage {shots}/{name}.png")


# The fake portal switches the run between light and dark.
def scheme(n: int, dark: bool) -> None:
    desktop.succeed(f"echo {int(dark)} > /tmp/scheme{n}")
    time.sleep(2)


def agent(cmd: str) -> tuple[int, str]:
    return server.execute(f"mandate {cmd} 2>&1")


def sent() -> list[str]:
    return server.succeed("cat /var/lib/mandate/connectors/test/private/sent.jsonl 2>/dev/null || true").splitlines()


# Waits for cmd to succeed on the server after an action on screen n, and
# says what the screen shows if it never does.
def server_after(n: int, cmd: str, timeout: float = 90) -> None:
    try:
        until(cmd, lambda: server.execute(cmd)[0] == 0, timeout)
    except AssertionError:
        lines(n)
        desktop.execute(f"journalctl -u app{n} --no-pager | tail -n 30 >&2")
        raise AssertionError(f"{cmd!r} never succeeded; screen {n} reads: {seen}")


# The key log, as the connectors have it, holds n keys.
def keys(screen: int, n: int) -> None:
    server_after(
        screen,
        "setpriv --reuid=60999 --regid=$(getent group mandate-gateway | cut -d: -f3) --clear-groups "
        f"curl -s --unix-socket /run/mandate/connectors/test/socket http://mandate/keys | grep -q '\"length\":{n}'",
    )


# The 6-digit code an invite shows. Enlarged, as lines() reads it, a leading
# 5 in its large type comes out as 2 or 9, so lines() only finds it, and the
# code alone is read as digits at full and half size; both must agree.
def code(n: int) -> str:
    def read() -> str | None:
        boxes = [w for line in lines(n) for w in line if re.fullmatch(r"\d{6}", w[0])]
        if not boxes:
            return None
        _, left, top, right, bottom = boxes[0]
        crop = f"{right - left + 40}x{bottom - top + 20}+{left - 20}+{top - 10}"
        readings = {
            sh(
                n,
                f"magick /tmp/ocr.png -crop {crop} +repage -resize {size}% -bordercolor white -border 20 png:- "
                "| tesseract stdin stdout --psm 7 -c tessedit_char_whitelist=0123456789 2>/dev/null",
            ).strip()
            for size in (100, 50)
        }
        print(f"the code reads {readings} on screen {n}")
        got = readings.pop() if len(readings) == 1 else ""
        return got if re.fullmatch(r"\d{6}", got) else None

    return until(f"a code on screen {n}", read)


start_all()

with subtest("a session with a notification daemon, a keyring, a FIDO2 key and a token"):
    desktop.wait_for_unit("multi-user.target")
    desktop.succeed(f"mkdir -p -m 700 /tmp/run1 /tmp/run2 {shots} /var/lib/softhsm")
    for n in (1, 2):
        spawn(n, f"x{n}", f"Xvfb :{n} -screen 0 1280x900x24")
        spawn(n, f"bus{n}", f"dbus-daemon --session --nofork --address=unix:path=/tmp/bus{n}")
        desktop.wait_until_succeeds(f"test -S /tmp/bus{n} && test -S /tmp/.X11-unix/X{n}")
        spawn(n, f"portal{n}", f"fake-portal /tmp/scheme{n}")
    # Only the first run has a Secret Service; the second keeps its token in the 0600 file.
    spawn(1, "keyring", "printf pw | gnome-keyring-daemon --unlock --components=secrets --foreground")
    spawn(1, "dunst", "dunst -config /etc/dunstrc")
    spawn(1, "monitor", "dbus-monitor \"interface='org.freedesktop.Notifications',member='Notify'\" >/tmp/notify.log")
    desktop.wait_until_succeeds("ls /dev/hidraw0")
    desktop.succeed(
        "python3 -c 'from fido2.hid import CtapHidDevice; from fido2.ctap2 import Ctap2, ClientPin; "
        f"ClientPin(Ctap2(next(CtapHidDevice.list_devices()))).set_pin(\"{PIN}\")'"
    )
    sh(2, f"softhsm2-util --init-token --free --label desktop --pin {HSM_PIN} --so-pin 87654321")
    server.wait_for_unit("mandate.target")
    server.wait_for_open_port(8443)

with subtest("the first run pairs by the setup page's link, with the FIDO2 key"):
    setup = grab(r"Initial setup  (\S+)", server.succeed("mandate setup"))
    page = desktop.succeed(f"curl -sk {shlex.quote(setup)}")
    link = html.unescape(grab(r'href="(mandate://pair\?[^"]+)"', page))
    # The link goes through the x-scheme-handler, as a browser hands it on.
    sh(1, "xdg-mime query default x-scheme-handler/mandate | grep -q dev.mandate.app.desktop")
    spawn(1, "app1", f"xdg-open {shlex.quote(link)}")
    see(1, "Initial setup")
    tap(1, "Pair")
    see(1, "Security key")
    shot(1, "chooser-light")
    tap(1, "Security key")
    see(1, "Use this key")
    typed(1, PIN)
    shot(1, "chooser-fido-light")
    tap(1, "Use this key")
    see(1, "Waiting for you")
    desktop.wait_until_fails(f"curl -sk {shlex.quote(setup)} | grep -q mandate://pair")
    keys(1, 1)
    spawn(1, "background", "mandate-app --background")


def sign(n: int, pin: str | None) -> None:
    if pin is not None and find(n, "to sign") is not None:
        typed(n, pin, enter=True)


with subtest("an agent gets in by the code the app shows"):
    sh(1, "xdotool mousemove 562 785 click 1")  # the Agents tab; OCR misses its small label
    tap(1, "Add an agent")
    login = code(1)
    shot(1, "invite-agent-light")
    server.succeed(
        f"(echo {login} | mandate login https://localhost:8443 --name laptop >/tmp/login.log 2>&1;"
        " echo $? >/tmp/login.exit) >/dev/null 2>&1 &"
    )
    tap(1, "Let laptop in")
    until("the PIN prompt or the agent", lambda: find(1, "to sign") or server.execute("test -s /tmp/login.exit")[0] == 0)
    sign(1, PIN)
    server_after(1, "test $(cat /tmp/login.exit) = 0")
    status, out = agent("test chats")
    assert status != 0 and "mandate request test" in out, (status, out)

with subtest("the background run notifies of a waiting read, by identity, connector and kind"):
    status, out = agent("request test --chat Family --for 30m --reason 'plan dinner' --wait 0")
    assert status == 75, (status, out)
    desktop.wait_until_succeeds("grep -q 'laptop wants to read test' /tmp/notify.log")
    desktop.fail("grep -E 'Family|dinner' /tmp/notify.log")
    see(1, "laptop wants to read test")

# The Now tab, from a tab or from a screen pushed over one (Settings has a
# back arrow, no tabs). Neither is OCR-readable; on a tab, the back arrow's
# spot is the title, where a click does nothing.
def now(n: int) -> None:
    sh(n, "xdotool mousemove 343 115 click 1")
    time.sleep(1)
    sh(n, "xdotool mousemove 402 785 click 1")


with subtest("an approved read reads"):
    now(1)  # letting an agent in returns to Agents
    see(1, "Approve 30 min")
    shot(1, "now-light")
    scheme(1, True)
    shot(1, "now-dark")
    scheme(1, False)
    tap(1, "Approve 30 min")
    until("the PIN prompt or the grant", lambda: find(1, "to sign") or server.execute("mandate test chats")[0] == 0)
    if find(1, "to sign"):
        shot(1, "pin-light")
    sign(1, PIN)
    server_after(1, "mandate test chats")
    status, out = agent("test messages fam")
    assert status == 0 and "fresh family" in out, (status, out)
    gone(1, "Approve 30 min")

with subtest("a click on a send's notification opens it, and the send runs once"):
    status, out = agent("test send fam on my way --wait 0")
    assert status == 75, (status, out)
    proposal = out.split()[0]
    desktop.wait_until_succeeds("grep -q 'laptop wants to send on test' /tmp/notify.log")
    tap(1, "laptop wants to send")
    see(1, "Approve and send")
    shot(1, "review-light")
    scheme(1, True)
    shot(1, "review-dark")
    scheme(1, False)
    assert sent() == []
    tap(1, "Approve and send")
    until("the PIN prompt or the send", lambda: find(1, "to sign") or sent())
    sign(1, PIN)
    status, out = agent(f"status {proposal} --wait 60s")
    assert status == 0 and "done" in out, (status, out)
    assert len(sent()) == 1 and "on my way" in sent()[0], sent()

# The pairing screen's "Join as another device" link, which OCR misses for
# its underline: 20 columns right of "Already set up?".
def join_link(n: int) -> None:
    x, y = see(n, "Already set up")
    sh(n, f"xdotool mousemove {x + 168} {y} click 1")


with subtest("a second run joins with a PKCS#11 key and the code the first shows"):
    scheme(2, True)
    spawn(2, "app2", "mandate-app")
    join_link(2)
    see(2, "Server address")
    typed(2, "https://server:8443", enter=True)
    sh(1, "xdotool mousemove 938 115 click 1")  # the settings icon
    tap(1, "Add a device")
    digits = code(1)
    shot(1, "invite-device-light")
    typed(2, digits)
    shot(2, "join-dark")
    sh(2, "xdotool key Return")
    see(2, "PKCS11 token")
    shot(2, "chooser-dark")
    tap(2, "PKCS11 token")
    typed(2, module, enter=True)
    see(2, "desktop PIN")
    typed(2, HSM_PIN, enter=True)
    tap(2, "Make a new key")
    tap(1, "Let this device in")
    until("the PIN prompt or the join", lambda: find(1, "to sign") or find(2, "Waiting for you"))
    sign(1, PIN)
    see(2, "Waiting for you")
    keys(2, 2)
    # No Secret Service runs there: the token sits in the 0600 file.
    desktop.succeed("test $(stat -c %a /tmp/data2/mandate/device.json) = 600 && grep -q '\"token\"' /tmp/data2/mandate/device.json")
    desktop.fail("grep -q '\"token\"' /tmp/data1/mandate/device.json")

# Closes the open grant on Now, and waits until reads stop. OCR misses the
# small CLOSE button, two lines under the grant's "last 7 days", and the
# sheet's CLOSE, two lines under its question; a gate may want the PIN.
def close(n: int, pin: str) -> None:
    now(n)
    x, y = see(n, "last 7 days")
    sh(n, f"xdotool mousemove {x + 47} {y + 42} click 1")
    _, y = see(n, "grant on test")
    sh(n, f"xdotool mousemove 640 {y + 42} click 1")
    try:
        until(
            "the grant to close",
            lambda: server.execute("mandate test chats")[0] != 0
            or (find(n, "to confirm") is not None and typed(n, pin, enter=True)),
            60,
        )
    except AssertionError:
        dump(n)
        raise


with subtest("the joined run approves a read with its PKCS#11 key"):
    server.succeed("mandate test chats")
    close(1, PIN)
    status, out = agent("request test --chat Family --for 30m --reason 'later' --wait 0")
    assert status == 75, (status, out)
    tap(2, "Approve")  # OCR reads its "30" as "38" in dark mode
    server_after(2, "mandate test chats")
    assert len(sent()) == 1, sent()

with subtest("Settings names each run's key"):
    close(2, HSM_PIN)
    # The settings icon, top right of the page's 80 columns.
    sh(2, "xdotool mousemove 938 115 click 1")
    see(2, "PKCS11 token")
    shot(2, "settings-dark")

with subtest("the first run adds a token as a second way, ticks it, and drops its FIDO2 key"):
    sh(1, "xdotool mousemove 938 115 click 1")  # the settings icon
    tap(1, "Add a method")
    see(1, "PKCS11 token")
    assert find(1, "FIDO2") is None, "a security key is offered twice"
    tap(1, "PKCS11 token")
    typed(1, module, enter=True)
    see(1, "desktop PIN")
    typed(1, HSM_PIN, enter=True)
    tap(1, "Make a new key")
    until("the PIN prompt or the new way", lambda: find(1, "to sign") or find(1, "Added"))
    sign(1, PIN)  # the FIDO2 key signs the token's key in
    see(1, "Added")
    tap(1, "PKCS11 token desktop")
    until("the PIN prompt or the switch", lambda: find(1, "to confirm") or find(1, "Switched"))
    if find(1, "to confirm"):
        typed(1, PIN, enter=True)
    see(1, "Switched")
    desktop.succeed("grep -q token_id /tmp/data1/mandate/device.json")
    # OCR misses the small REMOVE link at the security key's row's right end:
    # clicks from the page's right edge inward until its sheet opens.
    _, y = see(1, "security key canokeysorg canokey")
    for x in range(950, 860, -10):
        sh(1, f"xdotool mousemove {x} {y} click 1")
        time.sleep(1)
        if find(1, "stops approving with it"):
            break
    # OCR misses the sheet's small REMOVE, two lines under what it says.
    _, y = see(1, "stops approving with it")
    sh(1, f"xdotool mousemove 640 {y + 42} click 1")
    until("the PIN prompt or the removal", lambda: find(1, "to sign") or find(1, "Removed"))
    sign(1, HSM_PIN)  # the token's key retires the FIDO2 key
    see(1, "Removed")
    gone(1, "Security key")
    shot(1, "methods-light")

with subtest("Connections, a Now card and Activity show each connector's mark"):
    status, out = agent("request signal --chat Hiking --for 30m --reason 'plan the hike' --wait 0")
    assert status == 75, (status, out)
    for n, mode in ((1, "light"), (2, "dark")):
        now(n)
        see(n, "laptop wants to read")
        shot(n, f"marks-now-{mode}")
        sh(n, "xdotool mousemove 722 785 click 1")  # the Connections tab
        see(n, "Discord")  # the first, by name
        shot(n, f"marks-connections-{mode}")
        sh(n, "xdotool mousemove 880 785 click 1")  # the Activity tab
        see(n, "asked to read Signal")
        shot(n, f"marks-activity-{mode}")
    desktop.copy_from_machine(shots, "")
