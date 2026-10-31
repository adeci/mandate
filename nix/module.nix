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
  # %d (the credentials directory) must reach systemd unescaped, so it is
  # appended after escapeSystemdExecArgs, which would turn it into %%d.
  credentialArg = " --token-file %d/token";

  # internal/exitcode.NeedsOwner: systemd leaves the unit failed instead of looping.
  needsOwner = 3;

  # Enabled sources: unit/user name -> gateway source name, ExecStart args,
  # and the session file whose absence means "not paired yet".
  sources = lib.filterAttrs (name: _: cfg.${name}.enable) {
    whatsapp = {
      source = "whatsapp";
      session = "private/session.db";
      args = [
        "whatsapp"
        "run"
        "--data"
        "${state "whatsapp"}/data.db"
        "--session"
        "${state "whatsapp"}/private/session.db"
      ];
    };
    messages = {
      source = "sms";
      session = "private/session.json";
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
      args = [
        "discord"
        "serve"
        "--data"
        "${state "discord"}/data.db"
        "--state"
        "${state "discord"}/private/state.json"
        "--listen"
        "/run/mandate-discord/api.sock"
      ]
      ++ lib.optionals (config.time.timeZone != null) [
        "--timezone"
        config.time.timeZone
      ];
    };
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
          + lib.optionalString (name == "discord") credentialArg;
        Restart = "always";
        RestartSec = "30s";
        RestartPreventExitStatus = needsOwner;
      }
      // lib.optionalAttrs (name == "discord") {
        LoadCredential = [ "token:${cfg.discord.tokenFile}" ];
        RuntimeDirectory = "mandate-discord";
        RuntimeDirectoryMode = "0750";
      };
  };
in
{
  options.services.mandate = {
    enable = lib.mkEnableOption "the mandate read-only SQL gateway (and the sources enabled below)";

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
      description = "Interface to open the gateway port on; null leaves the firewall alone.";
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
  };

  config = lib.mkMerge [
    (lib.mkIf (cfg.enable || sources != { }) {
      environment.systemPackages = [ cfg.package ];

      users.users = lib.mapAttrs' (
        name: _:
        lib.nameValuePair "mandate-${name}" {
          isSystemUser = true;
          group = "mandate-${name}";
          home = state name;
        }
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
      users.users.mandate-gateway = {
        isSystemUser = true;
        group = "mandate-gateway";
        # Read access to each source's data.db (0640) and the Discord socket.
        extraGroups = map (name: "mandate-${name}") (lib.attrNames sources);
      };
      users.groups.mandate-gateway = { };

      systemd.services.mandate-gateway = {
        description = "mandate read-only SQL gateway";
        after = [ "network.target" ] ++ lib.optional (sources ? discord) "mandate-discord.service";
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
              ++ lib.optionals (sources ? discord) [
                "--discord-socket"
                "/run/mandate-discord/api.sock"
              ]
            )
            + credentialArg;
          Restart = "always";
          RestartSec = "5s";
        };
      };

      networking.firewall.interfaces = lib.mkIf (cfg.openFirewallOn != null) {
        ${cfg.openFirewallOn}.allowedTCPPorts = [ cfg.port ];
      };
    })
  ];
}
