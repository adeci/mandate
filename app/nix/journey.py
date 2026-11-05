# The app's journey against the demo server: pair by link, let an agent in by
# code, approve a read and a send, then Stop all. Each effect is checked on
# the server. gpui draws no accessibility nodes, so the app's buttons are
# found by their text (OCR), and Android's own screens through uiautomator.
# journey.nix defines sdk, apk and tesseract.
import html
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


def lines() -> list[list[tuple[str, int, int, int, int]]]:
    png = run(f"{sdk}/platform-tools/adb", "-s", "emulator-5554", "exec-out", "screencap", "-p")
    tsv = run(tesseract, "stdin", "stdout", "tsv", input=png).decode()
    found: dict[tuple[str, ...], list[tuple[str, int, int, int, int]]] = {}
    for row in tsv.splitlines()[1:]:
        f = row.split("\t")
        word = re.sub(r"\W", "", f[-1]).lower() if len(f) == 12 else ""
        if word:
            x, y, w, h = map(int, f[6:10])
            found.setdefault(tuple(f[1:5]), []).append((word, x, y, x + w, y + h))
    seen[:] = [" ".join(w[0] for w in line) for line in found.values()]
    return list(found.values())


# The centre of the label's lowest appearance: "Let laptop in" is also the
# heading above its button, and a sheet's buttons sit below what it covers.
def find(label: str) -> tuple[int, int] | None:
    want = label.lower().split()
    at = None
    for line in lines():
        for i in range(len(line) - len(want) + 1):
            box = line[i : i + len(want)]
            if [w[0] for w in box] == want:
                top, bottom = min(w[2] for w in box), max(w[4] for w in box)
                if at is None or (top + bottom) // 2 > at[1]:
                    at = ((box[0][1] + box[-1][3]) // 2, (top + bottom) // 2)
    return at


# Waits for the label to sit still (cards come and go as the server's news
# arrives), scrolling down if asked.
def see(label: str, scroll: bool = False) -> tuple[int, int]:
    last: list[tuple[int, int]] = []

    def look() -> tuple[int, int] | None:
        at = find(label)
        if at is None and scroll:
            adb("shell", "input", "swipe", "540", "1800", "540", "800", "600")
        settled = at and last and abs(at[0] - last[0][0]) + abs(at[1] - last[0][1]) < 8
        last[:] = [at] if at else []
        return at if settled else None

    try:
        return until(f"{label!r} on screen", look)
    except AssertionError:
        raise AssertionError(f"no {label!r} on screen, which reads: {seen}")


def tap(label: str, scroll: bool = False) -> None:
    x, y = see(label, scroll)
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
    server.wait_until_succeeds("test $(curl -sk -o /dev/null -w '%{http_code}' https://localhost:8443/setup) = 404")
    server.wait_until_succeeds(
        "setpriv --reuid=60999 --regid=$(getent group mandate-gateway | cut -d: -f3) --clear-groups "
        "curl -s --unix-socket /run/mandate/connectors/test/socket http://mandate/keys | grep -q '\"length\":1'"
    )

# The code the app shows on Add an agent: one 6-digit word, read the same
# twice in a row.
def shown_code() -> str:
    last: list[str] = []

    def look() -> str | None:
        codes = [w[0] for line in lines() for w in line if re.fullmatch(r"\d{6}", w[0])]
        code = codes[0] if len(codes) == 1 else None
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
        tap("Let laptop in")
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

emulator.terminate()
emulator.wait()
