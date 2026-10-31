# Boots every unit under its real sandbox. WhatsApp is unlinked and Discord
# has no network, so both are checked for wiring only. A recorder stands in
# for an HTTP API, and the test driver plays the owner's passkey.
{ pkgs, self }:
let
  token = "0123456789abcdef0123456789abcdef0123456789abcdef";
  apiToken = "api-token-0123456789abcdef";
  adminPassword = "correct horse battery staple";
  # Dotted: curl, like the public suffix list, refuses cookies for bare names.
  host = "mandate.example.com";

  adminPasswordHash =
    pkgs.runCommand "mandate-test-admin-hash" { nativeBuildInputs = [ pkgs.mkpasswd ]; }
      ''
        printf %s '${adminPassword}' | mkpasswd -m bcrypt -R 4 -s | tr -d '\n' > $out
      '';

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

  extraPythonPackages = p: [
    p.cryptography
    p.cbor2
  ];

  nodes.machine = {
    imports = [ self.nixosModules.default ];
    services.mandate = {
      enable = true;
      tokenFile = "/etc/mandate-token";
      openFirewallOn = "eth1";
      connectors = {
        whatsapp = { };
        discord.credentials.token = "/etc/discord-token";
        http = {
          settings.services.api = {
            base_url = "http://127.0.0.1:9000";
            reads = [ "GET /items" ];
            writes = [ "POST /items" ];
          };
          credentials.api = "/etc/api-token";
        };
      };
      console = {
        enable = true;
        hostname = host;
        tlsCertFile = "/etc/mandate-tls/cert.pem";
        tlsKeyFile = "/etc/mandate-tls/key.pem";
        adminPasswordHashFile = "/etc/mandate-admin-hash";
      };
    };
    environment.etc = {
      "mandate-token".text = token;
      "discord-token".text = "not-a-real-token";
      "api-token".text = apiToken;
      "mandate-admin-hash".source = adminPasswordHash;
      "mandate-tls/cert.pem".source = "${tls}/cert.pem";
      "mandate-tls/key.pem".source = "${tls}/key.pem";
    };
    systemd.services.recorder = {
      wantedBy = [ "multi-user.target" ];
      before = [ "mandate-http.service" ];
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

    import cbor2
    from cryptography.hazmat.primitives import hashes
    from cryptography.hazmat.primitives.asymmetric import ec

    start_all()
    for unit in ["recorder", "mandate-gateway", "mandate-whatsapp", "mandate-discord", "mandate-console", "mandate-http"]:
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

    def connector_keys(name):
        return json.loads(machine.succeed(f"curl -s --unix-socket /run/mandate-{name}/connector.sock http://connector/keys"))

    def recorded():
        lines = machine.succeed("cat /var/lib/recorder/log.jsonl 2>/dev/null || true").splitlines()
        return [json.loads(line) for line in lines]

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
    def registration(options):
        n = passkey.public_key().public_numbers()
        cose = cbor2.dumps({1: 2, 3: -7, -1: 1, -2: n.x.to_bytes(32, "big"), -3: n.y.to_bytes(32, "big")})
        auth = (hashlib.sha256(options["rp"]["id"].encode()).digest() + bytes([0x45]) + (0).to_bytes(4, "big")
                + bytes(16) + len(credential).to_bytes(2, "big") + credential + cose)
        attestation = cbor2.dumps({"fmt": "none", "attStmt": {}, "authData": auth})
        return {"id": b64(credential), "rawId": b64(credential), "type": "public-key",
                "response": {"clientDataJSON": b64(client_data("webauthn.create", options["challenge"])),
                             "attestationObject": b64(attestation)}}

    # Answers navigator.credentials.get.
    def assertion(options, flags=0x05):
        cd = client_data("webauthn.get", options["challenge"])
        auth = hashlib.sha256(options["rpId"].encode()).digest() + bytes([flags]) + (1).to_bytes(4, "big")
        sig = passkey.sign(auth + hashlib.sha256(cd).digest(), ec.ECDSA(hashes.SHA256()))
        return {"id": b64(credential), "rawId": b64(credential), "type": "public-key",
                "response": {"authenticatorData": b64(auth), "clientDataJSON": b64(cd), "signature": b64(sig)}}

    with subtest("reads are per connector, filtered, and refuse names"):
        machine.succeed(
            "cd /var/lib/mandate-whatsapp && sudo -u mandate-whatsapp sh -c "
            "'umask 0027 && sqlite3 data.db < ${../store/schema.sql} && sqlite3 data.db < ${fixture}'"
        )
        machine.succeed("printf '[whatsapp]\\nhide_containers = [\"hidden@s.whatsapp.net\"]\\n' > /var/lib/mandate-gateway/exposure.toml")
        code, out = gw("GET", "/v1/whatsapp/chats")
        assert code == 200 and [r[0] for r in out["rows"]] == ["fam@g.us"], (code, out)
        code, out = gw("GET", "/v1/whatsapp/messages?chat=fam@g.us")
        assert [r[-1] for r in out["rows"]] == [None, "dinner at 7?"], out
        code, out = gw("GET", "/v1/whatsapp/messages?chat=hidden@s.whatsapp.net")
        assert code == 404, (code, out)
        code, out = gw("GET", "/v1/whatsapp/messages?chat=Family")
        assert code == 404 and "chats --search" in out["error"], (code, out)
        machine.fail("curl -sf http://127.0.0.1:8780/v1/whatsapp/chats")

    with subtest("broken exposure fails closed"):
        machine.succeed("printf '[whatsap]\\n' > /var/lib/mandate-gateway/exposure.toml")
        code, _ = gw("GET", "/v1/whatsapp/chats")
        assert code != 200, code
        machine.succeed("printf '[whatsapp]\\nhide_containers = [\"hidden@s.whatsapp.net\"]\\n' > /var/lib/mandate-gateway/exposure.toml")

    with subtest("unlinked WhatsApp asks for the owner"):
        code, out = gw("POST", "/v1/whatsapp/send", {"args": {"chat": "fam@g.us", "text": "hi"}})
        assert code == 503 and "https://${host}/connections" in out["error"], (code, out)

    with subtest("API reads get the token added and the agent's credentials stripped"):
        code, out = gw("GET", "/v1/forward/http/api/items")
        assert code == 200, (code, out)
        [req] = recorded()
        assert req["headers"]["Authorization"] == "Bearer ${apiToken}", req
        assert "${token}" not in json.dumps(req), req
        code, _ = gw("POST", "/v1/forward/http/api/items", {"text": "no approval"})
        assert code == 405, code

    with subtest("a write is a proposal until approved"):
        code, p = gw("POST", "/v1/http/request", {"note": "filing it", "args": {
            "service": "api", "method": "POST", "path": "/items", "body": "{\"text\": \"original\"}"}})
        assert code == 202 and p["status"] == "pending", (code, p)
        pid = p["id"]
        assert p["url"] == f"https://${host}/r/{pid}", p
        code, p = gw("GET", f"/v1/proposals/{pid}?wait=1")
        assert p["status"] == "pending", p
        assert len(recorded()) == 1

    with subtest("the console admits the admin password, not guesses"):
        code, _ = console("GET", "/", jar="/tmp/nobody")
        assert code == 303, code
        code, _ = console("POST", "/login", {"password": "wrong"}, jar="/tmp/nobody")
        assert code in (401, 403), code
        code, out = console("POST", "/login", {"password": "${adminPassword}"})
        assert code == 200, (code, out)
        code, page = console("GET", f"/r/{pid}")
        assert code == 200 and "filing it" in page, code

    with subtest("a key the console doesn't trust cannot approve"):
        code, ch = console("POST", f"/r/{pid}/challenge", {"edits": {}})
        assert code == 200, (code, ch)
        code, out = console("POST", f"/r/{pid}/approve", {"nonce": ch["nonce"], "assertion": assertion(ch["options"])})
        assert code == 403, (code, out)
        assert len(recorded()) == 1

    with subtest("the first key is trusted at once and reaches every connector"):
        code, options = console("POST", "/keys/options", {"name": "yubikey"})
        assert code == 200, (code, options)
        code, out = console("POST", "/keys", registration(options))
        assert code == 200 and out["trusted"], (code, out)
        for name in ["whatsapp", "discord", "http"]:
            machine.wait_until_succeeds(f"curl -s --unix-socket /run/mandate-{name}/connector.sock http://connector/keys | grep -q '\"length\":1'")

    with subtest("the gateway can reach a connector but cannot make it act or change its keys"):
        forged = {"record": base64.b64encode(b"{}").decode(), "nonce": base64.b64encode(b"0123456789abcdef").decode(),
                  "issued_at": "2026-10-31T00:00:00Z",
                  "assertion": {"id": "eA", "rawId": "eA", "type": "public-key", "response": {}}}
        data = base64.b64encode(json.dumps(forged).encode()).decode()
        code = machine.succeed(
            f"echo {data} | base64 -d | sudo -u mandate-gateway curl -s -o /dev/null -w '%{{http_code}}' "
            "--unix-socket /run/mandate-http/connector.sock --data-binary @- http://connector/execute"
        )
        assert code == "403", code
        code = machine.succeed(
            "echo '[]' | sudo -u mandate-gateway curl -s -o /dev/null -w '%{http_code}' -X PUT "
            "--unix-socket /run/mandate-http/connector.sock --data-binary @- http://connector/keys"
        )
        assert code == "409", code
        assert connector_keys("http")["length"] == 1
        assert len(recorded()) == 1

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
        machine.succeed("systemctl restart mandate-console.service mandate-http.service")
        machine.wait_for_open_port(443)
        machine.wait_until_succeeds("curl -s --unix-socket /run/mandate-http/connector.sock http://connector/keys | grep -q '\"length\":1'")
        code, _ = console("POST", f"/r/{pid}/approve", approved)
        assert code == 409, code
        assert len(recorded()) == 2

    with subtest("signing in with the key works; nothing else reaches console state"):
        code, options = console("POST", "/login/options", {}, jar="/tmp/keyjar")
        code, out = console("POST", "/login", {"assertion": assertion(options)}, jar="/tmp/keyjar")
        assert code == 200, (code, out)
        machine.fail("sudo -u mandate-gateway ls /var/lib/mandate-console")
        machine.fail("sudo -u mandate-gateway cat /run/credentials/mandate-http.service/api")
        machine.fail("sudo -u mandate-whatsapp test -w /run/mandate-http/connector.sock")

    with subtest("mandate-reset-keys clears every key list"):
        machine.succeed("mandate-reset-keys")
        machine.wait_for_open_port(443)
        for name in ["whatsapp", "discord", "http"]:
            machine.wait_until_succeeds(f"curl -s --unix-socket /run/mandate-{name}/connector.sock http://connector/keys | grep -q '\"length\":0'")

    with subtest("outsider cannot reach the gateway or the console"):
        machine.succeed("curl -sf http://127.0.0.1:8780/healthz")
        outsider.fail("curl -sf --max-time 5 http://machine:8780/healthz")
        outsider.fail("curl -sk --max-time 5 https://machine/login")

    with subtest("hardened units"):
        for unit in ["mandate-gateway", "mandate-whatsapp", "mandate-discord", "mandate-console", "mandate-http"]:
            machine.succeed(f"systemctl show -p MemoryDenyWriteExecute --value {unit} | grep -qx yes")
        machine.succeed("systemctl show -p CapabilityBoundingSet --value mandate-console | grep -qx cap_net_bind_service")
  '';
}
