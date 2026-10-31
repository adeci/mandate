# Boots the module with the gateway, WhatsApp, and Discord sources. WhatsApp is
# unpaired, so its unit must stay down; a fixture data.db is written as its
# user to prove the gateway reads another user's database read-only under the
# real sandbox. Discord runs without network, so only local wiring is checked.
{ pkgs, self }:
let
  token = "0123456789abcdef0123456789abcdef0123456789abcdef";
  fixture = pkgs.writeText "fixture.sql" ''
    INSERT INTO containers (id, kind, name, is_group, updated_at) VALUES
      ('fam@g.us', 'group', 'Family', 1, '2026-10-01T00:00:00.000Z'),
      ('hidden@s.whatsapp.net', 'dm', 'Hidden', 0, '2026-10-01T00:00:00.000Z');
    INSERT INTO messages (id, container_id, sender_id, sent_at, body, ingested_at) VALUES
      ('fam@g.us/1', 'fam@g.us', '1555@s.whatsapp.net', '2026-10-01T12:00:00.000Z', 'dinner at 7?', '2026-10-01T12:00:00.000Z'),
      ('fam@g.us/2', 'fam@g.us', '1555@s.whatsapp.net', '2026-10-01T12:01:00.000Z', 'Your WhatsApp code: 482-913', '2026-10-01T12:01:00.000Z'),
      ('hidden@s.whatsapp.net/3', 'hidden@s.whatsapp.net', '1666@s.whatsapp.net', '2026-10-01T12:02:00.000Z', 'private', '2026-10-01T12:02:00.000Z');
  '';
in
pkgs.testers.runNixOSTest {
  name = "mandate";

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
    };
    environment.etc."mandate-token".text = token;
    environment.etc."discord-token".text = "not-a-real-token";
    environment.systemPackages = [
      pkgs.sqlite
      pkgs.curl
    ];
  };

  # Not on the tailnet: reaches the open port but IPAddressAllow must drop it.
  nodes.outsider = {
    environment.systemPackages = [ pkgs.curl ];
  };

  testScript = ''
    import json

    start_all()
    machine.wait_for_unit("mandate-gateway.service")
    machine.wait_for_unit("mandate-discord.service")
    machine.wait_for_open_port(8780)

    def q(sql):
        body = json.dumps({"sql": sql})
        assert "'" not in body, "keep test SQL free of single quotes"
        return json.loads(machine.succeed(
            "curl -sf -H 'Authorization: Bearer ${token}' -d '" + body + "' http://127.0.0.1:8780/v1/query"
        ))

    with subtest("unpaired source stays down instead of crash-looping"):
        machine.succeed("systemctl show -p ActiveState --value mandate-whatsapp.service | grep -qx inactive")

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
        machine.succeed("printf '[whatsapp]\\nhide_container = []\\n' > /var/lib/mandate-gateway/exposure.toml")
        machine.fail("curl -sf -H 'Authorization: Bearer ${token}' -d '{\"sql\":\"SELECT 1\"}' http://127.0.0.1:8780/v1/query")
        machine.succeed("printf '[whatsapp]\\nhide_containers = [\"hidden@s.whatsapp.net\"]\\n' > /var/lib/mandate-gateway/exposure.toml")
        assert q("SELECT count(*) FROM whatsapp_messages")["rows"] == [[2]]

    with subtest("unauthenticated and bypass attempts fail"):
        machine.fail("curl -sf -d '{\"sql\":\"SELECT 1\"}' http://127.0.0.1:8780/v1/query")
        machine.fail(
            "curl -sf -H 'Authorization: Bearer ${token}' -d '{\"sql\":\"SELECT * FROM src_whatsapp.messages\"}' http://127.0.0.1:8780/v1/query"
        )

    with subtest("gateway user can read but not write source data or sessions"):
        machine.succeed("sudo -u mandate-gateway sqlite3 'file:/var/lib/mandate-whatsapp/data.db?mode=ro' 'SELECT count(*) FROM messages'")
        machine.fail("sudo -u mandate-gateway sqlite3 /var/lib/mandate-whatsapp/data.db 'DELETE FROM messages'")
        machine.fail("sudo -u mandate-gateway touch /var/lib/mandate-whatsapp/x")
        machine.succeed("stat -c %a /var/lib/mandate-whatsapp/private | grep -qx 700")
        machine.succeed("sudo -u mandate-whatsapp sqlite3 /var/lib/mandate-whatsapp/private/session.db 'PRAGMA journal_mode=WAL; CREATE TABLE k(x)'")
        machine.fail("sudo -u mandate-gateway ls /var/lib/mandate-whatsapp/private")

    with subtest("discord: socket reachable only by the gateway, hidden/unknown refused locally"):
        machine.succeed("test -S /run/mandate-discord/api.sock")
        machine.succeed("sudo -u mandate-gateway test -w /run/mandate-discord/api.sock")
        machine.fail("sudo -u mandate-whatsapp test -w /run/mandate-discord/api.sock")
        out = machine.succeed(
            "curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer ${token}' "
            "-d '{\"channel_id\":\"123456789012345678\"}' http://127.0.0.1:8780/v1/discord/fetch"
        )
        assert out == "404", out

    with subtest("outsider cannot reach the gateway"):
        machine.succeed("curl -sf http://127.0.0.1:8780/healthz")
        outsider.fail("curl -sf --max-time 5 http://machine:8780/healthz")

    with subtest("hardened units"):
        for unit in ["mandate-gateway", "mandate-discord"]:
            machine.succeed(f"systemctl show -p MemoryDenyWriteExecute --value {unit} | grep -qx yes")
        machine.succeed("systemctl show -p RestartPreventExitStatus --value mandate-discord | grep -q 3")
  '';
}
