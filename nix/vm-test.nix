# One server and one agent machine. The server runs mandate with a fake
# connector (nix/testconnector) and the real ones, unlinked; the test device
# (nix/testdevice) plays the owner's app on the server, and the agent uses
# the mandate CLI over the network.
{ self, pkgs }:
let
  packages = self.packages.${pkgs.stdenv.hostPlatform.system};
  testPrograms = import ./test-programs.nix packages.mandate;
  bundled = [
    "whatsapp"
    "discord"
    "fastmail"
    "gmessages"
    "matrix"
    "signal"
  ];
in
{
  name = "mandate";

  nodes.server = {
    imports = [ self.nixosModules.default ];
    services.mandate = {
      enable = true;
      agentListen = ":8444";
      connectors = {
        test = {
          package = testPrograms;
          config.account = "vm";
        };
        # Started by the test, on a store left from before.
        legacy = {
          package = testPrograms;
          config.account = "old";
        };
      }
      // pkgs.lib.genAttrs bundled (_: { });
    };
    systemd.services.mandate-legacy.wantedBy = pkgs.lib.mkForce [ ];
    # The module's units come from mandate; NixOS options still override them.
    systemd.services.mandate-test.serviceConfig.IPAddressAllow = [ "192.0.2.1" ];
    networking.firewall.allowedTCPPorts = [
      8443
      8444
    ];
    environment.systemPackages = [
      testPrograms
      pkgs.acl
      pkgs.curl
      pkgs.jq
      pkgs.sqlite
    ];
  };

  nodes.laptop = {
    environment.systemPackages = [ packages.mandate ];
  };

  testScript = ''
    import base64
    import html
    import json
    import re
    import shlex
    import time

    start_all()
    server.wait_for_unit("mandate.target")
    bundled = ${builtins.toJSON bundled}
    for unit in ["front", "gateway", "owner", "test"] + bundled:
        server.wait_for_unit(f"mandate-{unit}.service")
    server.wait_for_open_port(8443)
    # A unit is active before its socket is up.
    for sock in ["gateway/socket", "owner/socket"] + [f"connectors/{c}/socket" for c in ["test"] + bundled]:
        server.wait_until_succeeds(f"test -S /run/mandate/{sock}")

    def device(*args, state="/root/phone.json"):
        out = server.succeed(f"testdevice --state {state} " + " ".join(shlex.quote(a) for a in args))
        return json.loads(out)

    def call(method, path, body=None, state="/root/phone.json"):
        args = ["call", method, path] + ([json.dumps(body)] if body is not None else [])
        return device(*args, state=state)

    def sign(path, body=None, state="/root/phone.json"):
        return device("sign", path, json.dumps(body or {}), state=state)

    # Calls a socket as a process whose primary group is group, which is how
    # the processes tell each other apart.
    def as_group(group, sock, method, path, body=None):
        data = ""
        if body is not None:
            server.succeed(f"echo {base64.b64encode(json.dumps(body).encode()).decode()} | base64 -d > /tmp/body.json")
            data = "--data-binary @/tmp/body.json"
        out = server.succeed(
            f"setpriv --reuid=60999 --regid=$(getent group {group} | cut -d: -f3) --clear-groups "
            f"curl -s -w '\\n%{{http_code}}' -X {method} {data} --unix-socket {sock} http://mandate{path}"
        )
        body, _, code = out.rpartition("\n")
        return int(code), body

    def connector_keys():
        code, out = as_group("mandate-gateway", "/run/mandate/connectors/test/socket", "GET", "/keys")
        assert code == 200, (code, out)
        return json.loads(out)["length"]

    def agent(cmd):
        return laptop.execute("mandate " + cmd + " 2>&1")

    # Reads print a header row, then tab-separated rows.
    def table(cmd):
        status, out = laptop.execute("mandate " + cmd)
        assert status == 0, (cmd, status, out)
        return [line.split("\t") for line in out.splitlines()[1:]]

    def ids(cmd):
        return sorted(r[0] for r in table(cmd))

    def bodies(cmd):
        return [r[-1] for r in table(cmd)]

    def sent():
        out = server.succeed("cat /var/lib/mandate/connectors/test/private/sent.jsonl 2>/dev/null || true")
        return [json.loads(line) for line in out.splitlines()]

    # What mandate setup and reset print, and nothing else: the setup link
    # and, in own-key mode, the key.
    # and what reset adds, as aligned rows.
    def printed(out):
        m = re.fullmatch(r"(Initial setup +)(https://server:8443/setup/(\S+))\nKey +((?:[0-9a-f]{4} ){4}[0-9a-f]{4})\n((?:.+\n)*)", out)
        assert m, out
        rows = {}
        for line in out.splitlines():
            assert line[len(m[1]) - 1] == " " and line[len(m[1])] != " ", ("not aligned", out)
            rows[line[: len(m[1])].strip()] = line[len(m[1]):]
        return m[2].replace("server:8443", "localhost:8443"), m[3], m[4], rows

    # The pairing link on the page the setup link opens.
    def setup_link(url):
        page = server.succeed(f"curl -sk {url}")
        return html.unescape(grab(r'href="(mandate://pair\?[^"]+)"', page))

    def grab(pattern, text):
        m = re.search(pattern, text)
        assert m, (pattern, text)
        return m[1]

    # Another code than code.
    def other(code):
        return f"{(int(code) + 1) % 1000000:06d}"

    with subtest("every process runs as a dynamic user in its fixed group"):
        for p in ["front", "gateway", "owner"]:
            assert server.succeed(f"systemctl show -p DynamicUser --value mandate-{p}").strip() == "yes", p
            assert server.succeed(f"stat -c %G /proc/$(systemctl show -p MainPID --value mandate-{p})").strip() == f"mandate-{p}", p
        for sock, group in [("gateway/socket", "mandate-gateway"), ("owner/socket", "mandate-owner"),
                            ("connectors/test/socket", "mandate-test")]:
            assert server.succeed(f"stat -c '%G %a' /run/mandate/{sock}").strip() == f"{group} 666", sock
        acl = server.succeed("getfacl -p /var/lib/private/mandate/connectors/test")
        assert "group:mandate-gateway:r-x" in acl and "default:group:mandate-gateway:r-x" in acl, acl
        acl = server.succeed("getfacl -p /var/lib/mandate/connectors/test/data.db")
        assert re.search(r"group:mandate-gateway:r..\s+#effective:r--", acl), acl
        assert "mask::---" in server.succeed("getfacl -p /var/lib/mandate/connectors/test/private")
        assert server.succeed("systemctl show -p MemoryDenyWriteExecute --value mandate-signal").strip() == "yes"
        assert "192.0.2.1" in server.succeed("systemctl show -p IPAddressAllow --value mandate-test")
        # systemd skips a unit-file value it can't parse with only a warning,
        # so a sandbox line can silently do nothing.
        warnings = server.succeed("journalctl -b -o cat _PID=1 | grep -E 'mandate.*(is not known|ignoring|Unknown key|Failed to parse)' || true")
        assert warnings == "", warnings
        for u in ["front", "gateway", "owner", "test"]:
            allowed = server.succeed(f"systemctl show -p SystemCallFilter --value mandate-{u}").split()
            assert "read" in allowed and "reboot" not in allowed and "mount" not in allowed, (u, allowed)

    with subtest("mandate setup prints a link that opens the pairing page; the form is the fallback"):
        assert "waiting to be set up" in server.succeed("curl -skf https://localhost:8443/setup")
        assert server.succeed("curl -sk -o /dev/null -w '%{http_code} %{redirect_url}' https://localhost:8443/") == "302 https://localhost:8443/setup"
        url, password, key, rows = printed(server.succeed("mandate setup"))
        assert list(rows) == ["Initial setup", "Key"], rows
        assert server.succeed("stat -c %a /var/lib/mandate/setup-password").strip() == "600"
        page = server.succeed("curl -skf https://localhost:8443/setup/WRONG-PASS")
        assert "<form" in page and "<svg" not in page, page
        code = server.succeed("curl -sk -o /dev/null -w '%{http_code}' --data-urlencode password=WRONG-PASS https://localhost:8443/setup")
        assert code == "403", code
        page = server.succeed(f"curl -sk {url}")
        assert f'<p class="key">Key {key}</p>' in page and "<svg" in page, page
        link = setup_link(url)
        spki = grab(r"spki=([0-9a-f]{64})", link)
        assert spki.startswith(key.replace(" ", "")) and "url=https%3A%2F%2Flocalhost%3A8443" in link, (key, link)
        page = server.succeed(f"curl -skL --data-urlencode password={password} https://localhost:8443/setup")
        assert f"spki={spki}" in html.unescape(page), page
        assert password not in server.succeed("journalctl -u mandate-front -u mandate-owner")

    with subtest("the first device pairs, which spends the link and the setup page"):
        # What the open pairing page polls: / redirects to /setup until a
        # device pairs.
        assert server.succeed("curl -sk -o /dev/null -w '%{http_code}' https://localhost:8443/") == "302"
        out = device("pair", link, "phone")
        assert out["kind"] == "key", out
        assert call("GET", "/owner/v1/server")["body"] == {"agent_url": "https://localhost:8444"}
        assert server.succeed("curl -sk -o /dev/null -w '%{http_code}' https://localhost:8443/setup") == "404"
        assert '<p class="center">Set up.</p>' in server.succeed("curl -skf https://localhost:8443/")
        # The open pairing page polls /, which no longer redirects, and a
        # reload of the spent link lands there too.
        assert server.succeed(f"curl -sk -o /dev/null -w '%{{http_code}} %{{redirect_url}}' {url}") == "302 https://localhost:8443/"
        server.fail(f"testdevice --state /root/thief.json pair '{link}'")
        server.fail("mandate setup")
        server.wait_until_succeeds("test $(testdevice --state /root/phone.json call GET /owner/v1/keys | jq '.body.trusted | length') = 1")
        # The owner side pushes the key log to connectors after pairing.
        retry(lambda _: connector_keys() == 1)
        connectors = {c["name"]: c for c in call("GET", "/owner/v1/connectors")["body"]}
        assert set(connectors) == {"test", "legacy", *bundled}, connectors
        assert connectors["test"]["health"]["state"] == "ok", connectors
        for name in bundled:
            assert connectors[name]["health"]["state"] == "setup", connectors

    with subtest("the owner opens a device exchange; another device joins with its code, and the first signs its key"):
        out = call("POST", "/owner/v1/exchanges", {"kind": "device"})
        assert out["status"] == 201, out
        ex = out["body"]
        # The tablet's key is a fido2 one, as a desktop's security key is.
        join = f"testdevice --state /root/tablet.json --fido2 join https://localhost:8443 {spki}"
        status, out = server.execute(f"{join} {other(ex['code'])} tablet 2>&1")
        assert status != 0 and "403" in out and "attempts_left:2" in out, (status, out)
        server.succeed(f"({join} {ex['code']} tablet >/tmp/join.log 2>&1; echo $? >/tmp/join.exit) >/dev/null 2>&1 &")
        server.wait_until_succeeds("testdevice --state /root/phone.json call GET /owner/v1/exchanges | grep -q confirmed")
        [waiting] = call("GET", "/owner/v1/exchanges")["body"]
        assert waiting["id"] == ex["id"] and waiting["kind"] == "device" and waiting["name"] == "tablet", waiting
        out = sign(f"/owner/v1/exchanges/{ex['id']}/challenge")
        assert out["status"] == 200 and out["body"]["kind"] == "key", out
        record = out["record"]
        assert record["op"] == "add" and record["seq"] == 1 and record["key"] == waiting["payload"]["key"], (record, waiting)
        assert record["key"]["type"] == "fido2", record
        server.wait_until_succeeds("test $(cat /tmp/join.exit) = 0")
        assert call("GET", "/owner/v1/now", state="/root/tablet.json")["status"] == 200
        server.wait_until_succeeds("test $(testdevice --state /root/phone.json call GET /owner/v1/keys | jq '.body.trusted | length') = 2")
        server.wait_until_succeeds(
            "setpriv --reuid=60999 --regid=$(getent group mandate-gateway | cut -d: -f3) --clear-groups "
            "curl -s --unix-socket /run/mandate/connectors/test/socket http://mandate/keys | grep -q '\"length\":2'"
        )

    with subtest("an agent logs in with the code of the exchange the owner opened last; nothing else reaches the owner"):
        token = json.loads(server.succeed("cat /root/phone.json"))["token"]
        server.succeed(
            f"(curl -skN -H 'Authorization: Bearer {token}' https://localhost:8443/owner/v1/stream >/tmp/events 2>&1)"
            " >/dev/null 2>&1 &"
        )
        server.wait_until_succeeds("grep -q ': open' /tmp/events")
        status, out = laptop.execute("echo 123456 | mandate login https://server:8443 --name laptop 2>&1")
        assert status != 0 and "nothing is waiting for a login" in out, (status, out)
        # Denied: the login ends at once with "Not let in."
        denied = call("POST", "/owner/v1/exchanges", {"kind": "agent"})["body"]
        laptop.succeed(
            # set +e: the driver's errexit would end the subshell with mandate.
            f"(set +e; echo {denied['code']} | mandate login https://server:8443 --name laptop >/tmp/denied.log 2>&1;"
            " echo $? >/tmp/denied.exit) >/dev/null 2>&1 &"
        )
        server.wait_until_succeeds("testdevice --state /root/phone.json call GET /owner/v1/exchanges | grep -q confirmed")
        assert call("DELETE", f"/owner/v1/exchanges/{denied['id']}")["status"] == 200
        laptop.wait_until_succeeds("test -s /tmp/denied.exit")
        assert laptop.succeed("cat /tmp/denied.exit").strip() == "1" and "Not let in." in laptop.succeed("cat /tmp/denied.log")
        entry = call("GET", "/owner/v1/activity?kinds=access&limit=1")["body"]["entries"][0]
        assert entry["kind"] == "agent.denied" and entry["detail"] == "laptop" and entry["address"], entry
        first = call("POST", "/owner/v1/exchanges", {"kind": "agent"})["body"]
        ex = call("POST", "/owner/v1/exchanges", {"kind": "agent"})["body"]
        assert [e["id"] for e in call("GET", "/owner/v1/exchanges")["body"]] == [ex["id"]]
        wrong = first["code"] if first["code"] != ex["code"] else other(ex["code"])
        laptop.succeed(
            f"(printf '%s\\n%s\\n' {wrong} {ex['code']} | mandate login https://server:8443 --name laptop >/tmp/login.log 2>&1;"
            " echo $? >/tmp/login.exit) >/dev/null 2>&1 &"
        )
        server.wait_until_succeeds("testdevice --state /root/phone.json call GET /owner/v1/exchanges | grep -q confirmed")
        [waiting] = call("GET", "/owner/v1/exchanges")["body"]
        assert waiting["id"] == ex["id"] and waiting["kind"] == "agent" and waiting["name"] == "laptop", waiting
        assert f" {waiting['address']}/" in laptop.succeed("ip -o addr show eth1"), waiting
        # Admitted with the tablet's fido2 key, so the gateway verifies it.
        out = sign(f"/owner/v1/exchanges/{ex['id']}/challenge", state="/root/tablet.json")
        assert out["status"] == 200 and out["body"]["kind"] == "identity", out
        assert "authenticator_data" in out["envelope"], out
        assert out["record"]["token_sha256"] == waiting["payload"]["token_sha256"], (out, waiting)
        identity = out["body"]["id"]
        laptop.wait_until_succeeds("test $(cat /tmp/login.exit) = 0")
        assert "Wrong code; attempts left: 2." in laptop.succeed("cat /tmp/login.log")
        assert laptop.succeed("stat -c %a /root/.config/mandate/credentials.json").strip() == "600"
        server.wait_until_succeeds("grep -q '\"state\":\"approved\"' /tmp/events")
        events = [json.loads(e) for e in re.findall(r"event: exchange\ndata: (.*)", server.succeed("cat /tmp/events"))]
        want = [(denied["id"], "open"), (denied["id"], "confirmed"), (denied["id"], "ended"),
                (first["id"], "open"), (first["id"], "ended"), (ex["id"], "open"), (ex["id"], "confirmed"), (ex["id"], "approved")]
        assert [(e["id"], e["state"]) for e in events] == want, events

    with subtest("reads wait for a grant; one narrower than asked opens only its chats, back to its horizon"):
        status, out = agent("test chats")
        assert status != 0 and "mandate request test" in out, (status, out)
        status, out = agent("request test --chat Family --chat Nobody --for 30m --reason 'plan dinner' --wait 0")
        assert status == 75, (status, out)
        rid = out.split()[0]
        [req] = call("GET", "/owner/v1/now")["body"]["requests"]
        assert req["id"] == rid and req["reason"] == "plan dinner" and req["identity_name"] == "laptop", req
        terms = {t["term"]: [c["id"] for c in t["chats"] or []] for t in req["terms"]}
        assert terms == {"Family": ["fam"], "Nobody": []}, terms
        out = sign(f"/owner/v1/requests/{rid}/challenge", {"chats": ["fam"], "for": 600})
        assert out["status"] == 200 and out["body"]["kind"] == "grant", out
        laptop.wait_until_succeeds("mandate test chats")
        assert ids("test chats") == ["fam"]
        # The request's 7 days of horizon leave out last month's message.
        assert bodies("test messages fam") == ["[login code hidden]", "fresh family"]
        assert agent("test messages club")[0] != 0
        assert bodies("test search fresh") == ["fresh family"]

    with subtest("never-share hides a chat whatever the grants say"):
        out = sign("/owner/v1/grants/challenge", {"identity": identity, "connector": "test", "for": 1800, "horizon": 0})
        assert out["status"] == 200, out
        laptop.wait_until_succeeds("mandate test chats | grep -q sam")
        assert ids("test chats") == ["club", "fam", "sam"]
        assert "dinner at 7?" in bodies("test messages fam")
        assert call("PUT", "/owner/v1/never-share/test/chat/sam")["status"] == 200
        laptop.wait_until_fails("mandate test chats | grep -q sam")
        assert ids("test chats") == ["club", "fam"]
        assert agent("test messages sam")[0] != 0
        assert "fresh secret" not in bodies("test search fresh")

    with subtest("a store from before, another uid's and without ACLs, is read through the gateway; private/ is not"):
        d = "/var/lib/private/mandate/connectors/legacy"
        now = "strftime('%Y-%m-%dT%H:%M:%fZ', 'now')"
        server.succeed(
            f"mkdir -p {d}/private && echo session > {d}/private/session"
            f" && sqlite3 {d}/data.db < ${../store/schema.sql}"
            f" && sqlite3 {d}/data.db \"INSERT INTO containers (id, kind, name, updated_at) VALUES ('old', 'group', 'Old friends', {now});"
            f" INSERT INTO messages (id, container_id, sender_id, sent_at, body, ingested_at) VALUES ('old/1', 'old', '+15550009', {now}, 'kept from before', {now})\""
            f" && chown -R 4242:4242 {d} && chmod 0750 {d} && chmod 0700 {d}/private && chmod 0600 {d}/data.db"
        )
        assert "mandate-gateway" not in server.succeed(f"getfacl -p {d}/data.db")
        server.succeed("systemctl start mandate-legacy")
        server.wait_until_succeeds("test -S /run/mandate/connectors/legacy/socket")
        out = sign("/owner/v1/grants/challenge", {"identity": identity, "connector": "legacy", "for": 1800, "horizon": 0})
        assert out["status"] == 200, out
        laptop.wait_until_succeeds("mandate legacy chats | grep -q old")
        assert ids("legacy chats") == ["old"]
        assert bodies("legacy messages old") == ["kept from before"]
        # A process in the gateway's group, seeing what the gateway sees.
        as_gateway = ("systemd-run --wait --pipe --quiet -p DynamicUser=yes -p User=probe -p Group=mandate-gateway"
                      " -p BindReadOnlyPaths=/var/lib/private/mandate/connectors:/var/lib/mandate/connectors cat")
        server.succeed(f"{as_gateway} /var/lib/mandate/connectors/legacy/data.db >/dev/null")
        server.fail(f"{as_gateway} /var/lib/mandate/connectors/legacy/private/session")
        assert "mandate-gateway" not in server.succeed(f"getfacl -p {d}/private")

    # Before the write, so that the approval kept across mandate reset is
    # signed by the key the phone pairs with again.
    with subtest("the phone rotates its key: what the old key signed stays valid"):
        before = connector_keys()
        out = device("rotate", "phone")
        keys = call("GET", "/owner/v1/keys")["body"]
        assert keys["retired"] == [out["old"]] and len(keys["trusted"]) == 3, (out, keys)
        assert out["new"] in [k["id"] for k in keys["trusted"]], (out, keys)
        # The connectors take the log with the retire entry.
        retry(lambda _: connector_keys() == before + 2)
        laptop.succeed("mandate test chats")

    with subtest("a write approved with an edit runs exactly once"):
        status, out = agent("test send fam on my way --note 'mum asked' --wait 0")
        assert status == 75, (status, out)
        pid = out.split()[0]
        out = call("GET", f"/owner/v1/proposals/{pid}")
        assert out["status"] == 200 and "mum asked" in json.dumps(out["body"]) and "Family" in json.dumps(out["body"]), out
        assert sent() == []
        out = sign(f"/owner/v1/proposals/{pid}/challenge", {"edits": {"text": "on my way!"}})
        assert out["status"] == 200 and out["body"]["kind"] == "write", out
        approval = out["envelope"]
        status, out = agent(f"status {pid} --wait 60s")
        assert status == 0 and "done" in out, (status, out)
        assert [s["text"] for s in sent()] == ["on my way!"], sent()

        out = call("POST", "/owner/v1/signed", approval)
        assert out["status"] == 409, out
        code, out = as_group("mandate-owner", "/run/mandate/connectors/test/socket", "POST", "/execute", approval)
        assert code == 409, (code, out)
        server.succeed("systemctl restart mandate-test")
        server.wait_for_unit("mandate-test.service")
        server.wait_until_succeeds("test -S /run/mandate/connectors/test/socket")
        code, out = as_group("mandate-owner", "/run/mandate/connectors/test/socket", "POST", "/execute", approval)
        assert code == 409, (code, out)
        assert len(sent()) == 1, sent()

    with subtest("Stop all closes every grant and denies waiting requests"):
        status, out = agent("request test --chat club --reason 'one more' --wait 0")
        assert status == 75, (status, out)
        out = call("POST", "/owner/v1/stop-all")
        assert out == {"status": 200, "body": {"grants": 3, "requests": 1}}, out
        laptop.wait_until_fails("mandate test chats")
        status, out = agent("test chats")
        assert "mandate request test" in out, out
        now = call("GET", "/owner/v1/now")["body"]
        assert now["requests"] == [] and now["grants"] == [], now
        kinds = [e["kind"] for e in call("GET", "/owner/v1/activity?limit=500")["body"]["entries"]]
        # Reads the gateway batched may land after it.
        assert [k for k in kinds if k != "read"][0] == "stop_all" and "request.asked" in kinds and "request.approved" in kinds and "read" in kinds, kinds

    with subtest("callers are told apart by their group"):
        for path in ["/internal/live", "/internal/requests", "/index.html"]:
            code = server.succeed(f"curl -sk -o /dev/null -w '%{{http_code}}' https://localhost:8443{path}")
            assert code == "404", (path, code)
        for path, want in [("/", "404"), ("/owner/v1/now", "404"), ("/setup", "404"), ("/agent/v1/connectors", "401")]:
            code = server.succeed(f"curl -sk -o /dev/null -w '%{{http_code}}' https://localhost:8444{path}")
            assert code == want, ("agent port", path, code)
        for sock, path in [("gateway", "/internal/live"), ("owner", "/internal/requests")]:
            code, _ = as_group("mandate-front", f"/run/mandate/{sock}/socket", "GET", path)
            assert code == 403, (sock, path, code)
        assert as_group("mandate-front", "/run/mandate/owner/socket", "GET", "/owner/v1/now")[0] == 401
        assert as_group("nogroup", "/run/mandate/owner/socket", "GET", "/owner/v1/now")[0] == 403
        test = "/run/mandate/connectors/test/socket"
        for group, method, path, want in [
            ("nogroup", "GET", "/spec", 403),
            ("mandate-front", "GET", "/spec", 403),
            ("mandate-gateway", "GET", "/spec", 200),
            ("mandate-gateway", "POST", "/execute", 403),
            ("mandate-gateway", "PUT", "/keys", 403),
            ("mandate-owner", "POST", "/read", 403),
            ("mandate-owner", "GET", "/containers", 200),
        ]:
            code, out = as_group(group, test, method, path, {} if method != "GET" else None)
            assert code == want, (group, method, path, code, out)

    with subtest("mandate reset starts over, keeping never-share and spent approvals"):
        began = time.monotonic()
        out = server.succeed("mandate reset")
        took = time.monotonic() - began
        print(f"mandate reset took {took:.1f}s")
        # Every unit exits promptly on SIGTERM, well inside systemd's 90 s.
        assert took < 20, took
        new_url, new_password, _, rows = printed(out)
        assert new_password != password
        assert "test" in rows["Still linked"].split(", ") and "Not logged out" not in rows, rows
        server.wait_for_open_port(8443)
        server.wait_until_succeeds("test -S /run/mandate/owner/socket")
        assert call("GET", "/owner/v1/now")["status"] == 401
        assert call("GET", "/owner/v1/now", state="/root/tablet.json")["status"] == 401
        server.wait_until_succeeds("test -S /run/mandate/connectors/test/socket")
        assert connector_keys() == 0
        status, out = agent("test chats")
        assert status != 0 and "mandate login" in out, (status, out)
        # The app pairs again with the key it had.
        assert device("pair", setup_link(new_url), "phone")["kind"] == "key"
        server.wait_until_succeeds(
            "setpriv --reuid=60999 --regid=$(getent group mandate-gateway | cut -d: -f3) --clear-groups "
            "curl -s --unix-socket /run/mandate/connectors/test/socket http://mandate/keys | grep -q '\"length\":1'"
        )
        code, out = as_group("mandate-owner", "/run/mandate/connectors/test/socket", "POST", "/execute", approval)
        assert code == 409, (code, out)
        assert len(sent()) == 1, sent()
        hidden = call("GET", "/owner/v1/never-share")["body"]
        assert [(h["connector"], h["kind"], h["id"]) for h in hidden] == [("test", "chat", "sam")], hidden

    with subtest("mandate reset --unlink logs the accounts out and empties their stores"):
        test_sock = "/run/mandate/connectors/test/socket"
        assert json.loads(as_group("mandate-owner", test_sock, "GET", "/containers?limit=500")[1]) != []
        began = time.monotonic()
        out = server.succeed("mandate reset --unlink")
        print(f"mandate reset --unlink took {time.monotonic() - began:.1f}s")
        _, _, _, rows = printed(out)
        # The test connector logs out itself; nothing is still linked.
        assert "Still linked" not in rows and "test" not in rows.get("Not logged out", ""), rows
        server.wait_until_succeeds(f"test -S {test_sock}")
        code, health = as_group("mandate-gateway", test_sock, "GET", "/health")
        assert code == 200 and json.loads(health)["state"] == "setup", (code, health)
        code, chats = as_group("mandate-owner", test_sock, "GET", "/containers?limit=500")
        assert code == 200 and json.loads(chats) == [], (code, chats)
        # Its spent approvals stay.
        server.succeed("test -s /var/lib/private/mandate/connectors/test/private/spent")
  '';
}
