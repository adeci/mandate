# Boots every unit under its real sandbox. WhatsApp and Discord stay unlinked,
# so both are checked for wiring only. A recorder stands in for an HTTP API,
# whose token the owner sets in the console, and another for ntfy. The test
# driver plays the owner's passkey, creating the agent and its grants through
# the console. The console serves the ACME module's self-signed placeholder
# certificate, and the admin password is generated on first start.
{ pkgs, self }:
let
  apiToken = "api-token-0123456789abcdef";
  # Dotted: curl, like the public suffix list, refuses cookies for bare names.
  host = "mandate.example.com";

  fixture = pkgs.writeText "fixture.sql" ''
    INSERT INTO containers (id, kind, parent_id, name, is_group, updated_at) VALUES
      ('fam@g.us', 'group', NULL, 'Family', 1, '2026-10-01T00:00:00.000Z'),
      ('sub@g.us', 'thread', 'fam@g.us', 'Dinner thread', 1, '2026-10-01T00:00:00.000Z'),
      ('club@g.us', 'group', NULL, 'Book club', 1, '2026-10-01T00:00:00.000Z'),
      ('hidden@s.whatsapp.net', 'dm', NULL, 'Hidden', 0, '2026-10-01T00:00:00.000Z'),
      ('long@g.us', 'group', NULL, 'Long ' || replace(hex(zeroblob(120)), '0', 'w'), 1, '2026-10-01T00:00:00.000Z');
    INSERT INTO messages (id, container_id, sender_id, sent_at, body, ingested_at) VALUES
      ('fam@g.us/1', 'fam@g.us', '1555@s.whatsapp.net', '2026-10-01T12:00:00.000Z', 'dinner at 7?', '2026-10-01T12:00:00.000Z'),
      ('fam@g.us/2', 'fam@g.us', '1555@s.whatsapp.net', '2026-10-01T12:01:00.000Z', 'Your WhatsApp code: 482-913', '2026-10-01T12:01:00.000Z'),
      ('hidden@s.whatsapp.net/3', 'hidden@s.whatsapp.net', '1666@s.whatsapp.net', '2026-10-01T12:02:00.000Z', 'private', '2026-10-01T12:02:00.000Z'),
      ('fam@g.us/4', 'fam@g.us', '1555@s.whatsapp.net', '2026-10-01T12:03:00.000Z', 'look', '2026-10-01T12:03:00.000Z'),
      ('fam@g.us/5', 'fam@g.us', '1555@s.whatsapp.net', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), 'fresh family', strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
      ('club@g.us/6', 'club@g.us', '1777@s.whatsapp.net', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), 'fresh club', strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
      ('sub@g.us/7', 'sub@g.us', '1555@s.whatsapp.net', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), 'fresh thread', strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
      ('long@g.us/8', 'long@g.us', '1555@s.whatsapp.net', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), 'fresh long', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));
    INSERT INTO media (message_id, idx, type, size) VALUES
      ('hidden@s.whatsapp.net/3', 0, 'image/jpeg', 5),
      ('fam@g.us/4', 0, 'image/jpeg', 5);
  '';

  # One email thread with an old email (seq 1) and a new one (seq 2).
  fastmailFixture = pkgs.writeText "fastmail.sql" ''
    INSERT INTO containers (id, kind, name, updated_at) VALUES
      ('inv', 'thread', 'Invoice', '2026-10-01T00:00:00.000Z');
    INSERT INTO messages (id, container_id, sender_id, sent_at, body, ingested_at) VALUES
      ('inv/a', 'inv', 'billing@example.com', '2026-10-01T12:00:00.000Z', 'old invoice', '2026-10-01T12:00:00.000Z'),
      ('inv/b', 'inv', 'billing@example.com', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), 'new invoice', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));
  '';

  signalFixture = pkgs.writeText "signal.sql" ''
    INSERT INTO containers (id, kind, name, updated_at) VALUES
      ('sfam', 'dm', 'Mum', '2026-10-01T00:00:00.000Z'),
      ('swork', 'group', 'Work', '2026-10-01T00:00:00.000Z');
    INSERT INTO messages (id, container_id, sender_id, sent_at, body, ingested_at) VALUES
      ('sfam/a', 'sfam', '+15550001', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), 'call me', strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
      ('swork/a', 'swork', '+15550002', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), 'standup moved', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));
  '';

  # Logs every request to a stand-in service as a JSON line.
  recorder = pkgs.writeText "recorder.py" ''
    import http.server, json, sys

    port, log = int(sys.argv[1]), sys.argv[2]

    class H(http.server.BaseHTTPRequestHandler):
        def handle_one(self):
            body = self.rfile.read(int(self.headers.get("Content-Length") or 0)).decode()
            with open(log, "a") as f:
                f.write(json.dumps({"method": self.command, "path": self.path,
                                    "headers": dict(self.headers), "body": body}) + "\n")
            self.send_response(201 if self.command == "POST" else 200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"url": "https://api.example/items/1"}')
        do_GET = do_POST = handle_one
        def log_message(self, *args):
            pass

    http.server.ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
  '';
  recorderService = port: name: {
    wantedBy = [ "multi-user.target" ];
    before = [
      "mandate-http.service"
      "mandate-console.service"
    ];
    serviceConfig = {
      ExecStart = "${pkgs.python3}/bin/python3 ${recorder} ${toString port} /var/lib/${name}/log.jsonl";
      StateDirectory = name;
    };
  };
in
pkgs.testers.runNixOSTest {
  name = "mandate";

  extraPythonPackages = p: [
    p.cryptography
    p.cbor2
  ];

  nodes.machine = {
    imports = [ self.nixosModules.default ];
    services.mandate = {
      enable = true;
      hostname = host;
      useACMEHost = host;
      openFirewallOn = "eth1";
      connectors = {
        whatsapp = { };
        discord = { };
        signal = { };
        matrix = { };
        fastmail = { };
        gmessages = { };
        http = {
          settings.services.api = {
            base_url = "http://127.0.0.1:9000";
            reads = [ "GET /items" ];
            writes = [ "POST /items" ];
          };
          serviceConfig.IPAddressAllow = [ "127.0.0.1" ];
        };
      };
      notify.ntfy = {
        url = "http://127.0.0.1:9001/mandate";
        tokenFile = "/etc/mandate-ntfy-token";
      };
      gatewayUrl = "http://machine:8780";
    };
    environment.etc."mandate-ntfy-token".text = "tk_ntfytest";
    # No ACME server answers, so the console keeps the self-signed placeholder.
    security.acme = {
      acceptTerms = true;
      defaults.server = "https://127.0.0.1:1/directory";
      certs.${host}.webroot = "/var/lib/acme/acme-challenge";
    };
    time.timeZone = "Europe/Berlin";
    systemd.services.recorder = recorderService 9000 "recorder";
    systemd.services.ntfy = recorderService 9001 "ntfy";
    environment.systemPackages = [
      pkgs.sqlite
      pkgs.curl
    ];
  };

  # Not on the tailnet: reaches the open ports but IPAddressAllow must drop it.
  nodes.outsider = {
    environment.systemPackages = [ pkgs.curl ];
  };

  testScript = ''
    import base64
    import hashlib
    import json
    import re
    import secrets
    import time

    import cbor2
    from cryptography.hazmat.primitives import hashes
    from cryptography.hazmat.primitives.asymmetric import ec
    from cryptography.hazmat.primitives.asymmetric.utils import decode_dss_signature
    from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat

    start_all()
    for unit in ["recorder", "ntfy", "mandate-gateway", "mandate-whatsapp", "mandate-discord", "mandate-signal", "mandate-matrix", "mandate-fastmail", "mandate-gmessages", "mandate-console", "mandate-http"]:
        machine.wait_for_unit(f"{unit}.service")
    for port in [8780, 443, 9000, 9001]:
        machine.wait_for_open_port(port)

    admin_password = machine.succeed("cat /var/lib/mandate/admin-password").strip()
    token = "0" * 64

    def curl(args, body=None):
        cmd = "curl -s -o /tmp/resp -w '%{http_code}' " + args
        if body is not None:
            data = base64.b64encode(json.dumps(body).encode()).decode()
            cmd = f"echo {data} | base64 -d | {cmd} -H 'Content-Type: application/json' --data-binary @-"
        code = int(machine.succeed(cmd))
        out = machine.succeed("cat /tmp/resp")
        try:
            return code, json.loads(out)
        except ValueError:
            return code, out

    def gw(method, path, body=None, tok=None):
        return curl(f"-X {method} -H 'Authorization: Bearer {tok or token}' 'http://127.0.0.1:8780{path}'", body)

    # The gateway learns agents and grants when it next syncs with the console.
    def gw_until(path, ok, timeout=60):
        deadline = time.time() + timeout
        while True:
            code, out = gw("GET", path)
            if ok(code, out):
                return code, out
            assert time.time() < deadline, (path, code, out)
            time.sleep(1)

    # The console only admits localhost and the tailnet, so reach it over loopback.
    def console(method, path, body=None, jar="/tmp/jar"):
        return curl(f"-X {method} -b {jar} -c {jar} --cacert /var/lib/acme/${host}/chain.pem "
                    f"--resolve ${host}:443:127.0.0.1 https://${host}{path}", body)

    # Run as the gateway, which may read a connector's health and key head.
    def connector_get(name, path):
        return f"sudo -u mandate-gateway curl -s --unix-socket /run/mandate-{name}/connector.sock http://connector/{path}"

    def connector_keys(name):
        return json.loads(machine.succeed(connector_get(name, "keys")))

    def recorded(name="recorder"):
        lines = machine.succeed(f"cat /var/lib/{name}/log.jsonl 2>/dev/null || true").splitlines()
        return [json.loads(line) for line in lines]

    def rows(out):
        return [r[0] for r in out["rows"]]

    def bodies(out):
        return [r[-1] for r in out["rows"]]

    def b64(b):
        return base64.urlsafe_b64encode(b).rstrip(b"=").decode()

    P256_ORDER = 0xFFFFFFFF00000000FFFFFFFFFFFFFFFFBCE6FAADA7179E84F3B9CAC2FC632551
    seed = int.from_bytes(hashlib.sha256(b"mandate-vm-test-passkey").digest(), "big") % P256_ORDER
    passkey = ec.derive_private_key(seed, ec.SECP256R1())
    credential = b"mandate-vm-test-cred"
    origin = "https://${host}"

    def client_data(kind, challenge):
        return json.dumps({"type": kind, "challenge": challenge, "origin": origin, "crossOrigin": False}).encode()

    # Answers navigator.credentials.create the way a security key would.
    def registration(options, key=passkey, cred=credential):
        n = key.public_key().public_numbers()
        cose = cbor2.dumps({1: 2, 3: -7, -1: 1, -2: n.x.to_bytes(32, "big"), -3: n.y.to_bytes(32, "big")})
        auth = (hashlib.sha256(options["rp"]["id"].encode()).digest() + bytes([0x45]) + (0).to_bytes(4, "big")
                + bytes(16) + len(cred).to_bytes(2, "big") + cred + cose)
        attestation = cbor2.dumps({"fmt": "none", "attStmt": {}, "authData": auth})
        return {"id": b64(cred), "rawId": b64(cred), "type": "public-key",
                "response": {"clientDataJSON": b64(client_data("webauthn.create", options["challenge"])),
                             "attestationObject": b64(attestation)}}

    # Answers navigator.credentials.get.
    def assertion(options, flags=0x05, key=passkey, cred=credential):
        cd = client_data("webauthn.get", options["challenge"])
        auth = hashlib.sha256(options["rpId"].encode()).digest() + bytes([flags]) + (1).to_bytes(4, "big")
        sig = key.sign(auth + hashlib.sha256(cd).digest(), ec.ECDSA(hashes.SHA256()))
        return {"id": b64(cred), "rawId": b64(cred), "type": "public-key",
                "response": {"authenticatorData": b64(auth), "clientDataJSON": b64(cd), "signature": b64(sig)}}

    # Signs an agent or a grant on the Access page.
    def sign(fields, key=passkey, cred=credential, jar="/tmp/jar"):
        code, ch = console("POST", "/access/challenge", fields, jar=jar)
        assert code == 200, (code, ch)
        return console("POST", "/access/approve", {"nonce": ch["nonce"], "assertion": assertion(ch["options"], key=key, cred=cred)}, jar=jar)

    def grant(fields):
        code, out = sign({"kind": "grant", "agent": agent_id, **fields})
        assert code == 200 and out["kind"] == "grant", (code, out)
        return out["id"]

    with subtest("the console admits the admin password, not guesses"):
        code, _ = console("GET", "/", jar="/tmp/nobody")
        assert code == 303, code
        code, _ = console("POST", "/login", {"password": "wrong"}, jar="/tmp/nobody")
        assert code in (401, 403), code
        code, out = console("POST", "/login", {"password": admin_password})
        assert code == 200, (code, out)

    with subtest("the first key is trusted at once and reaches every connector; the password is off"):
        code, options = console("POST", "/keys/options", {"name": "yubikey"})
        assert code == 200, (code, options)
        code, out = console("POST", "/keys", registration(options))
        assert code == 200 and out["trusted"], (code, out)
        for name in ["whatsapp", "discord", "http"]:
            machine.wait_until_succeeds(connector_get(name, "keys") + " | grep -q '\"length\":1'")
        code, _ = console("GET", "/session")
        assert code == 401, code
        code, _ = console("POST", "/login", {"password": admin_password})
        assert code == 403, code
        code, options = console("POST", "/login/options", {})
        code, out = console("POST", "/login", {"assertion": assertion(options)})
        assert code == 200, (code, out)

    with subtest("the browser makes an agent's token; the console signs only its hash, which the gateway learns"):
        code, _ = gw("GET", "/v1/connectors")
        assert code == 401, code
        made = secrets.token_hex(32)
        code, out = sign({"kind": "agent", "name": "Grok Bot", "token_sha256": hashlib.sha256(made.encode()).hexdigest()})
        assert code == 200 and out["kind"] == "agent" and "token" not in out, (code, out)
        agent_id, token = out["id"], made
        gw_until("/v1/connectors", lambda code, _: code == 200)
        code, _ = gw("GET", "/v1/connectors", tok="f" * 64)
        assert code == 401, code
        code, page = console("GET", "/agents")
        assert code == 200 and "Grok Bot" in page and token not in page, code
        code, _ = console("GET", "/access")
        assert code == 303, code
        # The setup sheet's URL, and the gateway reporting that the agent connected.
        code, out = console("GET", "/agents.json")
        assert code == 200 and out["gateway_url"] == "http://machine:8780", (code, out)
        machine.wait_until_succeeds(
            "curl -s -b /tmp/jar --cacert /var/lib/acme/${host}/chain.pem --resolve ${host}:443:127.0.0.1 "
            "https://${host}/agents.json | grep -q first_seen"
        )
        machine.fail(f"grep -rq {token} /var/lib/mandate-console")
        machine.fail("curl -sf http://127.0.0.1:8780/v1/whatsapp/chats")

    with subtest("reads are closed until the owner grants them"):
        machine.succeed(
            "cd /var/lib/mandate-whatsapp && sudo -u mandate-whatsapp sh -c "
            "'umask 0027 && sqlite3 data.db < ${../store/schema.sql} && sqlite3 data.db < ${fixture}'"
        )
        machine.succeed("printf '[whatsapp]\\nhide_containers = [\"hidden@s.whatsapp.net\"]\\n' > /var/lib/mandate-gateway/exposure.toml")
        for path in ["/v1/whatsapp/chats", "/v1/whatsapp/messages?chat=fam@g.us", "/v1/whatsapp/search?text=dinner",
                     "/v1/whatsapp/media?chat=fam@g.us&seq=4"]:
            code, out = gw("GET", path)
            assert code == 403 and "no read access to whatsapp" in out["error"], (path, code, out)
            assert out["request"].startswith("mandate request whatsapp"), out
        code, out = gw("GET", "/v1/forward/http/api/items")
        assert code == 403, (code, out)
        for path, body in [("/v1/whatsapp/send", {"args": {"chat": "fam@g.us", "text": "hi"}}),
                           ("/v1/http/request", {"args": {"service": "api", "method": "POST", "path": "/items", "body": "{}"}})]:
            code, out = gw("POST", path, body)
            assert code == 403 and "no read access" in out["error"], (path, code, out)

    with subtest("a grant opens exactly its chats, back to its horizon, for reads and writes"):
        scoped = grant({"connector": "whatsapp", "chats": ["fam@g.us"], "for": "30m", "horizon": "24h"})
        _, out = gw_until("/v1/whatsapp/chats", lambda code, _: code == 200)
        # The thread under the granted group was not checked, so it stays out.
        assert rows(out) == ["fam@g.us"], out
        code, out = gw("GET", "/v1/whatsapp/messages?chat=fam@g.us")
        assert code == 200 and bodies(out) == ["fresh family"], (code, out)
        for chat in ["club@g.us", "sub@g.us"]:
            code, out = gw("GET", f"/v1/whatsapp/messages?chat={chat}")
            assert code != 200 and "fresh" not in json.dumps(out), (chat, code, out)
        code, out = gw("GET", "/v1/whatsapp/search?text=fresh")
        assert code == 200 and bodies(out) == ["fresh family"], (code, out)
        code, out = gw("GET", "/v1/whatsapp/search?text=dinner")
        assert code == 200 and "dinner" not in json.dumps(out["rows"]), (code, out)
        code, out = gw("GET", "/v1/whatsapp/media?chat=fam@g.us&seq=4")
        assert code == 404, (code, out)
        code, out = gw("GET", "/v1/access")
        assert code == 200 and "Family" in json.dumps(out), (code, out)
        # Outside the grant a chat is unknown; inside it, the unlinked connector is asked.
        for chat, want in [("club@g.us", 404), ("hidden@s.whatsapp.net", 404), ("nobody@g.us", 404), ("fam@g.us", 503)]:
            code, out = gw("POST", "/v1/whatsapp/send", {"args": {"chat": chat, "text": "hi"}})
            assert code == want, (chat, code, out)
            assert want == 503 or "unknown chat" in out["error"], (chat, out)

    with subtest("a message past the horizon is unknown, even in a granted chat"):
        machine.succeed(
            "cd /var/lib/mandate-fastmail && sudo -u mandate-fastmail sh -c "
            "'umask 0027 && sqlite3 data.db < ${../store/schema.sql} && sqlite3 data.db < ${fastmailFixture}'"
        )
        mail = grant({"connector": "fastmail", "chats": ["inv"], "for": "30m", "horizon": "24h"})
        gw_until("/v1/fastmail/chats", lambda code, _: code == 200)
        # The old email and one that doesn't exist get the same 404 before the
        # connector is asked; the new one reaches it.
        unknown = []
        for seq in ["1", "99"]:
            code, out = gw("POST", "/v1/fastmail/fetch", {"args": {"chat": "inv", "seq": seq}})
            assert code == 404, (seq, code, out)
            unknown.append(out["error"].replace(seq, "N"))
        assert unknown[0] == unknown[1], unknown
        code, out = gw("POST", "/v1/fastmail/reply", {"args": {"chat": "inv", "seq": "1", "body": "paid"}})
        assert code == 404, (code, out)
        code, out = gw("POST", "/v1/fastmail/fetch", {"args": {"chat": "inv", "seq": "2"}})
        assert code == 503, (code, out)

    with subtest("revoking a grant ends its reads and drops its signature from the console"):
        signed = machine.succeed(
            f"sudo -u mandate-console sqlite3 /var/lib/mandate-console/console.db \"SELECT hex(signed) FROM grants WHERE id = '{scoped}'\""
        ).strip()
        code, _ = console("POST", f"/access/grants/{scoped}/close")
        assert code == 303, code
        gw_until("/v1/whatsapp/chats", lambda code, _: code == 403)
        gone = machine.succeed(
            f"sudo -u mandate-console sqlite3 /var/lib/mandate-console/console.db \"SELECT signed IS NULL FROM grants WHERE id = '{scoped}'\""
        )
        assert gone.strip() == "1", gone

    with subtest("chat terms are checked before anything is looked up"):
        # A 201-character term is refused alike whether a chat's name holds it.
        answers = []
        for term in ["w" * 201, "q" * 201]:
            code, out = gw("POST", "/v1/access/requests", {"connector": "whatsapp", "chats": [term], "reason": "plan dinner"})
            assert code == 400, (code, out)
            answers.append(out)
        assert answers[0] == answers[1], answers

    with subtest("an agent asks, the owner hears of it, narrows it and approves with the key"):
        # Durations in nanoseconds, as Go encodes them. Terms that match nothing
        # get the same answer as those that do.
        code, ref = gw("POST", "/v1/access/requests", {"connector": "whatsapp", "chats": ["Nobody", "Hidden", "Family", "Book"],
                                                         "for": 3600 * 10**9, "reason": "plan dinner"})
        assert code == 202 and set(ref) == {"id", "url"} and ref["url"].startswith("https://${host}/"), (code, ref)
        rid = ref["id"]
        code, st = gw("GET", f"/v1/access/requests/{rid}?wait=1")
        assert code == 200 and st["status"] == "pending", (code, st)
        code, page = console("GET", "/")
        assert "Grok Bot" in page and "plan dinner" in page and "Book club" in page and "fam@g.us" in page, code
        assert "no visible chat matched" in page and "Nobody" in page and "hidden@s.whatsapp.net" not in page, code
        machine.wait_until_succeeds("grep -q 'Grok Bot' /var/lib/ntfy/log.jsonl")
        [note] = recorded("ntfy")
        assert note["method"] == "POST" and note["path"] == "/mandate", note
        assert note["headers"]["Title"] == "Grok Bot" and note["headers"]["Priority"] == "high", note
        assert note["headers"]["Click"].startswith("https://${host}/"), note
        assert note["headers"]["Authorization"] == "Bearer tk_ntfytest", note
        assert "Family" not in note["body"] and "Book" not in note["body"] and "plan dinner" not in note["body"], note

        code, out = sign({"kind": "grant", "request": rid, "chats": ["fam@g.us"], "for": "10m"})
        assert code == 200 and out["kind"] == "grant", (code, out)
        code, st = gw("GET", f"/v1/access/requests/{rid}?wait=5")
        assert st["status"] == "granted" and st["grant_id"] == out["id"], (code, st)
        _, out = gw_until("/v1/whatsapp/chats", lambda code, _: code == 200)
        assert rows(out) == ["fam@g.us"], out
        # The request's default horizon of 7 days hides the October messages.
        code, out = gw("GET", "/v1/whatsapp/messages?chat=fam@g.us")
        assert bodies(out) == ["fresh family"], (code, out)

    with subtest("an agent has at most 3 requests waiting; a denied one grants nothing"):
        waiting = []
        for _ in range(3):
            code, ref = gw("POST", "/v1/access/requests", {"connector": "discord", "reason": "look around"})
            assert code == 202, (code, ref)
            waiting.append(ref["id"])
        # Over the limit, a term that matches and one that doesn't get the same 429.
        answers = []
        for term in ["Family", "Nobody"]:
            code, out = gw("POST", "/v1/access/requests", {"connector": "whatsapp", "chats": [term], "reason": "look around"})
            assert code == 429, (term, code, out)
            answers.append(out)
        assert answers[0] == answers[1], answers
        for rid in waiting:
            code, _ = console("POST", f"/access/requests/{rid}/deny")
            assert code == 303, code
            code, st = gw("GET", f"/v1/access/requests/{rid}?wait=5")
            assert st["status"] == "denied", (code, st)

    with subtest("an agent asks for a routine; it opens on schedule, pauses and resumes"):
        machine.succeed(
            "cd /var/lib/mandate-signal && sudo -u mandate-signal sh -c "
            "'umask 0027 && sqlite3 data.db < ${../store/schema.sql} && sqlite3 data.db < ${signalFixture}'"
        )
        # Every day from this minute, so the first window is open now.
        start = machine.succeed("TZ=Europe/Berlin date +%H:%M").strip()
        code, ref = gw("POST", "/v1/access/requests", {"connector": "signal", "chats": ["Mum"], "reason": "morning summary",
                                                         "schedule": {"start": start, "length": 30 * 60 * 10**9}})
        assert code == 202, (code, ref)
        code, page = console("GET", "/")
        assert "morning summary" in page, code
        code, out = sign({"kind": "grant", "request": ref["id"], "zone": "Europe/Berlin", "chats": ["sfam"]})
        assert code == 200 and out["kind"] == "grant", (code, out)
        routine = out["id"]
        code, st = gw("GET", f"/v1/access/requests/{ref['id']}?wait=5")
        assert st["status"] == "granted" and st["grant_id"] == routine, (code, st)
        _, out = gw_until("/v1/signal/chats", lambda code, _: code == 200)
        assert rows(out) == ["sfam"], out
        code, now = console("GET", "/now.json")
        assert {"waiting", "open", "routines", "stopped", "today"} <= set(now) and routine in json.dumps(now["routines"]), now

        code, _ = console("POST", f"/access/grants/{routine}/close")
        assert code == 303, code
        gw_until("/v1/signal/chats", lambda code, _: code == 403)
        code, out = sign({"kind": "resume", "grant": routine})
        assert code == 200 and out["id"] != routine, (code, out)
        routine = out["id"]
        gw_until("/v1/signal/chats", lambda code, _: code == 200)

    with subtest("an agent asks to extend an open grant; the owner adds more with one touch"):
        code, ref = gw("POST", "/v1/access/requests", {"connector": "fastmail", "extend": True, "for": 30 * 60 * 10**9,
                                                         "reason": "still reading"})
        assert code == 202, (code, ref)
        code, out = sign({"kind": "grant", "request": ref["id"]})
        assert code == 200 and out["kind"] == "grant" and out["id"] != mail, (code, out)
        code, st = gw("GET", f"/v1/access/requests/{ref['id']}?wait=5")
        assert st["status"] == "granted" and st["grant_id"] == out["id"], (code, st)
        extended = out["id"]
        code, out = sign({"kind": "extend", "grant": extended, "for": "30m"})
        assert code == 200 and out["id"] != extended, (code, out)
        mail = out["id"]
        _, out = gw_until("/v1/access", lambda code, out: code == 200 and mail in json.dumps(out))
        assert extended not in json.dumps(out), out
        code, now = console("GET", "/now.json")
        assert mail in json.dumps(now["open"]), now
        code, out = gw("POST", "/v1/fastmail/fetch", {"args": {"chat": "inv", "seq": "2"}})
        assert code == 503, (code, out)

    with subtest("a grant on every chat reads them all, through the hide rules"):
        grant({"connector": "whatsapp", "for": "30m", "horizon": "0"})
        _, out = gw_until("/v1/whatsapp/chats", lambda code, out: code == 200 and "club@g.us" in rows(out))
        assert sorted(rows(out)) == ["club@g.us", "fam@g.us", "long@g.us", "sub@g.us"], out
        code, out = gw("GET", "/v1/whatsapp/messages?chat=fam@g.us")
        assert bodies(out) == ["fresh family", "[image/jpeg] look", "[login code hidden]", "dinner at 7?"], out
        code, out = gw("GET", "/v1/whatsapp/messages?chat=hidden@s.whatsapp.net")
        assert code == 404, (code, out)
        code, out = gw("GET", "/v1/whatsapp/messages?chat=Family")
        assert code == 404 and "chats --search" in out["error"], (code, out)
        code, out = gw("GET", "/v1/whatsapp/media?chat=hidden@s.whatsapp.net&seq=3")
        assert code == 404 and "unknown chat" in out["error"], (code, out)
        code, out = gw("GET", "/v1/whatsapp/media?chat=fam@g.us&seq=1")
        assert code == 404 and "no media" in out["error"], (code, out)
        # Unlinked, so the gateway gets as far as asking whether it is healthy.
        code, out = gw("GET", "/v1/whatsapp/media?chat=fam@g.us&seq=4")
        assert code == 503 and "needs the owner" in out["error"], (code, out)
        machine.wait_until_succeeds(
            f"sqlite3 /var/lib/mandate-console/console.db \"SELECT count(*) FROM reads WHERE agent_id = '{agent_id}'\" | grep -qv '^0$'"
        )

    with subtest("broken exposure fails closed"):
        machine.succeed("printf '[whatsap]\\n' > /var/lib/mandate-gateway/exposure.toml")
        code, _ = gw("GET", "/v1/whatsapp/chats")
        assert code != 200, code
        machine.succeed("printf '[whatsapp]\\nhide_containers = [\"hidden@s.whatsapp.net\"]\\n' > /var/lib/mandate-gateway/exposure.toml")

    with subtest("unlinked WhatsApp, Discord, Signal, Matrix, Fastmail and Google Messages ask for the owner"):
        code, out = gw("POST", "/v1/whatsapp/send", {"args": {"chat": "fam@g.us", "text": "hi"}})
        assert code == 503 and "https://${host}/connections" in out["error"], (code, out)
        health = json.loads(machine.succeed(connector_get("discord", "health")))
        assert health == {"state": "setup", "detail": "needs a token"}, health
        for name in ["signal", "matrix", "fastmail", "gmessages"]:
            health = json.loads(machine.succeed(connector_get(name, "health")))
            assert health == {"state": "setup", "detail": "not linked"}, (name, health)

    with subtest("http grants are for every allowlisted read: no chats, no horizon"):
        for fields in [{"horizon": "24h"}, {"chats": ["items"], "horizon": "0"}]:
            code, out = console("POST", "/access/challenge", {"kind": "grant", "agent": agent_id, "connector": "http", "for": "30m", **fields})
            assert code == 400, (fields, code, out)
        code, out = gw("POST", "/v1/access/requests", {"connector": "http", "chats": ["items"], "reason": "read items"})
        assert code == 400, (code, out)

    with subtest("the owner sets the API token in the console, which never shows it"):
        grant({"connector": "http", "for": "30m", "horizon": "0"})
        code, _ = console("POST", "/connections/http/setup", {"args": {"api": "${apiToken}"}}, jar="/tmp/nobody")
        assert code == 401, code
        _, out = gw_until("/v1/forward/http/api/items", lambda code, _: code == 503)
        assert "https://${host}/connections" in out["error"], out
        code, out = console("POST", "/connections/http/setup", {"args": {"api": "${apiToken}"}})
        assert code == 200 and out["done"], (code, out)
        code, page = console("GET", "/connections")
        assert code == 200 and "${apiToken}" not in page, code
        assert machine.succeed("stat -c %a /var/lib/mandate-http/private/api").strip() == "600"
        assert recorded() == []

    with subtest("API reads get the token added and the agent's credentials stripped"):
        code, out = gw("GET", "/v1/forward/http/api/items")
        assert code == 200, (code, out)
        [req] = recorded()
        assert req["headers"]["Authorization"] == "Bearer ${apiToken}", req
        assert token not in json.dumps(req), req
        code, _ = gw("POST", "/v1/forward/http/api/items", {"text": "no approval"})
        assert code == 405, code

    with subtest("a write is a proposal until approved, and the owner hears of it"):
        code, p = gw("POST", "/v1/http/request", {"note": "filing it", "args": {
            "service": "api", "method": "POST", "path": "/items", "body": "{\"text\": \"original\"}"}})
        assert code == 202 and set(p) == {"id", "status", "url"} and p["status"] == "pending", (code, p)
        pid = p["id"]
        assert p["url"] == f"https://${host}/r/{pid}", p
        code, p = gw("GET", f"/v1/proposals/{pid}?wait=1")
        assert set(p) == {"id", "status", "url"} and p["status"] == "pending", p
        assert len(recorded()) == 1
        code, page = console("GET", f"/r/{pid}")
        assert code == 200 and "filing it" in page and "Grok Bot" in page, code
        machine.wait_until_succeeds(f"grep -q '/r/{pid}' /var/lib/ntfy/log.jsonl")
        note = recorded("ntfy")[-1]
        assert note["headers"]["Title"] == "Grok Bot" and "Priority" not in note["headers"], note
        assert "original" not in note["body"] and "filing it" not in note["body"], note

    with subtest("the gateway can reach a connector but cannot make it act or change its keys"):
        forged = {"record": base64.b64encode(b"{}").decode(), "nonce": base64.b64encode(b"0123456789abcdef").decode(),
                  "issued_at": "2026-10-31T00:00:00Z",
                  "assertion": {"id": "eA", "rawId": "eA", "type": "public-key", "response": {}}}
        data = base64.b64encode(json.dumps(forged).encode()).decode()
        for method, path, body in [("POST", "execute", data), ("PUT", "keys", base64.b64encode(b"[]").decode()),
                                   ("POST", "setup", base64.b64encode(b"{}").decode()), ("POST", "unlink", "")]:
            code = machine.succeed(
                f"echo {body} | base64 -d | sudo -u mandate-gateway curl -s -o /dev/null -w '%{{http_code}}' -X {method} "
                f"--unix-socket /run/mandate-http/connector.sock --data-binary @- http://connector/{path}"
            )
            assert code == "403", (path, code)
        assert connector_keys("http")["length"] == 1
        assert len(recorded()) == 1
        code = machine.succeed("sudo -u mandate-gateway curl -s -o /dev/null -w '%{http_code}' --unix-socket /run/mandate-http/connector.sock http://connector/health")
        assert code == "200", code

    with subtest("the owner edits and approves with the key; the API gets exactly that, once"):
        code, ch = console("POST", f"/r/{pid}/challenge", {"edits": {"body": "{\"text\": \"edited\"}"}})
        assert code == 200, (code, ch)
        code, out = console("POST", f"/r/{pid}/approve", {"nonce": ch["nonce"], "assertion": assertion(ch["options"], flags=0x01)})
        assert code == 403, (code, out)
        approved = {"nonce": ch["nonce"], "assertion": assertion(ch["options"])}
        code, out = console("POST", f"/r/{pid}/approve", approved)
        assert code == 200, (code, out)
        sent = recorded()[1:]
        assert len(sent) == 1, sent
        assert json.loads(sent[0]["body"]) == {"text": "edited"}, sent
        assert sent[0]["headers"]["Authorization"] == "Bearer ${apiToken}", sent
        code, p = gw("GET", f"/v1/proposals/{pid}")
        assert p["status"] == "done" and "items/1" in p["result"]["output"], p

    with subtest("replaying the approval does nothing, even after restarts"):
        code, _ = console("POST", f"/r/{pid}/approve", approved)
        assert code == 409, code
        machine.succeed("systemctl restart mandate-gateway.service mandate-console.service mandate-http.service")
        machine.wait_for_open_port(443)
        machine.wait_until_succeeds(connector_get("http", "keys") + " | grep -q '\"length\":1'")
        code, _ = console("POST", f"/r/{pid}/approve", approved)
        assert code == 409, code
        assert len(recorded()) == 2

    with subtest("a revoked grant stays revoked, whatever the console says later"):
        machine.wait_for_open_port(8780)
        gw_until(f"/v1/proposals/{pid}", lambda code, _: code == 200)
        machine.succeed(
            "sudo -u mandate-console sqlite3 /var/lib/mandate-console/console.db "
            f"\"UPDATE grants SET revoked_at = NULL, signed = CAST(x'{signed}' AS TEXT) WHERE id = '{scoped}'\""
        )
        offered = json.loads(machine.succeed(
            "sudo -u mandate-gateway curl -s --unix-socket /run/mandate-console/proposals.sock http://console/access"))
        assert any(json.loads(base64.b64decode(r["record"]))["id"] == scoped for r in offered["records"]), offered
        time.sleep(20)
        code, out = gw("GET", "/v1/access")
        assert code == 200 and scoped not in json.dumps(out), (code, out)

    with subtest("generated secrets survive restarts and stay root-only"):
        assert machine.succeed("cat /var/lib/mandate/admin-password").strip() == admin_password
        machine.fail("sudo -u mandate-console cat /var/lib/mandate/admin-password")
        machine.fail("sudo -u mandate-console cat /var/lib/mandate-gateway/access.json")

    with subtest("signing in with the key works; nothing else reaches console state or connector data"):
        code, options = console("POST", "/login/options", {}, jar="/tmp/keyjar")
        code, out = console("POST", "/login", {"assertion": assertion(options)}, jar="/tmp/keyjar")
        assert code == 200, (code, out)
        machine.fail("sudo -u mandate-gateway ls /var/lib/mandate-console")
        machine.fail("sudo -u mandate-gateway cat /var/lib/mandate-http/private/api")
        machine.fail("sudo -u mandate-whatsapp test -w /run/mandate-http/connector.sock")
        # The console lists chats through the connector, not its data.db.
        machine.fail("sudo -u mandate-console cat /var/lib/mandate-whatsapp/data.db")
        for name in ["whatsapp", "discord", "signal", "matrix", "fastmail", "gmessages", "http"]:
            machine.fail(f"sudo -u mandate-console ls /var/lib/mandate-{name}")
            sock = f"/run/mandate-{name}/connector.sock"
            assert machine.succeed(f"stat -c '%G %a' {sock}").strip() == f"mandate-{name}-socket 660", name
            code = machine.succeed(f"sudo -u mandate-console curl -s -o /dev/null -w '%{{http_code}}' --unix-socket {sock} http://connector/health")
            assert code == "200", (name, code)
        containers = "--unix-socket /run/mandate-whatsapp/connector.sock 'http://connector/containers?q=fam@g.us'"
        assert "fam@g.us" in machine.succeed(f"sudo -u mandate-console curl -s {containers}")
        code = machine.succeed(f"sudo -u mandate-gateway curl -s -o /dev/null -w '%{{http_code}}' {containers}")
        assert code == "403", code
        # Reads, files and forwards are the gateway's alone.
        for name, method, path in [("whatsapp", "POST", "read"), ("whatsapp", "POST", "media"), ("http", "GET", "forward/api/items")]:
            code = machine.succeed(
                f"sudo -u mandate-console curl -s -o /dev/null -w '%{{http_code}}' -X {method} -d '{{}}' "
                f"--unix-socket /run/mandate-{name}/connector.sock http://connector/{path}"
            )
            assert code == "403", (name, path, code)
        assert len(recorded()) == 2

    with subtest("one touch opens an unlock window; the next write needs only the session key"):
        def unb64(s):
            return base64.urlsafe_b64decode(s + "=" * (-len(s) % 4))

        def hash_parts(*parts):
            h = hashlib.sha256()
            for p in parts:
                h.update(len(p).to_bytes(8, "big") + p)
            return h.digest()

        # A page's session key, and the passkey's delegation to it until expires.
        def unlock(covers, expires):
            key = ec.generate_private_key(ec.SECP256R1())
            pub = key.public_key().public_bytes(Encoding.X962, PublicFormat.UncompressedPoint)
            at = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(expires))
            challenge = hash_parts(b"mandate-unlock-v1", pub, at.encode(), covers)
            return key, {"public_key": b64(pub), "expires_at": at, "covers": b64(covers),
                         "assertion": assertion({"challenge": b64(challenge), "rpId": "${host}"})}

        # WebCrypto's ECDSA signature: r and s, 32 bytes each.
        def session_sign(key, challenge):
            r, s = decode_dss_signature(key.sign(challenge, ec.ECDSA(hashes.SHA256())))
            return r.to_bytes(32, "big") + s.to_bytes(32, "big")

        def propose(text):
            code, p = gw("POST", "/v1/http/request", {"args": {
                "service": "api", "method": "POST", "path": "/items", "body": json.dumps({"text": text})}})
            assert code == 202, (code, p)
            return p["id"]

        def challenge(pid):
            code, ch = console("POST", f"/r/{pid}/challenge", {"edits": {}}, jar="/tmp/keyjar")
            assert code == 200, (code, ch)
            return ch["nonce"], unb64(ch["options"]["challenge"])

        def approve_in(pid, nonce, delegation, signature):
            return console("POST", f"/r/{pid}/approve", {"nonce": nonce, "delegation": delegation,
                                                         "signature": b64(signature)}, jar="/tmp/keyjar")

        now = int(machine.succeed("date +%s"))
        first, second, third = propose("first"), propose("second"), propose("third")
        nonce, c = challenge(first)
        session, delegation = unlock(c, now + 600)
        code, out = approve_in(first, nonce, delegation, session_sign(session, c))
        assert code == 200 and out["status"] == "done", (code, out)
        nonce, c = challenge(second)
        code, out = approve_in(second, nonce, delegation, session_sign(session, c))
        assert code == 200 and out["status"] == "done", (code, out)

        nonce, c = challenge(third)
        forger = ec.generate_private_key(ec.SECP256R1())
        old_session, expired = unlock(c, now - 1)
        refused = [(delegation, session_sign(forger, c)), (expired, session_sign(old_session, c))]
        for d, sig in refused:
            code, out = approve_in(third, nonce, d, sig)
            assert code == 403, (code, out)

        # The connector refuses them too, even sent by the console's user.
        rec, n, issued = machine.succeed(
            "sqlite3 -separator ' ' /var/lib/mandate-console/console.db "
            f"\"SELECT hex(record), hex(nonce), issued_at FROM challenges WHERE nonce = x'{unb64(nonce).hex()}'\""
        ).split()
        ns = int(issued)
        issued_at = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(ns // 10**9)) + f".{ns % 10**9:09d}Z"

        def std(b):
            return base64.b64encode(b).decode()

        for d, sig in refused:
            body = {"record": std(bytes.fromhex(rec)), "nonce": std(bytes.fromhex(n)), "issued_at": issued_at,
                    "delegation": {"public_key": std(unb64(d["public_key"])), "expires_at": d["expires_at"],
                                   "covers": std(unb64(d["covers"])), "assertion": d["assertion"]},
                    "signature": std(sig)}
            data = base64.b64encode(json.dumps(body).encode()).decode()
            code = machine.succeed(
                f"echo {data} | base64 -d | sudo -u mandate-console curl -s -o /dev/null -w '%{{http_code}}' "
                "--unix-socket /run/mandate-http/connector.sock --data-binary @- http://connector/execute"
            )
            assert code == "403", code

        code, p = gw("GET", f"/v1/proposals/{third}")
        assert p["status"] == "pending", p
        sent = [json.loads(r["body"]) for r in recorded()[2:]]
        assert sent == [{"text": "first"}, {"text": "second"}], sent

    with subtest("a key the console doesn't trust yet cannot approve or sign"):
        laptop, laptop_cred = ec.generate_private_key(ec.SECP256R1()), b"mandate-vm-test-laptop"
        code, options = console("POST", "/keys/options", {"name": "laptop"}, jar="/tmp/keyjar")
        assert code == 200, (code, options)
        code, out = console("POST", "/keys", registration(options, laptop, laptop_cred), jar="/tmp/keyjar")
        assert code == 200 and not out["trusted"], (code, out)
        code, ch = console("POST", f"/r/{third}/challenge", {"edits": {}}, jar="/tmp/keyjar")
        assert code == 200, (code, ch)
        code, out = console("POST", f"/r/{third}/approve",
                            {"nonce": ch["nonce"], "assertion": assertion(ch["options"], key=laptop, cred=laptop_cred)}, jar="/tmp/keyjar")
        assert code == 403, (code, out)
        code, out = sign({"kind": "agent", "name": "Laptop Bot", "token_sha256": hashlib.sha256(b"laptop").hexdigest()},
                         key=laptop, cred=laptop_cred, jar="/tmp/keyjar")
        assert code == 403, (code, out)
        assert len(recorded()) == 4

    with subtest("a trusted key approves the second one, which reaches every connector and signs in"):
        code, options = console("POST", "/login/options", {}, jar="/tmp/laptopjar")
        code, _ = console("POST", "/login", {"assertion": assertion(options, key=laptop, cred=laptop_cred)}, jar="/tmp/laptopjar")
        assert code == 403, code
        code, page = console("GET", "/keys", jar="/tmp/keyjar")
        [change] = re.findall(r'data-change="([^"]+)"', page)
        code, ch = console("POST", f"/keys/changes/{change}/challenge", jar="/tmp/keyjar")
        assert code == 200, (code, ch)
        code, out = console("POST", f"/keys/changes/{change}/approve",
                            {"change": ch["change"], "assertion": assertion(ch["options"])}, jar="/tmp/keyjar")
        assert code == 200, (code, out)
        for name in ["whatsapp", "discord", "http"]:
            machine.wait_until_succeeds(connector_get(name, "keys") + " | grep -q '\"length\":2'")
        code, options = console("POST", "/login/options", {}, jar="/tmp/laptopjar")
        code, out = console("POST", "/login", {"assertion": assertion(options, key=laptop, cred=laptop_cred)}, jar="/tmp/laptopjar")
        assert code == 200, (code, out)

    with subtest("Stop all closes every grant, denies every request, cancels approvals under way, and leaves writes alone"):
        code, ref = gw("POST", "/v1/access/requests", {"connector": "discord", "reason": "look around"})
        assert code == 202, (code, ref)
        # The owner has started approving the request and a +30m when Stop all comes.
        started = []
        for fields in [{"kind": "grant", "request": ref["id"]}, {"kind": "extend", "grant": mail, "for": "30m"}]:
            code, ch = console("POST", "/access/challenge", fields)
            assert code == 200, (fields, code, ch)
            started.append(ch)
        code, _ = console("POST", "/access/stop-all")
        assert code == 303, code
        for ch in started:
            code, out = console("POST", "/access/approve", {"nonce": ch["nonce"], "assertion": assertion(ch["options"])})
            assert code == 409, (code, out)
        for path in ["/v1/whatsapp/chats", "/v1/signal/chats", "/v1/fastmail/chats", "/v1/forward/http/api/items"]:
            gw_until(path, lambda code, _: code == 403)
        code, st = gw("GET", f"/v1/access/requests/{ref['id']}?wait=5")
        assert st["status"] == "denied", (code, st)
        code, p = gw("GET", f"/v1/proposals/{third}")
        assert p["status"] == "pending", p
        code, now = console("GET", "/now.json")
        waiting = json.dumps(now["waiting"])
        assert not now["open"] and ref["id"] not in waiting and third in waiting, now
        assert routine in json.dumps(now["stopped"]), now

    with subtest("writes may follow a grant for a day after it ends, unless it was closed"):
        grant({"connector": "signal", "chats": ["sfam"], "for": "10m", "horizon": "0"})
        closed = grant({"connector": "signal", "chats": ["swork"], "for": "10m", "horizon": "0"})
        grant({"connector": "fastmail", "chats": ["inv"], "for": "10m", "horizon": "0"})
        code, _ = console("POST", f"/access/grants/{closed}/close")
        assert code == 303, code
        gw_until("/v1/signal/chats", lambda code, out: code == 200 and rows(out) == ["sfam"])
        machine.succeed("date -s @$(( $(date +%s) + 7200 ))")
        gw_until("/v1/signal/chats", lambda code, _: code == 403)
        code, out = gw("POST", "/v1/signal/send", {"args": {"chat": "sfam", "text": "on my way"}})
        assert code == 503 and "needs the owner" in out["error"], (code, out)
        code, out = gw("POST", "/v1/signal/send", {"args": {"chat": "swork", "text": "on my way"}})
        assert code == 404 and "unknown chat" in out["error"], (code, out)
        # A reply may answer an email from before the grant ended (seq 2), not one
        # that arrived after (seq 3).
        machine.succeed(
            "sudo -u mandate-fastmail sqlite3 /var/lib/mandate-fastmail/data.db \"INSERT INTO messages "
            "(id, container_id, sender_id, sent_at, body, ingested_at) VALUES ('inv/c', 'inv', 'billing@example.com', "
            "strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), 'late invoice', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))\""
        )
        code, out = gw("POST", "/v1/fastmail/reply", {"args": {"chat": "inv", "seq": "3", "body": "paid"}})
        assert code == 404, (code, out)
        code, out = gw("POST", "/v1/fastmail/reply", {"args": {"chat": "inv", "seq": "2", "body": "paid"}})
        assert code == 503 and "needs the owner" in out["error"], (code, out)

    with subtest("mandate-reset-keys clears every key list and the agents they signed; the password is back"):
        machine.succeed("mandate-reset-keys")
        machine.wait_for_open_port(443)
        for name in ["whatsapp", "discord", "http"]:
            machine.wait_until_succeeds(connector_get(name, "keys") + " | grep -q '\"length\":0'")
        code, out = console("POST", "/login", {"password": admin_password}, jar="/tmp/resetjar")
        assert code == 200, (code, out)
        machine.wait_for_open_port(8780)
        gw_until("/v1/connectors", lambda code, _: code == 401)

    with subtest("outsider cannot reach the gateway or the console"):
        machine.succeed("curl -sf http://127.0.0.1:8780/healthz")
        outsider.fail("curl -sf --max-time 5 http://machine:8780/healthz")
        outsider.fail("curl -sk --max-time 5 https://machine/login")

    with subtest("ACME renewals restart the console; connectors get the time zone"):
        machine.succeed(
            "systemctl show -p ExecStartPost --value acme-order-renew-${host}.service"
            " | grep -o '/nix/store/[^ ;]*-acme-postrun' | xargs grep -q mandate-console.service"
        )
        machine.succeed("systemctl show -p Environment --value mandate-discord | grep -q TZ=Europe/Berlin")

    with subtest("hardened units"):
        for unit in ["mandate-gateway", "mandate-whatsapp", "mandate-discord", "mandate-signal", "mandate-matrix", "mandate-fastmail", "mandate-gmessages", "mandate-console", "mandate-http"]:
            machine.succeed(f"systemctl show -p MemoryDenyWriteExecute --value {unit} | grep -qx yes")
        machine.succeed("systemctl show -p CapabilityBoundingSet --value mandate-console | grep -qx cap_net_bind_service")

    with subtest("connectors reach private addresses only where allowed"):
        for unit in ["mandate-whatsapp", "mandate-discord", "mandate-signal", "mandate-matrix", "mandate-fastmail", "mandate-gmessages", "mandate-http"]:
            deny = machine.succeed(f"systemctl show -p IPAddressDeny --value {unit}").split()
            assert {"127.0.0.0/8", "10.0.0.0/8", "192.168.0.0/16", "100.64.0.0/10", "fc00::/7"} <= set(deny), (unit, deny)
            assert "127.0.0.53/32" in machine.succeed(f"systemctl show -p IPAddressAllow --value {unit}").split(), unit
            machine.succeed(f"systemctl is-active {unit}")
        assert "127.0.0.1/32" in machine.succeed("systemctl show -p IPAddressAllow --value mandate-http").split()

        # The filter belongs to the unit's cgroup, so a curl moved into it is held to it.
        def curl_as(unit):
            return (f"sh -c 'echo $$ > /sys/fs/cgroup/system.slice/{unit}.service/cgroup.procs"
                    " && exec curl -sf -o /dev/null --max-time 5 http://127.0.0.1:9000/items'")
        machine.fail(curl_as("mandate-whatsapp"))
        machine.succeed(curl_as("mandate-http"))
  '';
}
