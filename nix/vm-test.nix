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

    start_all()
    server.wait_for_unit("mandate.target")
    bundled = ${builtins.toJSON bundled}
    for unit in ["front", "gateway", "owner", "test"] + bundled:
        server.wait_for_unit(f"mandate-{unit}.service")
    server.wait_for_open_port(8443)

    def device(*args, state="/root/phone.json"):
        out = server.succeed(f"testdevice --state {state} " + " ".join(shlex.quote(a) for a in args))
        return json.loads(out)

    def call(method, path, body=None, state="/root/phone.json"):
        args = ["call", method, path] + ([json.dumps(body)] if body is not None else [])
        return device(*args, state=state)

    def sign(path, body=None):
        return device("sign", path, json.dumps(body or {}))

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

    def setup_link(password):
        page = server.succeed(f"curl -sk --data-urlencode password={password} https://localhost:8443/setup")
        return html.unescape(grab(r'href="(mandate://pair\?[^"]+)"', page))

    def grab(pattern, text):
        m = re.search(pattern, text)
        assert m, (pattern, text)
        return m[1]

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

    with subtest("mandate setup prints a password, which the setup page trades for a pairing link"):
        out = server.succeed("mandate setup")
        password = grab(r"Setup password: (\S+)", out)
        spki = grab(r"Server key SHA-256: ([0-9a-f]{64})", out)
        assert server.succeed("stat -c %a /var/lib/mandate/setup-password").strip() == "600"
        assert "password" in server.succeed("curl -skf https://localhost:8443/setup")
        code = server.succeed("curl -sk -o /dev/null -w '%{http_code}' --data-urlencode password=WRONG-PASS https://localhost:8443/setup")
        assert code == "403", code
        link = setup_link(password)
        assert f"spki={spki}" in link and "url=https%3A%2F%2Flocalhost%3A8443" in link, link

    with subtest("the first device pairs, which spends the link and the setup page"):
        out = device("pair", link, "phone")
        assert out["kind"] == "key", out
        assert server.succeed("curl -sk -o /dev/null -w '%{http_code}' https://localhost:8443/setup") == "404"
        server.fail(f"testdevice --state /root/thief.json pair '{link}'")
        server.fail("mandate setup")
        server.wait_until_succeeds("test $(testdevice --state /root/phone.json call GET /owner/v1/keys | jq '.body.trusted | length') = 1")
        assert connector_keys() == 1
        connectors = {c["name"]: c for c in call("GET", "/owner/v1/connectors")["body"]}
        assert set(connectors) == {"test", "legacy", *bundled}, connectors
        assert connectors["test"]["health"]["state"] == "ok", connectors
        for name in bundled:
            assert connectors[name]["health"]["state"] == "setup", connectors

    with subtest("another device joins by code, and the first signs its key"):
        server.succeed(
            f"(testdevice --state /root/tablet.json join https://localhost:8443 {spki} tablet >/tmp/join.log 2>&1;"
            " echo $? >/tmp/join.exit) >/dev/null 2>&1 &"
        )
        server.wait_until_succeeds("grep -q 'code: ' /tmp/join.log")
        code = grab(r"code: (\d{6})", server.succeed("cat /tmp/join.log"))
        [ex] = call("GET", "/owner/v1/exchanges")["body"]
        assert ex["kind"] == "device" and ex["name"] == "tablet", ex
        out = call("POST", f"/owner/v1/exchanges/{ex['id']}/code", {"code": code})
        assert out["status"] == 200 and out["body"]["payload"]["key"]["name"] == "tablet", out
        out = sign(f"/owner/v1/exchanges/{ex['id']}/challenge")
        assert out["status"] == 200 and out["body"]["kind"] == "key", out
        record = out["record"]
        assert record["op"] == "add" and record["seq"] == 1, record
        server.wait_until_succeeds("test $(cat /tmp/join.exit) = 0")
        assert call("GET", "/owner/v1/now", state="/root/tablet.json")["status"] == 200
        server.wait_until_succeeds("test $(testdevice --state /root/phone.json call GET /owner/v1/keys | jq '.body.trusted | length') = 2")
        server.wait_until_succeeds(
            "setpriv --reuid=60999 --regid=$(getent group mandate-gateway | cut -d: -f3) --clear-groups "
            "curl -s --unix-socket /run/mandate/connectors/test/socket http://mandate/keys | grep -q '\"length\":2'"
        )

    with subtest("an agent logs in with a code the owner types; a wrong code costs a try"):
        laptop.succeed(
            "(mandate login https://server:8443 --name laptop >/tmp/login.log 2>&1; echo $? >/tmp/login.exit)"
            " >/dev/null 2>&1 &"
        )
        laptop.wait_until_succeeds("grep -q 'code: ' /tmp/login.log")
        code = grab(r"code: (\d{6})", laptop.succeed("cat /tmp/login.log"))
        server.wait_until_succeeds("testdevice --state /root/phone.json call GET /owner/v1/exchanges | grep -q laptop")
        [ex] = call("GET", "/owner/v1/exchanges")["body"]
        assert ex["kind"] == "agent" and ex["name"] == "laptop", ex
        assert f" {ex['address']}/" in laptop.succeed("ip -o addr show eth1"), ex
        wrong = "000000" if code != "000000" else "111111"
        out = call("POST", f"/owner/v1/exchanges/{ex['id']}/code", {"code": wrong})
        assert out["status"] == 403 and out["body"]["attempts_left"] == 2, out
        out = call("POST", f"/owner/v1/exchanges/{ex['id']}/code", {"code": code})
        assert out["status"] == 200 and "token_sha256" in out["body"]["payload"], out
        out = sign(f"/owner/v1/exchanges/{ex['id']}/challenge")
        assert out["status"] == 200 and out["body"]["kind"] == "identity", out
        identity = out["body"]["id"]
        laptop.wait_until_succeeds("test $(cat /tmp/login.exit) = 0")
        assert laptop.succeed("stat -c %a /root/.config/mandate/credentials.json").strip() == "600"

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

    with subtest("callers are told apart by their group"):
        for path in ["/internal/live", "/internal/requests", "/"]:
            code = server.succeed(f"curl -sk -o /dev/null -w '%{{http_code}}' https://localhost:8443{path}")
            assert code == "404", (path, code)
        for path, want in [("/owner/v1/now", "404"), ("/setup", "404"), ("/agent/v1/connectors", "401")]:
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
        out = server.succeed("mandate reset")
        new_password = grab(r"Setup password: (\S+)", out)
        assert new_password != password
        server.wait_for_open_port(8443)
        server.wait_until_succeeds("test -S /run/mandate/owner/socket")
        assert call("GET", "/owner/v1/now")["status"] == 401
        assert call("GET", "/owner/v1/now", state="/root/tablet.json")["status"] == 401
        server.wait_until_succeeds("test -S /run/mandate/connectors/test/socket")
        assert connector_keys() == 0
        status, out = agent("test chats")
        assert status != 0 and "mandate login" in out, (status, out)
        # The app pairs again with the key it had.
        assert device("pair", setup_link(new_password), "phone")["kind"] == "key"
        server.wait_until_succeeds(
            "setpriv --reuid=60999 --regid=$(getent group mandate-gateway | cut -d: -f3) --clear-groups "
            "curl -s --unix-socket /run/mandate/connectors/test/socket http://mandate/keys | grep -q '\"length\":1'"
        )
        code, out = as_group("mandate-owner", "/run/mandate/connectors/test/socket", "POST", "/execute", approval)
        assert code == 409, (code, out)
        assert len(sent()) == 1, sent()
        hidden = call("GET", "/owner/v1/never-share")["body"]
        assert [(h["connector"], h["kind"], h["id"]) for h in hidden] == [("test", "chat", "sam")], hidden
  '';
}
