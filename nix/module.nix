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
  exe = lib.getExe cfg.package;
  state = name: "/var/lib/mandate-${name}";
  socket = unit: "/run/mandate-${unit}/connector.sock";
  proposalSocket = "/run/mandate-console/proposals.sock";

  # %d (the credentials directory) must reach systemd unescaped, so credential
  # paths are appended after escapeSystemdExecArgs, which would turn it into %%d.
  credential = flag: name: " ${flag} %d/${name}";

  # Browsers omit the default port from the origin a passkey signs.
  origin =
    "https://${cfg.console.hostname}"
    + lib.optionalString (cfg.console.port != 443) ":${toString cfg.console.port}";
  approvers = pkgs.writeText "mandate-approvers.json" (builtins.toJSON cfg.console.approvers);

  # Connectors verify every approval themselves; without these flags they
  # refuse all writes.
  verifyArgs =
    seen:
    lib.optionals cfg.console.enable [
      "--approvers"
      approvers
      "--rp-id"
      cfg.console.hostname
      "--origin"
      origin
      "--seen"
      seen
    ];

  # Only the tailnet and localhost may connect.
  tailnetOnly = {
    IPAddressAllow = [
      "localhost"
      "100.64.0.0/10"
      "fd7a:115c:a1e0::/48"
    ];
    IPAddressDeny = "any";
  };

  # Enabled sources: unit/user name -> gateway source name and ExecStart args.
  sources = lib.filterAttrs (name: _: cfg.${name}.enable) {
    whatsapp = {
      source = "whatsapp";
      args = [
        "whatsapp"
        "run"
        "--data"
        "${state "whatsapp"}/data.db"
        "--session"
        "${state "whatsapp"}/private/session.db"
        "--listen"
        (socket "whatsapp")
      ]
      ++ verifyArgs "${state "whatsapp"}/private/seen.json";
    };
    discord = {
      source = "discord";
      args = [
        "discord"
        "serve"
        "--data"
        "${state "discord"}/data.db"
        "--state"
        "${state "discord"}/private/state.json"
        "--listen"
        (socket "discord")
      ]
      ++ lib.optionals (config.time.timeZone != null) [
        "--timezone"
        config.time.timeZone
      ];
    };
  };

  httpProxy = cfg.enable && cfg.http.services != { };

  # Connector name -> the unit serving it.
  connectors =
    lib.mapAttrs' (unit: src: lib.nameValuePair src.source { inherit unit; }) sources
    // lib.optionalAttrs httpProxy { http.unit = "http-proxy"; };
  connectorArgs = lib.concatMap (name: [
    "--connector"
    "${name}=${socket connectors.${name}.unit}"
  ]) (lib.attrNames connectors);
  connectorGroups = map (c: "mandate-${c.unit}") (lib.attrValues connectors);
  connectorUnits = map (c: "mandate-${c.unit}.service") (lib.attrValues connectors);

  httpConfig = pkgs.writeText "mandate-http.json" (
    builtins.toJSON (
      lib.mapAttrsToList (name: s: {
        inherit name;
        base_url = s.baseUrl;
        token = {
          header = s.tokenHeader;
          prefix = s.tokenPrefix;
          file = name;
        };
        reads = map rule s.reads;
        writes = map rule s.writes;
      }) cfg.http.services
    )
  );
  rule =
    r:
    let
      parts = lib.splitString " " r;
    in
    {
      method = lib.elemAt parts 0;
      path = lib.elemAt parts 1;
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

  # A unit that serves a connector socket as its own user.
  connectorService =
    unit: description: extra:
    lib.recursiveUpdate {
      inherit description;
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      wantedBy = [ "multi-user.target" ];
      startLimitIntervalSec = 0;
      serviceConfig = hardening // {
        User = "mandate-${unit}";
        Group = "mandate-${unit}";
        StateDirectory = "mandate-${unit}";
        RuntimeDirectory = "mandate-${unit}";
        RuntimeDirectoryMode = "0750";
        Restart = "always";
        RestartSec = "30s";
      };
    } extra;
in
{
  options.services.mandate = {
    enable = lib.mkEnableOption "the mandate gateway (and the connectors enabled below)";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.mandate;
      defaultText = lib.literalExpression "mandate.packages.\${system}.mandate";
      description = "The mandate package.";
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

    console = {
      enable = lib.mkEnableOption "the console, where you link accounts and approve each write with a passkey";

      hostname = lib.mkOption {
        type = lib.types.str;
        example = "mandate.example.com";
        description = ''
          Hostname of the console, which is also the passkey relying party:
          changing it invalidates every enrolled passkey.
        '';
      };

      port = lib.mkOption {
        type = lib.types.port;
        default = 443;
        description = "Console port (HTTPS).";
      };

      tlsCertFile = lib.mkOption {
        type = lib.types.externalPath;
        description = "TLS certificate for the hostname. Loaded via LoadCredential; restart mandate-console after renewing.";
      };

      tlsKeyFile = lib.mkOption {
        type = lib.types.externalPath;
        description = "TLS key for the hostname. Loaded via LoadCredential.";
      };

      approvers = lib.mkOption {
        type = lib.types.listOf (
          lib.types.submodule {
            options = {
              name = lib.mkOption { type = lib.types.str; };
              credential_id = lib.mkOption { type = lib.types.str; };
              public_key = lib.mkOption { type = lib.types.str; };
            };
          }
        );
        default = [ ];
        example = lib.literalExpression "lib.importJSON ./approvers.json";
        description = ''
          Passkeys that may sign in and approve writes, as shown by the
          console after adding one. Keys are trusted only from here, so a
          compromised console can't add its own.
        '';
      };
    };

    whatsapp.enable = lib.mkEnableOption "the WhatsApp connector (linked from the console)";
    discord = {
      enable = lib.mkEnableOption "the Discord connector (on-demand reads with a user token)";
      tokenFile = lib.mkOption {
        type = lib.types.externalPath;
        example = "/run/secrets/discord-token";
        description = "Discord user token, outside the Nix store. Loaded via LoadCredential.";
      };
    };

    http.services = lib.mkOption {
      type = lib.types.attrsOf (
        lib.types.submodule {
          options = {
            baseUrl = lib.mkOption {
              type = lib.types.str;
              example = "https://api.github.com";
            };
            tokenFile = lib.mkOption {
              type = lib.types.externalPath;
              description = "API token, outside the Nix store. Loaded via LoadCredential.";
            };
            tokenHeader = lib.mkOption {
              type = lib.types.str;
              default = "Authorization";
            };
            tokenPrefix = lib.mkOption {
              type = lib.types.str;
              default = "Bearer ";
            };
            reads = lib.mkOption {
              type = lib.types.listOf (lib.types.strMatching "(GET|HEAD) /[^ ]*");
              default = [ ];
              example = [ "GET /repos/*/*/issues" ];
              description = "GET or HEAD requests agents may make directly. `*` matches one path segment, a trailing `**` the rest.";
            };
            writes = lib.mkOption {
              type = lib.types.listOf (lib.types.strMatching "[A-Z]+ /[^ ]*");
              default = [ ];
              example = [ "POST /repos/*/*/issues" ];
              description = "Requests agents may propose; each one waits for your approval.";
            };
          };
        }
      );
      default = { };
      description = "Token APIs agents use through the gateway without ever seeing the token.";
    };
  };

  config = lib.mkMerge [
    (lib.mkIf (cfg.enable || sources != { }) {
      environment.systemPackages = [ cfg.package ];

      users.users = lib.mapAttrs' (
        name: _:
        lib.nameValuePair "mandate-${name}" (systemUser "mandate-${name}" [ ] // { home = state name; })
      ) sources;
      users.groups = lib.mapAttrs' (name: _: lib.nameValuePair "mandate-${name}" { }) sources;

      systemd.services = lib.mapAttrs' (
        name: src:
        lib.nameValuePair "mandate-${name}" (
          connectorService name "mandate ${name} connector" {
            serviceConfig = {
              StateDirectoryMode = "0750";
              WorkingDirectory = state name;
              ExecStart =
                utils.escapeSystemdExecArgs ([ exe ] ++ src.args)
                + lib.optionalString (name == "discord") (credential "--token-file" "token");
            }
            // lib.optionalAttrs (name == "discord") {
              LoadCredential = [ "token:${cfg.discord.tokenFile}" ];
            };
          }
        )
      ) sources;

      # data.db is readable by the gateway's group. Session state, including
      # SQLite -wal/-shm files that ignore file modes, lives in private/,
      # which only the connector's user can traverse.
      systemd.tmpfiles.rules = lib.concatLists (
        lib.mapAttrsToList (name: _: [
          "d ${state name} 0750 mandate-${name} mandate-${name} -"
          "d ${state name}/private 0700 mandate-${name} mandate-${name} -"
        ]) sources
      );
    })

    (lib.mkIf cfg.enable {
      # Group membership grants read access to each source's data.db (0640)
      # and connector sockets (0660).
      users.users.mandate-gateway = systemUser "mandate-gateway" (
        connectorGroups ++ lib.optional cfg.console.enable "mandate-console"
      );
      users.groups.mandate-gateway = { };

      systemd.services.mandate-gateway = {
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
                ++ lib.concatLists (
                  lib.mapAttrsToList (name: src: [
                    "--source"
                    "${src.source}=${state name}/data.db"
                  ]) sources
                )
                ++ connectorArgs
                ++ lib.optionals cfg.console.enable [
                  "--console-socket"
                  proposalSocket
                  "--console-url"
                  origin
                ]
                ++ lib.optionals httpProxy [
                  "--http-proxy-socket"
                  (socket "http-proxy")
                ]
              )
              + credential "--token-file" "token";
            Restart = "always";
            RestartSec = "5s";
          };
      };

      networking.firewall.interfaces = lib.mkIf (cfg.openFirewallOn != null) {
        ${cfg.openFirewallOn}.allowedTCPPorts = [
          cfg.port
        ]
        ++ lib.optional cfg.console.enable cfg.console.port;
      };
    })

    (lib.mkIf (cfg.enable && cfg.console.enable) {
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
            # Sessions and decided proposals are the console's alone; the
            # gateway only reaches the proposal socket.
            StateDirectory = "mandate-console";
            StateDirectoryMode = "0700";
            RuntimeDirectory = "mandate-console";
            RuntimeDirectoryMode = "0750";
            AmbientCapabilities = [ "CAP_NET_BIND_SERVICE" ];
            CapabilityBoundingSet = [ "CAP_NET_BIND_SERVICE" ];
            LoadCredential = [
              "tls-cert:${cfg.console.tlsCertFile}"
              "tls-key:${cfg.console.tlsKeyFile}"
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
                  "--approvers"
                  approvers
                  "--socket"
                  proposalSocket
                  "--admin-socket"
                  "/run/mandate-console/admin.sock"
                ]
                ++ connectorArgs
              )
              + credential "--tls-cert" "tls-cert"
              + credential "--tls-key" "tls-key";
            Restart = "always";
            RestartSec = "5s";
          };
      };
    })

    (lib.mkIf httpProxy {
      users.users.mandate-http-proxy = systemUser "mandate-http-proxy" [ ];
      users.groups.mandate-http-proxy = { };

      systemd.services.mandate-http-proxy = connectorService "http-proxy" "mandate HTTP API proxy" {
        serviceConfig = {
          StateDirectoryMode = "0700";
          LoadCredential = lib.mapAttrsToList (name: s: "${name}:${s.tokenFile}") cfg.http.services;
          # Reads token files from $CREDENTIALS_DIRECTORY.
          ExecStart = utils.escapeSystemdExecArgs (
            [
              exe
              "http-proxy"
              "--config"
              httpConfig
              "--listen"
              (socket "http-proxy")
            ]
            ++ verifyArgs "${state "http-proxy"}/seen.json"
          );
        };
      };
    })
  ];
}
