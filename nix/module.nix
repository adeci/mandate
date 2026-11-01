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
  names = lib.attrNames cfg.connectors;
  connectorGroups = map (n: "mandate-${n}") names;
  accounts = [
    "mandate-gateway"
    "mandate-console"
  ]
  ++ connectorGroups;
  state = name: "/var/lib/mandate-${name}";
  socket = name: "/run/mandate-${name}/connector.sock";
  proposalSocket = "/run/mandate-console/proposals.sock";
  connectorArgs = lib.concatMap (n: [
    "--connector"
    "${n}=${socket n}"
  ]) names;

  generated = "/var/lib/mandate";
  tokenFile = if cfg.tokenFile != null then cfg.tokenFile else "${generated}/token";
  hashFile =
    if cfg.adminPasswordHashFile != null then
      cfg.adminPasswordHashFile
    else
      "${generated}/admin-password-hash";
  secretsUnit = lib.optional (
    cfg.tokenFile == null || cfg.adminPasswordHashFile == null
  ) "mandate-secrets.service";
  acmeUnit = lib.optional (cfg.useACMEHost != null) "acme-${cfg.useACMEHost}.service";
  acmeDir = config.security.acme.certs.${cfg.useACMEHost}.directory;
  connectorUnits = map (g: "${g}.service") connectorGroups;

  # Browsers omit the default port from the origin a passkey signs.
  origin =
    "https://${cfg.hostname}"
    + lib.optionalString (cfg.consolePort != 443) ":${toString cfg.consolePort}";

  # %d (the credentials directory) must reach systemd unescaped, so credential
  # paths are appended after escapeSystemdExecArgs, which would turn it into %%d.
  credential = flag: name: " ${flag} %d/${name}";

  # Deletes every key so the admin password can add a first one again.
  resetKeys = pkgs.writeShellApplication {
    name = "mandate-reset-keys";
    text = ''
      units=(mandate-console ${toString connectorGroups})
      systemctl stop "''${units[@]}"
      rm -f ${state "console"}/keys.json ${
        lib.concatMapStringsSep " " (n: "${state n}/private/keys.json") names
      }
      systemctl start "''${units[@]}"
    '';
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

  service = name: serviceConfig: {
    description = "mandate ${name}";
    wantedBy = [ "multi-user.target" ];
    serviceConfig =
      hardening
      // {
        User = "mandate-${name}";
        Group = "mandate-${name}";
        # The gateway's group can read a connector's data.db; sessions stay in private/.
        StateDirectory = "mandate-${name}";
        StateDirectoryMode = "0750";
        RuntimeDirectory = "mandate-${name}";
        RuntimeDirectoryMode = "0750";
        Restart = "always";
        RestartSec = "5s";
      }
      // serviceConfig;
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

  configFile = name: c: json.generate "mandate-${name}.json" c.settings;

  connectorService =
    name: c:
    service name (
      {
        ExecStart = utils.escapeSystemdExecArgs [
          (lib.getExe c.package)
          "--socket"
          (socket name)
          "--state"
          (state name)
          "--config"
          (configFile name c)
          "--rp-id"
          cfg.hostname
          "--origin"
          origin
        ];
        RestartSec = "30s";
      }
      // c.serviceConfig
    )
    // {
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      startLimitIntervalSec = 0;
      environment = lib.optionalAttrs (config.time.timeZone != null) { TZ = config.time.timeZone; };
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
          defaultText = lib.literalMD "the built-in connector of that name (`whatsapp`, `signal`, `matrix`, `discord`, `fastmail`, `http`)";
          description = "The connector program.";
        };
        settings = lib.mkOption {
          inherit (json) type;
          default = { };
          description = "Connector settings, passed as JSON and validated by the connector at build time.";
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
    enable = lib.mkEnableOption "mandate";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${system}.mandate;
      defaultText = lib.literalExpression "mandate.packages.\${system}.mandate";
      description = "The mandate package (gateway, console and agent CLI).";
    };

    hostname = lib.mkOption {
      type = lib.types.str;
      example = "mandate.example.com";
      description = "Hostname of the console, where you link connectors and approve writes. Passkeys are bound to it.";
    };

    useACMEHost = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "mandate.example.com";
      description = ''
        Name of a {option}`security.acme.certs` entry to serve the console
        with. The console restarts when it renews.
      '';
    };

    tlsCertFile = lib.mkOption {
      type = lib.types.externalPath;
      description = ''
        TLS certificate chain for the hostname. Defaults to the one of
        {option}`services.mandate.useACMEHost`. Read at start, so restart
        mandate-console after renewing it.
      '';
    };

    tlsKeyFile = lib.mkOption {
      type = lib.types.externalPath;
      description = "TLS key for the hostname. Defaults to the one of {option}`services.mandate.useACMEHost`.";
    };

    tokenFile = lib.mkOption {
      type = lib.types.nullOr lib.types.externalPath;
      default = null;
      example = "/run/secrets/mandate-token";
      description = ''
        File with the bearer token agents use, at least 32 characters.
        If null, one is generated on first start; read it with
        `sudo cat ${generated}/token`.
      '';
    };

    adminPasswordHashFile = lib.mkOption {
      type = lib.types.nullOr lib.types.externalPath;
      default = null;
      example = "/run/secrets/mandate-admin-password-hash";
      description = ''
        bcrypt hash of the console's admin password, which signs in to add
        the first passkey. If null, a password is generated on first start;
        read it with `sudo cat ${generated}/admin-password`.
      '';
    };

    port = lib.mkOption {
      type = lib.types.port;
      default = 8780;
      description = "Gateway port, for agents.";
    };

    consolePort = lib.mkOption {
      type = lib.types.port;
      default = 443;
      description = "Console port (HTTPS).";
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
          discord = { };
          webhook.package = inputs.mandate-webhook.packages.''${system}.default;
        }
      '';
      description = "Connectors by name. Each runs as its own user and is linked on the console's Connections page.";
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = map (name: {
      assertion =
        builtins.match "[a-z][a-z0-9-]*" name != null
        && !lib.elem name [
          "gateway"
          "console"
          "secrets"
        ];
      message = "services.mandate.connectors.${name}: names are lowercase letters, digits and dashes, and not gateway, console or secrets";
    }) names;

    services.mandate = lib.mkIf (cfg.useACMEHost != null) {
      tlsCertFile = lib.mkDefault "${acmeDir}/fullchain.pem";
      tlsKeyFile = lib.mkDefault "${acmeDir}/key.pem";
    };
    security.acme.certs = lib.mkIf (cfg.useACMEHost != null) {
      ${cfg.useACMEHost}.reloadServices = [ "mandate-console.service" ];
    };

    environment.systemPackages = [
      cfg.package
      resetKeys
    ];
    system.checks = lib.mapAttrsToList (
      name: c:
      pkgs.runCommand "mandate-${name}-config-check" { } ''
        ${lib.getExe c.package} --check-config ${configFile name c}
        touch $out
      ''
    ) cfg.connectors;

    users.users = lib.mkMerge [
      (lib.genAttrs accounts (user: {
        isSystemUser = true;
        group = user;
      }))
      (lib.mapAttrs' (n: _: lib.nameValuePair "mandate-${n}" { home = state n; }) cfg.connectors)
      # Connector groups grant read access to each data.db (0640) and socket (0660).
      {
        mandate-gateway.extraGroups = [ "mandate-console" ] ++ connectorGroups;
        mandate-console.extraGroups = connectorGroups;
      }
    ];
    users.groups = lib.genAttrs accounts (_: { });

    systemd.services =
      lib.mapAttrs' (n: c: lib.nameValuePair "mandate-${n}" (connectorService n c)) cfg.connectors
      // {
        mandate-gateway =
          service "gateway" (
            tailnetOnly
            // {
              LoadCredential = [ "token:${tokenFile}" ];
              ExecStart =
                utils.escapeSystemdExecArgs (
                  [
                    exe
                    "gateway"
                    "--listen"
                    ":${toString cfg.port}"
                    "--exposure"
                    "${state "gateway"}/exposure.toml"
                    "--console-socket"
                    proposalSocket
                    "--console-url"
                    origin
                  ]
                  # A connector is a source once it has written a data.db.
                  ++ lib.concatMap (n: [
                    "--source"
                    "${n}=${state n}/data.db"
                  ]) names
                  ++ connectorArgs
                )
                + credential "--token-file" "token";
            }
          )
          // {
            requires = secretsUnit;
            after = [ "network.target" ] ++ secretsUnit ++ connectorUnits;
          };

        mandate-console =
          service "console" (
            tailnetOnly
            // {
              StateDirectoryMode = "0700";
              AmbientCapabilities = [ "CAP_NET_BIND_SERVICE" ];
              CapabilityBoundingSet = [ "CAP_NET_BIND_SERVICE" ];
              LoadCredential = [
                "tls-cert:${cfg.tlsCertFile}"
                "tls-key:${cfg.tlsKeyFile}"
                "admin-password-hash:${hashFile}"
              ];
              ExecStart =
                utils.escapeSystemdExecArgs (
                  [
                    exe
                    "console"
                    "--hostname"
                    cfg.hostname
                    "--listen"
                    ":${toString cfg.consolePort}"
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
            }
          )
          // {
            requires = secretsUnit;
            wants = acmeUnit;
            after = [ "network.target" ] ++ secretsUnit ++ acmeUnit ++ connectorUnits;
          };

        mandate-secrets = lib.mkIf (secretsUnit != [ ]) {
          description = "mandate generated secrets";
          path = [
            pkgs.openssl
            pkgs.mkpasswd
          ];
          script = ''
            cd ${generated}
            ${lib.optionalString (cfg.tokenFile == null) ''
              if [ ! -e token ]; then
                openssl rand -hex 32 > token.tmp
                mv token.tmp token
              fi
            ''}
            ${lib.optionalString (cfg.adminPasswordHashFile == null) ''
              if [ ! -e admin-password-hash ]; then
                openssl rand -hex 16 > admin-password
                mkpasswd -m bcrypt -s < admin-password > admin-password-hash.tmp
                mv admin-password-hash.tmp admin-password-hash
              fi
            ''}
          '';
          serviceConfig = hardening // {
            Type = "oneshot";
            StateDirectory = "mandate";
            StateDirectoryMode = "0700";
            UMask = "0077";
          };
        };
      };

    networking.firewall.interfaces = lib.mkIf (cfg.openFirewallOn != null) {
      ${cfg.openFirewallOn}.allowedTCPPorts = [
        cfg.port
        cfg.consolePort
      ];
    };
  };
}
