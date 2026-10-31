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
  proposalSocket = "/run/mandate-approvals/proposals.sock";

  # %d (the credentials directory) must reach systemd unescaped, so credential
  # paths are appended after escapeSystemdExecArgs, which would turn it into %%d.
  credential = flag: name: " ${flag} %d/${name}";

  # internal/exitcode.NeedsOwner: systemd leaves the unit failed instead of looping.
  needsOwner = 3;

  # Browsers omit the default port from the origin a passkey signs.
  origin =
    "https://${cfg.approvals.hostname}"
    + lib.optionalString (cfg.approvals.port != 443) ":${toString cfg.approvals.port}";
  approvers = pkgs.writeText "mandate-approvers.json" (builtins.toJSON cfg.approvals.approvers);

  # Connectors verify every approval themselves; without these flags they
  # refuse all writes.
  verifyArgs =
    seen:
    lib.optionals cfg.approvals.enable [
      "--approvers"
      approvers
      "--rp-id"
      cfg.approvals.hostname
      "--origin"
      origin
      "--seen"
      seen
    ];

  # Enabled sources: unit/user name -> gateway source name, ExecStart args,
  # the session file whose absence means "not paired yet", and whether the
  # unit serves a connector socket.
  sources = lib.filterAttrs (name: _: cfg.${name}.enable) {
    whatsapp = {
      source = "whatsapp";
      session = "private/session.db";
      connector = true;
      writes = true;
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
    messages = {
      source = "sms";
      session = "private/session.json";
      connector = false;
      writes = false;
      args = [
        "messages"
        "run"
        "--data"
        "${state "messages"}/data.db"
        "--session"
        "${state "messages"}/private/session.json"
      ];
    };
    discord = {
      source = "discord";
      session = null;
      connector = true;
      writes = false;
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
    lib.mapAttrs' (
      unit: src:
      lib.nameValuePair src.source {
        inherit unit;
        inherit (src) writes;
      }
    ) (lib.filterAttrs (_: src: src.connector) sources)
    // lib.optionalAttrs httpProxy {
      http = {
        unit = "http-proxy";
        writes = true;
      };
    };
  writers = lib.filterAttrs (_: c: c.writes) connectors;
  connectorArgs =
    cs:
    lib.concatMap (name: [
      "--connector"
      "${name}=${socket cs.${name}.unit}"
    ]) (lib.attrNames cs);
  groupsOf = cs: map (c: "mandate-${c.unit}") (lib.attrValues cs);

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
  ruleType = lib.types.strMatching "[A-Z]+ /[^ ]*";

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

  sourceService = name: src: {
    description = "mandate ${name} source";
    after = [ "network-online.target" ];
    wants = [ "network-online.target" ];
    wantedBy = [ "multi-user.target" ];
    startLimitIntervalSec = 0;
    # Not started until paired: run `mandate <name> pair` as mandate-<name> first.
    unitConfig = lib.optionalAttrs (src.session != null) {
      ConditionPathExists = "${state name}/${src.session}";
    };
    serviceConfig =
      hardening
      // {
        User = "mandate-${name}";
        Group = "mandate-${name}";
        StateDirectory = "mandate-${name}";
        StateDirectoryMode = "0750";
        WorkingDirectory = state name;
        ExecStart =
          utils.escapeSystemdExecArgs ([ exe ] ++ src.args)
          + lib.optionalString (name == "discord") (credential "--token-file" "token");
        Restart = "always";
        RestartSec = "30s";
        RestartPreventExitStatus = needsOwner;
      }
      // lib.optionalAttrs src.connector {
        RuntimeDirectory = "mandate-${name}";
        RuntimeDirectoryMode = "0750";
      }
      // lib.optionalAttrs (name == "discord") {
        LoadCredential = [ "token:${cfg.discord.tokenFile}" ];
      };
  };
in
{
  options.services.mandate = {
    enable = lib.mkEnableOption "the mandate gateway (and the sources enabled below)";

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
      description = "Interface to open the gateway and approval ports on; null leaves the firewall alone.";
    };

    approvals = {
      enable = lib.mkEnableOption "writes: agents propose, you approve each one with a passkey";

      hostname = lib.mkOption {
        type = lib.types.str;
        example = "box.example.ts.net";
        description = ''
          Hostname of the approval page, which is also the passkey relying
          party: changing it invalidates every enrolled passkey.
        '';
      };

      port = lib.mkOption {
        type = lib.types.port;
        default = 8781;
        description = "Approval page port (HTTPS).";
      };

      tlsCertFile = lib.mkOption {
        type = lib.types.externalPath;
        description = "TLS certificate for the hostname. Loaded via LoadCredential; restart mandate-approvals after renewing.";
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
          Passkeys that may approve writes, as printed by
          `mandate approver enroll`. Empty means nothing can be approved.
        '';
      };

      ntfy = {
        url = lib.mkOption {
          type = lib.types.nullOr lib.types.str;
          default = null;
          example = "https://ntfy.example.com";
          description = "ntfy server for approval pushes; null disables them.";
        };
        topic = lib.mkOption {
          type = lib.types.str;
          default = "mandate";
          description = "ntfy topic.";
        };
        tokenFile = lib.mkOption {
          type = lib.types.nullOr lib.types.externalPath;
          default = null;
          description = "ntfy access token that may publish to the topic.";
        };
      };
    };

    whatsapp.enable = lib.mkEnableOption "the WhatsApp source (linked device)";
    messages.enable = lib.mkEnableOption "the Google Messages (SMS/RCS) source";
    discord = {
      enable = lib.mkEnableOption "the Discord source (on-demand reads with a user token)";
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
              type = lib.types.listOf ruleType;
              default = [ ];
              example = [ "GET /repos/*/*/issues" ];
              description = "Requests agents may make directly. `*` matches one path segment, a trailing `**` the rest.";
            };
            writes = lib.mkOption {
              type = lib.types.listOf ruleType;
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
        name: src: lib.nameValuePair "mandate-${name}" (sourceService name src)
      ) sources;

      # `pair` runs before StateDirectory exists. Session state (incl. SQLite
      # -wal/-shm files, which ignore file modes) lives in private/, which the
      # gateway cannot traverse.
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
        map (name: "mandate-${name}") (lib.attrNames sources)
        ++ groupsOf (lib.filterAttrs (_: c: c.unit == "http-proxy") connectors)
        ++ lib.optional cfg.approvals.enable "mandate-approvals"
      );
      users.groups.mandate-gateway = { };

      systemd.services.mandate-gateway = {
        description = "mandate gateway";
        after = [ "network.target" ] ++ map (c: "mandate-${c.unit}.service") (lib.attrValues connectors);
        wantedBy = [ "multi-user.target" ];
        serviceConfig = hardening // {
          User = "mandate-gateway";
          Group = "mandate-gateway";
          StateDirectory = "mandate-gateway";
          StateDirectoryMode = "0750";
          LoadCredential = [ "token:${cfg.tokenFile}" ];
          # Only the tailnet and localhost may connect; no egress at all.
          IPAddressAllow = [
            "localhost"
            "100.64.0.0/10"
            "fd7a:115c:a1e0::/48"
          ];
          IPAddressDeny = "any";
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
              ++ connectorArgs connectors
              ++ lib.optionals cfg.approvals.enable [
                "--approvals-socket"
                proposalSocket
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
        ++ lib.optional cfg.approvals.enable cfg.approvals.port;
      };
    })

    (lib.mkIf (cfg.enable && cfg.approvals.enable) {
      users.users.mandate-approvals = systemUser "mandate-approvals" (groupsOf writers);
      users.groups.mandate-approvals = { };

      systemd.services.mandate-approvals = {
        description = "mandate approvals";
        after = [
          "network-online.target"
        ]
        ++ map (c: "mandate-${c.unit}.service") (lib.attrValues writers);
        wants = [ "network-online.target" ];
        wantedBy = [ "multi-user.target" ];
        serviceConfig = hardening // {
          User = "mandate-approvals";
          Group = "mandate-approvals";
          # Proposals and the audit log are this unit's alone; the gateway
          # only reaches the proposal socket.
          StateDirectory = "mandate-approvals";
          StateDirectoryMode = "0700";
          RuntimeDirectory = "mandate-approvals";
          RuntimeDirectoryMode = "0750";
          LoadCredential = [
            "tls-cert:${cfg.approvals.tlsCertFile}"
            "tls-key:${cfg.approvals.tlsKeyFile}"
          ]
          ++ lib.optional (cfg.approvals.ntfy.tokenFile != null) "ntfy-token:${cfg.approvals.ntfy.tokenFile}";
          ExecStart =
            utils.escapeSystemdExecArgs (
              [
                exe
                "approvals"
                "--hostname"
                cfg.approvals.hostname
                "--https-listen"
                ":${toString cfg.approvals.port}"
                "--state"
                (state "approvals")
                "--approvers"
                approvers
                "--socket"
                proposalSocket
                "--admin-socket"
                "/run/mandate-approvals/admin.sock"
              ]
              ++ connectorArgs writers
              ++ lib.optionals (cfg.approvals.ntfy.url != null) [
                "--ntfy-url"
                cfg.approvals.ntfy.url
                "--ntfy-topic"
                cfg.approvals.ntfy.topic
              ]
            )
            + credential "--tls-cert" "tls-cert"
            + credential "--tls-key" "tls-key"
            + lib.optionalString (cfg.approvals.ntfy.tokenFile != null) (
              credential "--ntfy-token-file" "ntfy-token"
            );
          Restart = "always";
          RestartSec = "5s";
        };
      };
    })

    (lib.mkIf httpProxy {
      users.users.mandate-http-proxy = systemUser "mandate-http-proxy" [ ];
      users.groups.mandate-http-proxy = { };

      systemd.services.mandate-http-proxy = {
        description = "mandate HTTP API proxy";
        after = [ "network-online.target" ];
        wants = [ "network-online.target" ];
        wantedBy = [ "multi-user.target" ];
        serviceConfig = hardening // {
          User = "mandate-http-proxy";
          Group = "mandate-http-proxy";
          StateDirectory = "mandate-http-proxy";
          StateDirectoryMode = "0700";
          RuntimeDirectory = "mandate-http-proxy";
          RuntimeDirectoryMode = "0750";
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
          Restart = "always";
          RestartSec = "5s";
        };
      };
    })
  ];
}
