self:
{
  config,
  lib,
  pkgs,
  utils,
  ...
}:
let
  cfg = config.services.mandate;
  system = pkgs.stdenv.hostPlatform.system;
  exe = lib.getExe cfg.package;
  json = pkgs.formats.json { };
  state = name: "/var/lib/mandate-${name}";
  socket = name: "/run/mandate-${name}/connector.sock";
  proposalSocket = "/run/mandate-console/proposals.sock";

  # %d (the credentials directory) must reach systemd unescaped, so credential
  # paths are appended after escapeSystemdExecArgs, which would turn it into %%d.
  credential = flag: name: " ${flag} %d/${name}";

  # Browsers omit the default port from the origin a passkey signs.
  origin =
    "https://${cfg.console.hostname}"
    + lib.optionalString (cfg.console.port != 443) ":${toString cfg.console.port}";

  # Deletes every key so the admin password can add a first one again.
  resetKeys = pkgs.writeShellApplication {
    name = "mandate-reset-keys";
    text = ''
      units=(mandate-console ${
        lib.concatMapStringsSep " " (n: "mandate-${n}") (lib.attrNames connectors)
      })
      systemctl stop "''${units[@]}"
      rm -f ${state "console"}/keys.json ${
        lib.concatMapStringsSep " " (n: "${state n}/private/keys.json") (lib.attrNames connectors)
      }
      systemctl start "''${units[@]}"
    '';
  };

  # Only the tailnet and localhost may connect.
  tailnetOnly = {
    IPAddressAllow = [
      "localhost"
      "100.64.0.0/10"
      "fd7a:115c:a1e0::/48"
    ];
    IPAddressDeny = "any";
  };

  hardening = {
    NoNewPrivileges = true;
    ProtectSystem = "strict";
    ProtectHome = true;
    PrivateTmp = true;
    PrivateDevices = true;
    ProtectKernelTunables = true;
    ProtectKernelModules = true;
    ProtectKernelLogs = true;
    ProtectControlGroups = true;
    ProtectClock = true;
    ProtectHostname = true;
    ProtectProc = "invisible";
    ProcSubset = "pid";
    RestrictNamespaces = true;
    RestrictRealtime = true;
    RestrictSUIDSGID = true;
    RemoveIPC = true;
    LockPersonality = true;
    MemoryDenyWriteExecute = true;
    SystemCallArchitectures = "native";
    SystemCallFilter = [
      "@system-service"
      "~@privileged"
    ];
    CapabilityBoundingSet = "";
    RestrictAddressFamilies = [
      "AF_UNIX"
      "AF_INET"
      "AF_INET6"
    ];
    UMask = "0027";
  };

  systemUser = name: extraGroups: {
    isSystemUser = true;
    group = name;
    inherit extraGroups;
  };

  connectors = cfg.connectors;
  connectorGroups = map (name: "mandate-${name}") (lib.attrNames connectors);
  connectorUnits = map (name: "mandate-${name}.service") (lib.attrNames connectors);
  connectorArgs = lib.concatMap (name: [
    "--connector"
    "${name}=${socket name}"
  ]) (lib.attrNames connectors);

  configFile = name: c: json.generate "mandate-${name}.json" c.settings;

  # Checks connector settings at build time.
  configCheck =
    name: c:
    pkgs.runCommand "mandate-${name}-config-check" { } ''
      ${lib.getExe c.package} --check-config ${configFile name c}
      touch $out
    '';

  connectorService = name: c: {
    description = "mandate ${name} connector";
    after = [ "network-online.target" ];
    wants = [ "network-online.target" ];
    wantedBy = [ "multi-user.target" ];
    startLimitIntervalSec = 0;
    environment = lib.optionalAttrs (config.time.timeZone != null) { TZ = config.time.timeZone; };
    serviceConfig =
      hardening
      // {
        User = "mandate-${name}";
        Group = "mandate-${name}";
        # The gateway's group can read data.db; sessions stay in private/.
        StateDirectory = "mandate-${name}";
        StateDirectoryMode = "0750";
        RuntimeDirectory = "mandate-${name}";
        RuntimeDirectoryMode = "0750";
        LoadCredential = lib.mapAttrsToList (cred: path: "${cred}:${path}") c.credentials;
        ExecStart = utils.escapeSystemdExecArgs (
          [
            (lib.getExe c.package)
            "--socket"
            (socket name)
            "--state"
            (state name)
            "--config"
            (configFile name c)
          ]
          # Lets connectors verify approvals.
          ++ lib.optionals cfg.console.enable [
            "--rp-id"
            cfg.console.hostname
            "--origin"
            origin
          ]
        );
        Restart = "always";
        RestartSec = "30s";
      }
      // c.serviceConfig;
  };

  connectorOptions =
    { name, ... }:
    {
      options = {
        package = lib.mkOption {
          type = lib.types.package;
          default =
            self.packages.${system}."mandate-${name}"
              or (throw "services.mandate.connectors.${name}.package must be set: ${name} isn't a built-in connector");
          defaultText = lib.literalMD "the built-in connector of that name (`whatsapp`, `discord`, `http`)";
          description = "The connector program.";
        };
        settings = lib.mkOption {
          inherit (json) type;
          default = { };
          description = "Connector settings, passed as JSON and validated by the connector at build time.";
        };
        credentials = lib.mkOption {
          type = lib.types.attrsOf lib.types.externalPath;
          default = { };
          example = {
            token = "/run/secrets/discord-token";
          };
          description = "Secrets the connector reads by name, as files outside the Nix store. Loaded via LoadCredential.";
        };
        serviceConfig = lib.mkOption {
          type = lib.types.attrsOf lib.types.anything;
          default = { };
          description = "systemd service settings merged over the defaults.";
        };
      };
    };
in
{
  options.services.mandate = {
    enable = lib.mkEnableOption "mandate: the gateway, the console and the connectors below";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${system}.mandate;
      defaultText = lib.literalExpression "mandate.packages.\${system}.mandate";
      description = "The mandate package (gateway, console and agent CLI).";
    };

    tokenFile = lib.mkOption {
      type = lib.types.externalPath;
      example = "/run/secrets/mandate-token";
      description = ''
        Bearer token for the gateway (at least 32 characters), outside the
        Nix store. Loaded via LoadCredential.
      '';
    };

    port = lib.mkOption {
      type = lib.types.port;
      default = 8780;
      description = "Gateway port.";
    };

    openFirewallOn = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = "tailscale0";
      description = "Interface to open the gateway and console ports on; null leaves the firewall alone.";
    };

    connectors = lib.mkOption {
      type = lib.types.attrsOf (lib.types.submodule connectorOptions);
      default = { };
      example = lib.literalExpression ''
        {
          whatsapp = { };
          discord.credentials.token = "/run/secrets/discord-token";
          signal.package = inputs.mandate-signal.packages.''${system}.default;
        }
      '';
      description = ''
        Connectors by name. Each runs as its own user with its own credentials.
      '';
    };

    console = {
      enable = lib.mkEnableOption "the console, where you link connectors and approve writes";

      hostname = lib.mkOption {
        type = lib.types.str;
        example = "mandate.example.com";
        description = ''
          Console hostname. Passkeys are bound to it.
        '';
      };

      port = lib.mkOption {
        type = lib.types.port;
        default = 443;
        description = "Console port (HTTPS).";
      };

      tlsCertFile = lib.mkOption {
        type = lib.types.externalPath;
        description = "TLS certificate for the hostname. Loaded via LoadCredential; restart mandate-console after renewing it.";
      };

      tlsKeyFile = lib.mkOption {
        type = lib.types.externalPath;
        description = "TLS key for the hostname. Loaded via LoadCredential.";
      };

      adminPasswordHashFile = lib.mkOption {
        type = lib.types.externalPath;
        example = "/run/secrets/mandate-admin-password-hash";
        description = "bcrypt hash of the admin password. Loaded via LoadCredential.";
      };
    };
  };

  config = lib.mkIf cfg.enable (
    lib.mkMerge [
      {
        assertions = map (name: {
          assertion = builtins.match "[a-z][a-z0-9-]*" name != null;
          message = "services.mandate.connectors.${name}: names are lowercase letters, digits and dashes";
        }) (lib.attrNames connectors);

        environment.systemPackages = [ cfg.package ] ++ lib.optional cfg.console.enable resetKeys;
        system.checks = lib.mapAttrsToList configCheck connectors;

        users.users =
          lib.mapAttrs' (
            name: _:
            lib.nameValuePair "mandate-${name}" (systemUser "mandate-${name}" [ ] // { home = state name; })
          ) connectors
          // {
            # Group membership grants read access to each source's data.db (0640)
            # and every connector socket (0660).
            mandate-gateway = systemUser "mandate-gateway" (
              connectorGroups ++ lib.optional cfg.console.enable "mandate-console"
            );
          };
        users.groups = lib.mapAttrs' (name: _: lib.nameValuePair "mandate-${name}" { }) connectors // {
          mandate-gateway = { };
        };

        systemd.services =
          lib.mapAttrs' (name: c: lib.nameValuePair "mandate-${name}" (connectorService name c)) connectors
          // {
            mandate-gateway = {
              description = "mandate gateway";
              after = [ "network.target" ] ++ connectorUnits;
              wantedBy = [ "multi-user.target" ];
              serviceConfig =
                hardening
                // tailnetOnly
                // {
                  User = "mandate-gateway";
                  Group = "mandate-gateway";
                  StateDirectory = "mandate-gateway";
                  StateDirectoryMode = "0750";
                  LoadCredential = [ "token:${cfg.tokenFile}" ];
                  ExecStart =
                    utils.escapeSystemdExecArgs (
                      [
                        exe
                        "gateway"
                        "--listen"
                        ":${toString cfg.port}"
                        "--exposure"
                        "${state "gateway"}/exposure.toml"
                      ]
                      # A connector is a source once it has written a data.db.
                      ++ lib.concatMap (name: [
                        "--source"
                        "${name}=${state name}/data.db"
                      ]) (lib.attrNames connectors)
                      ++ connectorArgs
                      ++ lib.optionals cfg.console.enable [
                        "--console-socket"
                        proposalSocket
                        "--console-url"
                        origin
                      ]
                    )
                    + credential "--token-file" "token";
                  Restart = "always";
                  RestartSec = "5s";
                };
            };
          };

        networking.firewall.interfaces = lib.mkIf (cfg.openFirewallOn != null) {
          ${cfg.openFirewallOn}.allowedTCPPorts = [
            cfg.port
          ]
          ++ lib.optional cfg.console.enable cfg.console.port;
        };
      }

      (lib.mkIf cfg.console.enable {
        users.users.mandate-console = systemUser "mandate-console" connectorGroups;
        users.groups.mandate-console = { };

        systemd.services.mandate-console = {
          description = "mandate console";
          after = [ "network.target" ] ++ connectorUnits;
          wantedBy = [ "multi-user.target" ];
          serviceConfig =
            hardening
            // tailnetOnly
            // {
              User = "mandate-console";
              Group = "mandate-console";
              StateDirectory = "mandate-console";
              StateDirectoryMode = "0700";
              RuntimeDirectory = "mandate-console";
              RuntimeDirectoryMode = "0750";
              AmbientCapabilities = [ "CAP_NET_BIND_SERVICE" ];
              CapabilityBoundingSet = [ "CAP_NET_BIND_SERVICE" ];
              LoadCredential = [
                "tls-cert:${cfg.console.tlsCertFile}"
                "tls-key:${cfg.console.tlsKeyFile}"
                "admin-password-hash:${cfg.console.adminPasswordHashFile}"
              ];
              ExecStart =
                utils.escapeSystemdExecArgs (
                  [
                    exe
                    "console"
                    "--hostname"
                    cfg.console.hostname
                    "--listen"
                    ":${toString cfg.console.port}"
                    "--state"
                    (state "console")
                    "--socket"
                    proposalSocket
                  ]
                  ++ connectorArgs
                )
                + credential "--tls-cert" "tls-cert"
                + credential "--tls-key" "tls-key"
                + credential "--admin-password-hash-file" "admin-password-hash";
              Restart = "always";
              RestartSec = "5s";
            };
        };
      })
    ]
  );
}
