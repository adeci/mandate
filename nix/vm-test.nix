# Boots every unit under its real sandbox. WhatsApp is unlinked and Discord
# has no network, so both are checked for wiring and health only. A fixture
# data.db is written as WhatsApp's user to prove the gateway reads another
# user's database read-only. A recorder stands in for an HTTP API, and the
# test driver holds a pinned passkey to sign in and approve writes end to end.
{ pkgs, self }:
let
  token = "0123456789abcdef0123456789abcdef0123456789abcdef";
  apiToken = "api-token-0123456789abcdef";
  # Dotted: curl, like the public suffix list, refuses cookies for bare names.
  host = "mandate.example.com";

  fixture = pkgs.writeText "fixture.sql" ''
    INSERT INTO containers (id, kind, name, is_group, updated_at) VALUES
      ('fam@g.us', 'group', 'Family', 1, '2026-10-01T00:00:00.000Z'),
      ('hidden@s.whatsapp.net', 'dm', 'Hidden', 0, '2026-10-01T00:00:00.000Z');
    INSERT INTO messages (id, container_id, sender_id, sent_at, body, ingested_at) VALUES
      ('fam@g.us/1', 'fam@g.us', '1555@s.whatsapp.net', '2026-10-01T12:00:00.000Z', 'dinner at 7?', '2026-10-01T12:00:00.000Z'),
      ('fam@g.us/2', 'fam@g.us', '1555@s.whatsapp.net', '2026-10-01T12:01:00.000Z', 'Your WhatsApp code: 482-913', '2026-10-01T12:01:00.000Z'),
      ('hidden@s.whatsapp.net/3', 'hidden@s.whatsapp.net', '1666@s.whatsapp.net', '2026-10-01T12:02:00.000Z', 'private', '2026-10-01T12:02:00.000Z');
  '';

  tls = pkgs.runCommand "mandate-test-tls" { nativeBuildInputs = [ pkgs.openssl ]; } ''
    mkdir $out
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 3650 \
      -subj /CN=${host} -addext subjectAltName=DNS:${host} \
      -keyout $out/key.pem -out $out/cert.pem
  '';

  # Logs every request to the stand-in API as a JSON line.
  recorder = pkgs.writeText "recorder.py" ''
    import http.server, json

    class H(http.server.BaseHTTPRequestHandler):
        def handle_one(self):
            body = self.rfile.read(int(self.headers.get("Content-Length") or 0)).decode()
            with open("/var/lib/recorder/log.jsonl", "a") as f:
                f.write(json.dumps({"method": self.command, "path": self.path,
                                    "headers": dict(self.headers), "body": body}) + "\n")
            self.send_response(201 if self.command == "POST" else 200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"url": "https://api.example/items/1"}')
        do_GET = do_POST = handle_one
        def log_message(self, *args):
            pass

    http.server.ThreadingHTTPServer(("127.0.0.1", 9000), H).serve_forever()
  '';
in
pkgs.testers.runNixOSTest {
  name = "mandate";

  extraPythonPackages = p: [ p.cryptography ];

  nodes.machine = {
    imports = [ self.nixosModules.default ];
    services.mandate = {
      enable = true;
      tokenFile = "/etc/mandate-token";
      openFirewallOn = "eth1";
      whatsapp.enable = true;
      discord = {
        enable = true;
        tokenFile = "/etc/discord-token";
      };
      console = {
        enable = true;
        hostname = host;
        tlsCertFile = "/etc/mandate-tls/cert.pem";
        tlsKeyFile = "/etc/mandate-tls/key.pem";
        # The driver derives this key from a fixed seed; see testScript.
        approvers = [
          {
            name = "test";
            credential_id = "bWFuZGF0ZS12bS10ZXN0LWNyZWQ";
            public_key = "pQECAyYgASFYIOJvyL4X7KsvqBhltIpjtVLlYmtVYBs5iANG3kiq4oSpIlggOGxuiqaMwfmB5nJhiqWfe1MDJcQy49wlH88_uRiqrKU";
          }
        ];
      };
      http.services.api = {
        baseUrl = "http://127.0.0.1:9000";
        tokenFile = "/etc/api-token";
        reads = [ "GET /items" ];
        writes = [ "POST /items" ];
      };
    };
    environment.etc = {
      "mandate-token".text = token;
      "discord-token".text = "not-a-real-token";
      "api-token".text = apiToken;
      "mandate-tls/cert.pem".source = "${tls}/cert.pem";
      "mandate-tls/key.pem".source = "${tls}/key.pem";
    };
    systemd.services.recorder = {
      wantedBy = [ "multi-user.target" ];
      before = [ "mandate-http-proxy.service" ];
      serviceConfig = {
        ExecStart = "${pkgs.python3}/bin/python3 ${recorder}";
        StateDirectory = "recorder";
      };
    };
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

    from cryptography.hazmat.primitives import hashes
    from cryptography.hazmat.primitives.asymmetric import ec

    start_all()
    for unit in ["recorder", "mandate-gateway", "mandate-whatsapp", "mandate-discord", "mandate-console", "mandate-http-proxy"]:
        machine.wait_for_unit(f"{unit}.service")
    for port in [8780, 443, 9000]:
        machine.wait_for_open_port(port)

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

    def gw(method, path, body=None):
        return curl(f"-X {method} -H 'Authorization: Bearer ${token}' http://127.0.0.1:8780{path}", body)

    # The console only admits localhost and the tailnet, so reach it over loopback.
    def console(method, path, body=None, jar="/tmp/jar"):
        return curl(f"-X {method} -b {jar} -c {jar} --cacert /etc/mandate-tls/cert.pem "
                    f"--resolve ${host}:443:127.0.0.1 https://${host}{path}", body)

    def q(sql):
        code, out = gw("POST", "/v1/query", {"sql": sql})
        assert code == 200, (code, out)
        return out

    def recorded():
        lines = machine.succeed("cat /var/lib/recorder/log.jsonl 2>/dev/null || true").splitlines()
        return [json.loads(line) for line in lines]

    def b64(b):
        return base64.urlsafe_b64encode(b).rstrip(b"=").decode()

    P256_ORDER = 0xFFFFFFFF00000000FFFFFFFFFFFFFFFFBCE6FAADA7179E84F3B9CAC2FC632551
    seed = int.from_bytes(hashlib.sha256(b"mandate-vm-test-passkey").digest(), "big") % P256_ORDER
    passkey = ec.derive_private_key(seed, ec.SECP256R1())
    credential = b"mandate-vm-test-cred"

    # Answers navigator.credentials.get the way a phone would.
    def assertion(options, flags=0x05):
        client = json.dumps({"type": "webauthn.get", "challenge": options["challenge"],
                             "origin": "https://${host}", "crossOrigin": False}).encode()
        auth = hashlib.sha256(options["rpId"].encode()).digest() + bytes([flags]) + (1).to_bytes(4, "big")
        sig = passkey.sign(auth + hashlib.sha256(client).digest(), ec.ECDSA(hashes.SHA256()))
        return {"id": b64(credential), "rawId": b64(credential), "type": "public-key",
                "response": {"authenticatorData": b64(auth), "clientDataJSON": b64(client), "signature": b64(sig)}}

    with subtest("fixture written as the source user is 0640"):
        machine.succeed(
            "cd /var/lib/mandate-whatsapp && sudo -u mandate-whatsapp sh -c "
            "'umask 0027 && sqlite3 data.db < ${../internal/store/schema.sql} && sqlite3 data.db < ${fixture}'"
        )
        machine.succeed("stat -c %a /var/lib/mandate-whatsapp/data.db | grep -qx 640")

    with subtest("exposure and auth filtering"):
        machine.succeed("printf '[whatsapp]\\nhide_containers = [\"hidden@s.whatsapp.net\"]\\n' > /var/lib/mandate-gateway/exposure.toml")
        rows = q("SELECT id, body, is_auth FROM whatsapp_messages ORDER BY seq")["rows"]
        assert rows == [["fam@g.us/1", "dinner at 7?", 0], ["fam@g.us/2", None, 1]], rows

    with subtest("broken exposure fails closed, then recovers"):
        machine.succeed("printf '[whatsap]\\nhide_containers = []\\n' > /var/lib/mandate-gateway/exposure.toml")
        code, _ = gw("POST", "/v1/query", {"sql": "SELECT 1"})
        assert code != 200, code
        machine.succeed("printf '[whatsapp]\\nhide_containers = [\"hidden@s.whatsapp.net\"]\\n' > /var/lib/mandate-gateway/exposure.toml")
        assert q("SELECT count(*) FROM whatsapp_messages")["rows"] == [[2]]

    with subtest("unauthenticated and bypass attempts fail"):
        machine.fail("curl -sf -d '{\"sql\":\"SELECT 1\"}' http://127.0.0.1:8780/v1/query")
        code, _ = gw("POST", "/v1/query", {"sql": "SELECT * FROM src_whatsapp.messages"})
        assert code != 200, code

    with subtest("gateway user can read but not write source data or sessions"):
        machine.succeed("sudo -u mandate-gateway sqlite3 'file:/var/lib/mandate-whatsapp/data.db?mode=ro' 'SELECT count(*) FROM messages'")
        machine.fail("sudo -u mandate-gateway sqlite3 /var/lib/mandate-whatsapp/data.db 'DELETE FROM messages'")
        machine.fail("sudo -u mandate-gateway touch /var/lib/mandate-whatsapp/x")
        machine.succeed("stat -c %a /var/lib/mandate-whatsapp/private | grep -qx 700")
        machine.fail("sudo -u mandate-gateway ls /var/lib/mandate-whatsapp/private")

    with subtest("unlinked WhatsApp asks for the owner instead of failing"):
        code, schema = gw("GET", "/v1/schema")
        health = {c["name"]: c["health"]["state"] for c in schema["connectors"]}
        assert health["whatsapp"] == "setup" and health["http"] == "ok", health
        code, out = gw("POST", "/v1/actions", {"connector": "whatsapp", "op": "send", "args": {"to": "fam@g.us", "text": "hi"}})
        assert code == 503 and "https://${host}/connections" in out["error"], (code, out)

    with subtest("discord: socket reachable only by the gateway and console, hidden/unknown refused locally"):
        machine.succeed("sudo -u mandate-gateway test -w /run/mandate-discord/connector.sock")
        machine.succeed("sudo -u mandate-console test -w /run/mandate-discord/connector.sock")
        machine.fail("sudo -u mandate-whatsapp test -w /run/mandate-discord/connector.sock")
        # Outside Discord's active hours the gateway answers "resting" first.
        code, out = gw("POST", "/v1/read/discord/fetch", {"args": {"channel_id": "123456789012345678"}})
        assert code == 404 or (code == 429 and "resting" in out["error"]), (code, out)

    with subtest("API reads get the token added and the agent's credentials stripped"):
        code, out = gw("GET", "/v1/http/api/items")
        assert code == 200, (code, out)
        [req] = recorded()
        assert req["headers"]["Authorization"] == "Bearer ${apiToken}", req
        assert "${token}" not in json.dumps(req), req
        code, _ = gw("GET", "/v1/http/api/admin")
        assert code == 403, code
        code, _ = gw("POST", "/v1/http/api/items", {"text": "no approval"})
        assert code == 405, code
        assert len(recorded()) == 1

    with subtest("a write is only a proposal until approved"):
        code, p = gw("POST", "/v1/actions", {"connector": "http", "op": "request", "note": "filing it",
                     "args": {"service": "api", "method": "POST", "path": "/items", "body": "{\"text\": \"original\"}"}})
        assert code == 202 and p["status"] == "pending", (code, p)
        pid = p["id"]
        assert p["url"] == f"https://${host}/r/{pid}", p
        code, p = gw("GET", f"/v1/actions/{pid}?wait=1")
        assert p["status"] == "pending", p
        assert len(recorded()) == 1

    with subtest("the console admits nobody without a passkey"):
        code, _ = console("GET", "/", jar="/tmp/nobody")
        assert code == 303, code
        code, _ = console("POST", f"/r/{pid}/deny", {}, jar="/tmp/nobody")
        assert code == 401, code
        code, _ = console("POST", f"/r/{pid}/challenge", {"edits": {}}, jar="/tmp/nobody")
        assert code == 401, code
        code, options = console("POST", "/login/options", {}, jar="/tmp/nobody")
        code, _ = console("POST", "/login", {"assertion": assertion(options, flags=0x01)}, jar="/tmp/nobody")
        assert code == 403 or code == 401, code
        code, p = gw("GET", f"/v1/actions/{pid}")
        assert p["status"] == "pending", p

    with subtest("the gateway can reach the connector but cannot make it act"):
        # Signed by nobody: only the console's records carry a real passkey signature.
        forged = {"record": base64.b64encode(b"{}").decode(), "nonce": base64.b64encode(b"0123456789abcdef").decode(),
                  "issued_at": "2026-10-31T00:00:00Z",
                  "assertion": {"id": "eA", "rawId": "eA", "type": "public-key", "response": {}}}
        data = base64.b64encode(json.dumps(forged).encode()).decode()
        code = machine.succeed(
            f"echo {data} | base64 -d | sudo -u mandate-gateway curl -s -o /dev/null -w '%{{http_code}}' "
            "--unix-socket /run/mandate-http-proxy/connector.sock --data-binary @- http://connector/execute"
        )
        assert code == "403", code
        assert len(recorded()) == 1

    with subtest("the owner signs in, edits, and approves; the API gets exactly that, once"):
        code, options = console("POST", "/login/options", {})
        assert code == 200, (code, options)
        code, out = console("POST", "/login", {"assertion": assertion(options)})
        assert code == 200, (code, out)
        code, page = console("GET", f"/r/{pid}")
        assert code == 200 and "filing it" in page, code

        code, ch = console("POST", f"/r/{pid}/challenge", {"edits": {"body": "{\"text\": \"edited\"}"}})
        assert code == 200, (code, ch)
        assert any("edited" in f["value"] for f in ch["display"]), ch["display"]
        code, out = console("POST", f"/r/{pid}/approve", {"nonce": ch["nonce"], "assertion": assertion(ch["options"], flags=0x01)})
        assert code == 403, (code, out)
        approved = {"nonce": ch["nonce"], "assertion": assertion(ch["options"])}
        code, out = console("POST", f"/r/{pid}/approve", approved)
        assert code == 200, (code, out)

        sent = recorded()[1:]
        assert len(sent) == 1, sent
        assert sent[0]["method"] == "POST" and sent[0]["path"] == "/items", sent
        assert json.loads(sent[0]["body"]) == {"text": "edited"}, sent
        assert sent[0]["headers"]["Authorization"] == "Bearer ${apiToken}", sent
        code, p = gw("GET", f"/v1/actions/{pid}")
        assert p["status"] == "done" and "items/1" in p["result"]["output"], p

    with subtest("replaying the approval does nothing, even after restarts"):
        code, _ = console("POST", f"/r/{pid}/approve", approved)
        assert code == 409, code
        machine.succeed("systemctl restart mandate-console.service mandate-http-proxy.service")
        machine.wait_for_open_port(443)
        machine.wait_for_unit("mandate-http-proxy.service")
        code, _ = console("POST", f"/r/{pid}/approve", approved)
        assert code == 409, code
        assert len(recorded()) == 2

    with subtest("passkeys come from root or a signed-in owner, and nothing else reaches console state"):
        out = machine.succeed("mandate passkey add --name laptop")
        [url] = [w for w in out.split() if w.startswith("https://${host}/enroll/")]
        machine.succeed(f"curl -sf --cacert /etc/mandate-tls/cert.pem --resolve ${host}:443:127.0.0.1 {url} >/dev/null")
        machine.fail("sudo -u mandate-gateway mandate passkey add --name x")
        machine.fail("sudo -u mandate-gateway ls /var/lib/mandate-console")
        machine.fail("sudo -u mandate-gateway cat /run/credentials/mandate-http-proxy.service/api")
        machine.fail("sudo -u mandate-whatsapp test -w /run/mandate-http-proxy/connector.sock")

    with subtest("outsider cannot reach the gateway or the console"):
        machine.succeed("curl -sf http://127.0.0.1:8780/healthz")
        outsider.fail("curl -sf --max-time 5 http://machine:8780/healthz")
        outsider.fail("curl -sk --max-time 5 https://machine/login")

    with subtest("hardened units"):
        for unit in ["mandate-gateway", "mandate-whatsapp", "mandate-discord", "mandate-console", "mandate-http-proxy"]:
            machine.succeed(f"systemctl show -p MemoryDenyWriteExecute --value {unit} | grep -qx yes")
        machine.succeed("systemctl show -p CapabilityBoundingSet --value mandate-console | grep -qx cap_net_bind_service")
  '';
}
