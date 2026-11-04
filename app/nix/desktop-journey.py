# The desktop app's journey against the demo server. The first run pairs by
# the setup page's link with the CanoKey (FIDO2), lets an agent in, and
# approves a read and, from its notification, a send. The second run joins
# with a SoftHSM2 key (PKCS#11), is let in by the first, and approves a read.
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
    sh(n, "import -window root -resize 200% /tmp/ocr.png")
    for negate in ("", "-colorspace gray -negate -threshold 50%"):
        tsv = sh(n, f"magick /tmp/ocr.png {negate} png:- | tesseract stdin stdout tsv 2>/dev/null")
        for row in tsv.splitlines()[1:]:
            f = row.split("\t")
            word = re.sub(r"\W", "", f[-1]).lower() if len(f) == 12 else ""
            if word:
                x, y, w, h = (int(v) // 2 for v in f[6:10])
                found.setdefault((negate, *f[1:5]), []).append((word, x, y, x + w, y + h))
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
        desktop.execute(f"journalctl -u app{n} -u background --no-pager | tail -n 30 >&2")
        png = sh(n, "import -window root png:- | base64 -w0")
        for i in range(0, len(png), 1000):
            print(f"SCREEN{n} {png[i : i + 1000]}")
        raise AssertionError(f"no {label!r} on screen {n}, which reads: {seen}")


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


# The 6 digits the joining run shows, one per box.
def code(n: int) -> str:
    def read() -> str | None:
        for line in lines(n):
            digits = "".join(w[0] for w in line)
            if re.fullmatch(r"\d{6}", digits):
                return digits
        return None

    return until("the join code", read)


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
    password = grab(r"Setup password: (\S+)", server.succeed("mandate setup"))
    page = desktop.succeed(f"curl -sk --data-urlencode password={password} https://server:8443/setup")
    link = html.unescape(grab(r'href="(mandate://pair\?[^"]+)"', page))
    # The link goes through the x-scheme-handler, as a browser hands it on.
    sh(1, "xdg-mime query default x-scheme-handler/mandate | grep -q dev.mandate.app.desktop")
    spawn(1, "app1", f"xdg-open {shlex.quote(link)}")
    see(1, "Pair only if")
    tap(1, "Pair")
    see(1, "Security key")
    shot(1, "chooser-light")
    tap(1, "Security key")
    see(1, "Use this key")
    typed(1, PIN)
    shot(1, "chooser-fido-light")
    tap(1, "Use this key")
    see(1, "Stop all access")
    server.wait_until_succeeds("test $(curl -sk -o /dev/null -w '%{http_code}' https://localhost:8443/setup) = 404")
    keys(1, 1)
    spawn(1, "background", "mandate-app --background")


def sign(n: int, pin: str | None) -> None:
    if pin is not None and find(n, "to sign") is not None:
        typed(n, pin, enter=True)


with subtest("an agent gets in by the code the owner types"):
    server.succeed(
        "(mandate login https://localhost:8443 --name laptop >/tmp/login.log 2>&1; echo $? >/tmp/login.exit)"
        " >/dev/null 2>&1 &"
    )
    server.wait_until_succeeds("grep -q 'code: ' /tmp/login.log")
    login = grab(r"code: (\d{6})", server.succeed("cat /tmp/login.log"))
    tap(1, "Type the code")
    see(1, "6 digits")
    typed(1, login)
    tap(1, "Let laptop in")
    until("the PIN prompt or the agent", lambda: find(1, "to sign") or server.execute("test -s /tmp/login.exit")[0] == 0)
    sign(1, PIN)
    server_after(1, "test $(cat /tmp/login.exit) = 0")

with subtest("the background run notifies of a waiting read, by identity, connector and kind"):
    status, out = agent("request test --chat Family --for 30m --reason 'plan dinner' --wait 0")
    assert status == 75, (status, out)
    desktop.wait_until_succeeds("grep -q 'laptop wants to read test' /tmp/notify.log")
    desktop.fail("grep -E 'Family|dinner' /tmp/notify.log")
    see(1, "laptop wants to read test")

with subtest("an approved read reads"):
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

with subtest("a second run joins with a PKCS#11 key, let in by the first"):
    scheme(2, True)
    spawn(2, "app2", "mandate-app")
    tap(2, "Join as another device")
    typed(2, "https://server:8443", enter=True)
    see(2, "PKCS11 token")
    shot(2, "chooser-dark")
    tap(2, "PKCS11 token")
    typed(2, module, enter=True)
    see(2, "desktop PIN")
    typed(2, HSM_PIN, enter=True)
    tap(2, "Make a new key")
    digits = code(2)
    shot(2, "joining-dark")
    tap(1, "Type the code")
    typed(1, digits)
    tap(1, "Let this device in")
    until("the PIN prompt or the join", lambda: find(1, "to sign") or find(2, "Stop all access"))
    sign(1, PIN)
    see(2, "Stop all access")
    keys(2, 2)
    # No Secret Service runs there: the token sits in the 0600 file.
    desktop.succeed("test $(stat -c %a /tmp/data2/mandate/device.json) = 600 && grep -q '\"token\"' /tmp/data2/mandate/device.json")
    desktop.fail("grep -q '\"token\"' /tmp/data1/mandate/device.json")

# Closes the open grant: OCR misses its small CLOSE button, which sits two
# lines under the grant's "last 7 days".
def close(n: int) -> None:
    x, y = see(n, "last 7 days")
    sh(n, f"xdotool mousemove {x + 47} {y + 42} click 1")


with subtest("the joined run approves a read with its PKCS#11 key"):
    server.succeed("mandate test chats")
    close(1)
    server.wait_until_fails("mandate test chats")
    status, out = agent("request test --chat Family --for 30m --reason 'later' --wait 0")
    assert status == 75, (status, out)
    tap(2, "Approve")  # OCR reads its "30" as "38" in dark mode
    server_after(2, "mandate test chats")
    assert len(sent()) == 1, sent()

with subtest("Settings names each run's key"):
    close(2)
    # The settings icon, top right of the page's 80 columns.
    sh(2, "xdotool mousemove 938 115 click 1")
    see(2, "PKCS11 token")
    shot(2, "settings-dark")
    desktop.copy_from_machine(shots, "")
