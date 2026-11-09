# The app's journey against the demo server: pair by link, let an agent in by
# code, approve a read and a send, then Stop all. Each effect is checked on
# the server. gpui draws no accessibility nodes, so the app's buttons are
# found by their text (OCR), and Android's own screens through uiautomator.
# journey.nix defines sdk, apk, tesseract and magick.
import html
import itertools
import os
import re
import shlex
import subprocess
import tempfile
import time
from typing import Callable, TypeVar

T = TypeVar("T")

# The emulator runs beside the VM; its 10.0.2.2:8443 is the sandbox's
# loopback, which nix/demo.nix forwards to the server.
home = tempfile.mkdtemp()
env = dict(
    os.environ,
    HOME=home,
    ANDROID_HOME=sdk,
    ANDROID_SDK_ROOT=sdk,
    ANDROID_USER_HOME=f"{home}/.android",
    ANDROID_EMULATOR_HOME=f"{home}/.android",
    ANDROID_AVD_HOME=f"{home}/avd",
)
avd = f"{home}/avd/journey.avd"
os.makedirs(avd)
with open(f"{home}/avd/journey.ini", "w") as f:
    f.write(f"path={avd}\ntarget=android-35\n")
# A 1080x2400 phone at 420 dpi.
with open(f"{avd}/config.ini", "w") as f:
    f.write(
        "image.sysdir.1=system-images/android-35/default/x86_64/\n"
        "abi.type=x86_64\nhw.cpu.arch=x86_64\nhw.cpu.ncore=4\nhw.ramSize=2048\n"
        "hw.lcd.width=1080\nhw.lcd.height=2400\nhw.lcd.density=420\n"
        "hw.mainKeys=no\nhw.keyboard=no\nhw.sdCard=no\ndisk.dataPartition.size=4G\n"
    )
emulator = subprocess.Popen(
    [f"{sdk}/emulator/emulator", "-avd", "journey", "-port", "5554", "-no-window", "-no-audio",
     "-no-boot-anim", "-no-snapshot", "-no-metrics", "-gpu", "swiftshader_indirect"],
    env=env,
    stdout=open(f"{home}/emulator.log", "w"),
    stderr=subprocess.STDOUT,
)
start_all()


def run(*args: str, input: bytes | None = None) -> bytes:
    out = subprocess.run(args, env=env, input=input, capture_output=True, timeout=120)
    assert out.returncode == 0, (args, out.stdout[-2000:], out.stderr[-2000:])
    return out.stdout


def adb(*args: str) -> str:
    return run(f"{sdk}/platform-tools/adb", "-s", "emulator-5554", *args).decode()


def until(what: str, check: Callable[[], T | None], timeout: float = 120) -> T:
    deadline = time.monotonic() + timeout
    while True:
        got = check()
        if got:
            return got
        assert time.monotonic() < deadline, f"timed out waiting for {what}"
        time.sleep(1)


def focus() -> str:
    m = re.search(r"mCurrentFocus=(.*)", adb("shell", "dumpsys", "window"))
    return m[1] if m else ""


# Android's own screens: (text, x, y) of each node.
def nodes() -> list[tuple[str, int, int]]:
    adb("shell", "uiautomator", "dump", "/data/local/tmp/ui.xml")
    xml = adb("shell", "cat", "/data/local/tmp/ui.xml")
    found = re.findall(r'text="([^"]+)"[^>]*bounds="\[(\d+),(\d+)\]\[(\d+),(\d+)\]"', xml)
    return [(html.unescape(t), (int(a) + int(c)) // 2, (int(b) + int(d)) // 2) for t, a, b, c, d in found]


def shows(text: str) -> bool:
    return any(t == text for t, _, _ in nodes())


def press(*labels: str) -> bool:
    for text, x, y in nodes():
        if text in labels:
            adb("shell", "input", "tap", str(x), str(y))
            return True
    return False


# The app's screen: lines of (word, left, top, right, bottom), words in lower
# case without punctuation.
seen: list[str] = []


def screenshot() -> bytes:
    return run(f"{sdk}/platform-tools/adb", "-s", "emulator-5554", "exec-out", "screencap", "-p")


def lines(png: bytes | None = None, *opts: str) -> list[list[tuple[str, int, int, int, int]]]:
    tsv = run(tesseract, "stdin", "stdout", *opts, "tsv", input=png or screenshot()).decode()
    found: dict[tuple[str, ...], list[tuple[str, int, int, int, int]]] = {}
    for row in tsv.splitlines()[1:]:
        f = row.split("\t")
        word = re.sub(r"\W", "", f[-1]).lower() if len(f) == 12 else ""
        if word:
            x, y, w, h = map(int, f[6:10])
            found.setdefault(tuple(f[1:5]), []).append((word, x, y, x + w, y + h))
    seen[:] = [" ".join(w[0] for w in line) for line in found.values()]
    return list(found.values())


# The centre of the label's lowest appearance, below the label below if
# given: "Let laptop in" is also the heading above its button, and a sheet's
# buttons sit below what it covers. The page layout skips the picker's boxed
# rows, which block reads as one block of text.
def find(label: str, below: str | None = None, block: bool = False) -> tuple[int, int] | None:
    found = lines(None, "--psm", "6") if block else lines()

    def lowest(label: str) -> tuple[int, int] | None:
        want = label.lower().split()
        at = None
        for line in found:
            for i in range(len(line) - len(want) + 1):
                box = line[i : i + len(want)]
                if [w[0] for w in box] == want:
                    top, bottom = min(w[2] for w in box), max(w[4] for w in box)
                    if at is None or (top + bottom) // 2 > at[1]:
                        at = ((box[0][1] + box[-1][3]) // 2, (top + bottom) // 2)
        return at

    at = lowest(label)
    if below is not None:
        floor = lowest(below)
        if floor is None or at is None or at[1] <= floor[1]:
            return None
    return at


# Waits for the label to sit still (cards come and go as the server's news
# arrives), scrolling down if asked.
def see(label: str, scroll: bool = False, below: str | None = None, block: bool = False) -> tuple[int, int]:
    last: list[tuple[int, int]] = []

    def look() -> tuple[int, int] | None:
        at = find(label, below, block)
        if at is None and scroll:
            adb("shell", "input", "swipe", "540", "1800", "540", "800", "600")
        settled = at and last and abs(at[0] - last[0][0]) + abs(at[1] - last[0][1]) < 8
        last[:] = [at] if at else []
        return at if settled else None

    try:
        return until(f"{label!r} on screen", look)
    except AssertionError:
        raise AssertionError(f"no {label!r} on screen, which reads: {seen}")


def tap(label: str, scroll: bool = False, below: str | None = None, block: bool = False) -> None:
    x, y = see(label, scroll, below, block)
    print(f"tap {label!r} at {x},{y}; the screen reads: {seen}")
    adb("shell", "input", "tap", str(x), str(y))


# A decided card leaves Now once the app has caught up with the server.
def gone(label: str) -> None:
    until(f"{label!r} to leave the screen", lambda: find(label) is None)


# Signing asks for the fingerprint every time. The prompt's window can show
# before its sensor listens, so a touch that comes too early is repeated.
def touched() -> bool:
    adb("emu", "finger", "touch", "1")
    time.sleep(1)
    adb("emu", "finger", "remove", "1")
    for _ in range(5):
        if "BiometricPrompt" not in focus():
            return True
        time.sleep(1)
    return False


def finger() -> None:
    try:
        until("the fingerprint prompt", lambda: "BiometricPrompt" in focus(), timeout=30)
    except AssertionError:
        lines()
        log = adb("logcat", "-d", "-t", "40", "-s", "mandate:*", "AndroidRuntime:*")
        raise AssertionError(f"no fingerprint prompt; {focus()} reads: {seen}\n{log}")
    until("the fingerprint prompt to take the finger", touched, timeout=60)


def agent(cmd: str) -> tuple[int, str]:
    return server.execute(f"mandate {cmd} 2>&1")


def sent() -> list[str]:
    return server.succeed("cat /var/lib/mandate/connectors/test/private/sent.jsonl 2>/dev/null || true").splitlines()


def grab(pattern: str, text: str) -> str:
    m = re.search(pattern, text)
    assert m, (pattern, text)
    return m[1]


# Settings' fingerprint enrolment: past the introduction, then touches until
# it has the finger. The PIN goes on once the swipe lock is gone, or the
# lock screen would want it.
def unlocked() -> bool:
    adb("shell", "input", "keyevent", "KEYCODE_WAKEUP")
    adb("shell", "wm", "dismiss-keyguard")
    return "Launcher" in focus()


def introduced() -> bool:
    if shows("Touch the sensor"):
        return True
    press("MORE", "I AGREE")
    return False


def enrolled() -> bool:
    adb("emu", "finger", "touch", "1")
    time.sleep(0.5)
    adb("emu", "finger", "remove", "1")
    return shows("Fingerprint added")


with subtest("the phone boots with a PIN and a fingerprint"):
    adb("wait-for-device")
    until("the emulator to boot", lambda: adb("shell", "getprop", "sys.boot_completed").strip() == "1", timeout=600)
    until("the home screen", unlocked)
    adb("shell", "svc", "power", "stayon", "true")
    # A heads-up notification can cover the app and carry its own "Let laptop
    # in"; notifications still reach the shade.
    adb("shell", "settings", "put", "global", "heads_up_notifications_enabled", "0")
    adb("shell", "locksettings", "set-pin", "1111")
    adb("shell", "am", "start", "-a", "android.settings.FINGERPRINT_ENROLL")
    until("the PIN check", lambda: "ConfirmLock" in focus())
    adb("shell", "input", "text", "1111")
    adb("shell", "input", "keyevent", "KEYCODE_ENTER")
    until("the sensor step", introduced)
    until("the fingerprint to enrol", enrolled)
    until("DONE", lambda: press("DONE"))
    adb("install", "-g", apk)

with subtest("the app pairs by the setup page's link"):
    server.wait_for_unit("mandate.target")
    server.wait_for_open_port(8443)
    path = grab(r"Initial setup\s+https://[^/]+(/setup/\S+)", server.succeed("mandate setup"))
    # Under the emulator's name for the host, so that the link points there.
    page = server.succeed(f"curl -sk --connect-to 10.0.2.2:8443:localhost:8443 https://10.0.2.2:8443{path}")
    link = html.unescape(grab(r'href="(mandate://pair\?[^"]+)"', page))
    adb("shell", f"am start -a android.intent.action.VIEW -d {shlex.quote(link)}")
    # Pair shows once the app has checked the server.
    tap("Pair")
    # The emulator has a fingerprint, so the app asks how to approve.
    tap("Fingerprint or face")
    finger()
    # Once paired, the app asks once to run in the background.
    until("the background question", lambda: press("Allow", "ALLOW"), timeout=60)
    server.wait_until_succeeds("test $(curl -sk -o /dev/null -w '%{http_code}' https://localhost:8443/setup) = 404")
    server.wait_until_succeeds(
        "setpriv --reuid=60999 --regid=$(getent group mandate-gateway | cut -d: -f3) --clear-groups "
        "curl -s --unix-socket /run/mandate/connectors/test/socket http://mandate/keys | grep -q '\"length\":1'"
    )

# The code the app shows on Add an agent, between "Then type this code:" and
# "Copy code". Read with the rest of the page, a leading 5 in its large type
# comes out as 2 or 9, so the code alone is read again at three sizes, as
# digits only; two readings must agree, on two screenshots in a row.
def shown_code() -> str:
    last: list[str] = []

    def read(png: bytes) -> str | None:
        top = bottom = None
        for line in lines(png):
            text = " ".join(w[0] for w in line)
            if "type this code" in text:
                top = max(w[4] for w in line)
            elif text.startswith("copy code"):
                bottom = min(w[2] for w in line)
        if top is None or bottom is None or bottom <= top:
            return None
        readings = []
        for size in (50, 35, 25):
            crop = run(magick, "png:-", "-crop", f"1080x{bottom - top}+0+{top}", "+repage", "-resize", f"{size}%",
                       "-bordercolor", "white", "-border", "20", "png:-", input=png)
            digits = run(tesseract, "stdin", "stdout", "--psm", "7", "-c", "tessedit_char_whitelist=0123456789",
                         input=crop).decode()
            readings.append(re.sub(r"\s", "", digits))
        codes = [c for c in readings if re.fullmatch(r"\d{6}", c)]
        return next((c for c in codes if codes.count(c) >= 2), None)

    def look() -> str | None:
        code = read(screenshot())
        settled = code if code and last == [code] else None
        last[:] = [code] if code else []
        return settled

    try:
        return until("the code on screen", look)
    except AssertionError:
        raise AssertionError(f"no code on screen, which reads: {seen}")


with subtest("an agent gets in with the code the app shows"):
    tap("Agents")
    tap("Add an agent")
    see("type this code")
    code = shown_code()
    server.succeed(
        f"(echo {code} | mandate login https://localhost:8443 --name laptop >/tmp/login.log 2>&1;"
        " echo $? >/tmp/login.exit) >/dev/null 2>&1 &"
    )
    try:
        # The button sits below Deny and can show after the heading does.
        tap("Let laptop in", below="Deny")
    except AssertionError as e:
        raise AssertionError(f"{e}; mandate login says: {server.succeed('cat /tmp/login.log')!r}")
    finger()
    server.wait_until_succeeds("test -s /tmp/login.exit")
    assert server.succeed("cat /tmp/login.exit").strip() == "0", server.succeed("cat /tmp/login.log")
    # The gateway learns of the identity a moment after login returns.
    answers: list[tuple[int, str]] = []

    def let_in() -> bool:
        answers.append(agent("test chats"))
        return "mandate request test" in answers[-1][1]

    try:
        until("the agent to be let in", let_in, timeout=60)
    except AssertionError:
        login = server.succeed("cat /tmp/login.log")
        raise AssertionError(f"mandate test chats answers {answers[-2:]}; mandate login said {login!r}")

with subtest("an approved read reads"):
    status, out = agent("request test --chat Family --for 30m --reason 'plan dinner' --wait 0")
    assert status == 75, (status, out)
    tap("Now")
    tap("Approve 30 min")
    finger()
    server.wait_until_succeeds("mandate test chats")
    status, out = agent("test messages fam")
    assert status == 0 and "fresh family" in out, (status, out)
    gone("Approve 30 min")

with subtest("an approved send runs once"):
    status, out = agent("test send fam on my way --wait 0")
    assert status == 75, (status, out)
    proposal = out.split()[0]
    assert sent() == []
    tap("Now")
    tap("Approve and send")
    finger()
    status, out = agent(f"status {proposal} --wait 60s")
    assert status == 0 and "done" in out, (status, out)
    assert len(sent()) == 1 and "on my way" in sent()[0], sent()
    gone("Approve and send")


# The IDs a connector's chats list shows the agent.
def chat_ids(connector: str) -> list[str]:
    status, out = agent(f"{connector} chats")
    assert status == 0, (status, out)
    return sorted(line.split("\t")[0] for line in out.splitlines()[1:])


def save(name: str) -> None:
    (driver.out_dir / f"{name}.png").write_bytes(screenshot())


# Back keeps what the picker has picked and goes up a level: out of an opened
# server, then the picker, then the sheet. The sheet dims Now, which OCR then
# doesn't read, so Now's heading shows once it closes.
def close_sheet() -> None:
    for _ in range(3):
        adb("shell", "input", "keyevent", "KEYCODE_BACK")
        try:
            until("the sheet to close", lambda: find("Waiting for you"), 10)
            return
        except AssertionError:
            print(f"after Back, the screen reads: {seen}")
    raise AssertionError(f"the sheet stays open, and the screen reads: {seen}")


# The words of each line of the waiting card that see() last read, Approve's
# label left out: open grants follow under "OPEN NOW".
def waiting_card() -> list[list[str]]:
    waiting = itertools.takewhile(lambda line: not line.startswith("open"), seen)
    return [line.split() for line in waiting if "min" not in line.split()]


# The demo's discord has #general on servers "main" and "other", and a
# thread under main's (nix/testconnector/shapes.go). With the chats in its
# label, OCR can miss Approve's first letters; "30 min" is only there.
with subtest("a term that matched nothing is fixed in one place on its card"):
    status, out = agent("request discord --chat 'main / nosuch' --for 30m --reason 'catch up' --wait 0")
    assert status == 75, (status, out)
    assert agent("discord chats")[0] != 0
    tap("Now")
    see("Nothing matched")
    tap("Choose")
    # The sheet opens with the picker searching the term, and keys go to its
    # search; a tap above the sheet would close it.
    see("No chat matches")
    adb("shell", "input", "keyevent", *["KEYCODE_DEL"] * 20)
    # input text drops keys while the app catches up, so a few at a time.
    for chunk in ("main", "%s", "gen", "era", "l"):
        adb("shell", "input", "text", chunk)
        time.sleep(1)
    # The search reads "main general" above the one chat it finds. The page
    # layout skips the picker's boxed rows, so they are read as one block.
    rows: list[tuple[str, int, int, int, int]] = []

    def found() -> bool:
        rows[:] = [w for line in lines(None, "--psm", "6") for w in line if w[0] in ("general", "random", "rollout")]
        return [w[0] for w in rows] == ["general", "general"]

    try:
        until("the picker to find main's #general alone", found, 60)
    finally:
        save("seed-picker")
        print(f"the picker reads: {seen}")
    _, left, top, right, bottom = rows[-1]
    adb("shell", "input", "tap", str((left + right) // 2), str((top + bottom) // 2))
    close_sheet()
    tap("30 min")
    finger()
    server.wait_until_succeeds("mandate discord chats")
    assert chat_ids("discord") == ["main"], chat_ids("discord")
    gone("30 min")

with subtest("a term naming a server and a channel finds that one channel"):
    status, out = agent("request discord --chat 'main general' --for 30m --reason 'catch up' --wait 0")
    assert status == 75, (status, out)
    tap("Now")
    see("30 min")
    card = waiting_card()
    general = [words for words in card if "general" in words]
    # The server heads its chats, a line above them.
    has_main = any("main" in words for words in card)
    assert len(general) == 1 and has_main, f"want only main / #general on the card, which reads: {seen}"
    assert not any("other" in words or "rollout" in words for words in card), seen
    tap("30 min")
    finger()
    server.wait_until_succeeds("mandate discord chats")
    assert chat_ids("discord") == ["main"], chat_ids("discord")
    gone("30 min")
    # main's #general holds more than a page.
    status, out = agent("discord messages main")
    assert status == 0 and "next: --before" in out, (status, out)

with subtest("unticking unpicks, and only the ticks are granted"):

    def discord_grants() -> int:
        status, out = agent("access")
        assert status == 0, (status, out)
        return sum(line.split("\t")[0] == "discord" for line in out.splitlines()[1:])

    before = discord_grants()
    status, out = agent("request discord --chat 'main general' --for 30m --reason 'catch up' --wait 0")
    assert status == 75, (status, out)
    tap("Now")
    see("30 min")
    tap("Adjust")
    # PICKED holds main with #general under it; SERVERS lists main and other.
    see("Servers", block=True)
    # A server row's text ticks it, all of it; the same tap unticks it.
    tap("main", block=True)
    see("all channels", block=True)
    tap("main", block=True)
    until("all of main to be unpicked", lambda: find("all channels", block=True) is None, 30)
    # The › at the row's right end opens the server; ticking and unticking
    # #random there leaves #general alone.
    _, y = see("main", block=True)
    adb("shell", "input", "tap", "1010", str(y))
    see("random", block=True)
    tap("random", block=True)
    time.sleep(1)
    tap("random", block=True)
    time.sleep(1)
    save("seed-untick")
    close_sheet()
    see("30 min")
    card = waiting_card()
    assert sum("general" in words for words in card) == 1 and any("main" in words for words in card), seen
    assert not any("random" in words or "channels" in words for words in card), seen
    tap("30 min")
    finger()
    until("the new grant", lambda: discord_grants() == before + 1, 60)
    assert chat_ids("discord") == ["main"], chat_ids("discord")
    gone("30 min")

with subtest("a server's name asks for all of it"):
    status, out = agent("request discord --chat main --for 30m --reason 'catch up' --wait 0")
    assert status == 75, (status, out)
    tap("Now")
    see("30 min")
    see("all channels")
    card = waiting_card()
    assert not any("other" in words or "general" in words for words in card), f"want all of main on the card, which reads: {seen}"
    tap("30 min")
    finger()
    # Earlier grants on main's #general are still open, so wait for the new one.
    want = ["main", "main-random", "main-rollout"]
    try:
        until("the grant on all of main", lambda: chat_ids("discord") == want, 60)
    finally:
        print(f"discord chats: {chat_ids('discord')}")
    gone("30 min")

# The demo's fastmail files a 130-character subject and a 126-character one
# without spaces in its Inbox.
long_subject = (
    "Invoice_2026-11_ACME-Corporation_Professional-Services_Statement-of-Work_Amendment-No.3_Final_Signed_Countersigned_Archive.pdf"
)
with subtest("long names fit the request card and Activity"):
    status, out = agent(
        f"request fastmail --chat 'Quarterly planning' --chat {long_subject} --for 30m --reason 'file the invoice' --wait 0"
    )
    assert status == 75, (status, out)
    tap("Now")
    see("30 min")
    # Both long names show under their mailbox's heading, however the app
    # fits them to the width.
    see("Quarterly planning")
    see("Inbox")
    assert any(w.startswith("invoice") for line in seen for w in line.split()), seen
    save("seed-request")
    tap("30 min")
    finger()
    server.wait_until_succeeds("mandate fastmail chats")
    assert chat_ids("fastmail") == ["T00", "T01"], chat_ids("fastmail")
    gone("30 min")
    # More reads than Activity's first page holds.
    server.succeed("set -e; for i in $(seq 55); do mandate fastmail messages T00; mandate fastmail messages T01; done >/dev/null")
    tap("Activity")
    see("fastmail")
    save("seed-activity")

with subtest("Stop all closes the grant"):
    # Settings is the gear at the top bar's right end, which has no text.
    adb("shell", "input", "tap", "1002", "130")
    tap("Stop all access", scroll=True)
    see("Every grant closes")
    tap("Stop all")
    server.wait_until_fails("mandate test chats")
    status, out = agent("test chats")
    assert status != 0 and "mandate request test" in out, (status, out)
    assert len(sent()) == 1, sent()


# The screen lock's own prompt, outside the app, takes the phone's PIN. A
# fingerprint prompt may follow at once, so it only waits for the PIN to
# land.
def credential() -> None:
    until("the screen lock prompt", lambda: "dev.mandate.app" not in focus(), timeout=30)
    time.sleep(2)
    print(f"the screen lock prompt: {focus()}")
    adb("shell", "input", "text", "1111")
    adb("shell", "input", "keyevent", "66")
    time.sleep(3)


with subtest("a second way to approve takes over, and the first goes"):
    tap("Add a method", scroll=True)
    tap("Screen")  # OCR reads its "lock" as "1lock"
    credential()  # the new key proves it works
    finger()  # the fingerprint's key adds it
    see("Added")
    tap("Screen", scroll=True)
    finger()  # the ticked way lets the other take over
    see("Switched")
    tap("Remove", scroll=True)
    see("stops approving with it")
    tap("Remove")
    credential()  # the screen lock's key retires the fingerprint's
    see("Removed")
    gone("Fingerprint or face")

with subtest("an update restarts the stream, which notifies with the app closed"):
    adb("shell", "input", "keyevent", "KEYCODE_HOME")
    adb("install", "-r", "-g", apk)  # ends the app's processes

    def streaming() -> bool:
        services = adb("shell", "dumpsys", "activity", "services", "dev.mandate.app")
        return "dev.mandate.app/.Stream" in services and "isForeground=true" in services

    until("the stream service in the foreground", streaming, timeout=60)
    assert adb("shell", "pidof dev.mandate.app || true").strip() == "", "the app itself runs"
    status, out = agent("request test --chat Family --for 30m --reason 'after the update' --wait 0")
    assert status == 75, (status, out)
    until(
        "the request's notification",
        lambda: "wants to read" in adb("shell", "dumpsys", "notification", "--noredact"),
        timeout=60,
    )
    assert "dev.mandate.app" not in focus(), focus()

emulator.terminate()
emulator.wait()
