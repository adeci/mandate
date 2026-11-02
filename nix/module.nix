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
  socketGroup = name: "mandate-${name}-socket";
  socketGroups = map socketGroup names;
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
  hashFile =
    if cfg.adminPasswordHashFile != null then
      cfg.adminPasswordHashFile
    else
      "${generated}/admin-password-hash";
  secretsUnit = lib.optional (cfg.adminPasswordHashFile == null) "mandate-secrets.service";
  ntfy = cfg.notify.ntfy;
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

  # Deletes every key so the admin password can sign in and add a first one
  # again. The console, finding no keys on start, drops pending key requests
  # and passkey sessions. Agents and grants signed by the old keys stop
  # verifying at the gateway.
  resetKeys = pkgs.writeShellApplication {
    name = "mandate-reset-keys";
    text = ''
      units=(mandate-gateway mandate-console ${toString connectorGroups})
      systemctl stop "''${units[@]}"
      rm -f ${state "console"}/keys.json ${state "gateway"}/keys.json ${
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

  # Connectors talk to the internet. Reaching the LAN, the tailnet or
  # localhost takes an IPAddressAllow in the connector's serviceConfig.
  privateNets = [
    "127.0.0.0/8"
    "::1/128"
    "10.0.0.0/8"
    "172.16.0.0/12"
    "192.168.0.0/16"
    "100.64.0.0/10"
    "169.254.0.0/16"
    "fc00::/7"
    "fe80::/10"
  ];
  # systemd-resolved's stub listeners.
  dnsStub = [
    "127.0.0.53"
    "127.0.0.54"
  ];

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
          "--console-user"
          "mandate-console"
          "--gateway-user"
          "mandate-gateway"
          "--socket-group"
          (socketGroup name)
        ];
        # The console reaches the socket through its group, so it must cross
        # the runtime directory without the data group.
        RuntimeDirectoryMode = "0751";
        # Handing the socket to its group takes chown, which ~@privileged drops.
        SystemCallFilter = hardening.SystemCallFilter ++ [ "@chown" ];
        RestartSec = "30s";
        # A connector buffers whole media files; a cap keeps a runaway one
        # from taking the host's memory with it.
        MemoryMax = "1G";
        IPAddressDeny = privateNets;
      }
      // c.serviceConfig
      // {
        IPAddressAllow = dnsStub ++ lib.toList (c.serviceConfig.IPAddressAllow or [ ]);
      }
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
          defaultText = lib.literalMD "the built-in connector of that name (`whatsapp`, `signal`, `matrix`, `discord`, `fastmail`, `gmessages`, `http`)";
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
          example = {
            IPAddressAllow = [ "192.168.1.20" ];
          };
          description = ''
            systemd service settings merged over the defaults. Connectors
            may not reach localhost, the LAN or the tailnet; list a host in
            `IPAddressAllow` to let this one reach it, e.g. a self-hosted
            homeserver or a LAN API. Each connector is capped at
            `MemoryMax = "1G"`; raise it here for a large account.
          '';
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

    gatewayUrl = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "http://mandate.example.ts.net:8780";
      description = ''
        URL agents reach the gateway at, shown as `MANDATE_URL` when you
        create an agent. If null, the console leaves that line out.
      '';
    };

    notify.ntfy = {
      url = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        example = "http://ntfy.example.ts.net/mandate";
        description = ''
          ntfy topic URL the console posts to when a write or an access
          request waits for you. The post names the agent and connector,
          never message text or chats. The console reaches only localhost and
          the tailnet.
        '';
      };

      tokenFile = lib.mkOption {
        type = lib.types.nullOr lib.types.externalPath;
        default = null;
        example = "/run/secrets/mandate-ntfy-token";
        description = "File with an ntfy access token that may publish to the topic.";
      };
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
        && builtins.match ".*-socket" name == null
        && !lib.elem name [
          "gateway"
          "console"
          "secrets"
        ];
      message = "services.mandate.connectors.${name}: names are lowercase letters, digits and dashes, don't end in -socket, and are not gateway, console or secrets";
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
      (lib.mapAttrs' (
        n: _:
        lib.nameValuePair "mandate-${n}" {
          home = state n;
          extraGroups = [ (socketGroup n) ];
        }
      ) cfg.connectors)
      # A connector's group reads its data.db (0640); its socket group connects
      # to its socket (0660). The console gets the sockets only.
      {
        mandate-gateway.extraGroups = [ "mandate-console" ] ++ connectorGroups ++ socketGroups;
        mandate-console.extraGroups = socketGroups;
      }
    ];
    users.groups = lib.genAttrs (accounts ++ socketGroups) (_: { });

    systemd.services =
      lib.mapAttrs' (n: c: lib.nameValuePair "mandate-${n}" (connectorService n c)) cfg.connectors
      // {
        mandate-gateway =
          service "gateway" (
            tailnetOnly
            // {
              ExecStart = utils.escapeSystemdExecArgs (
                [
                  exe
                  "gateway"
                  "--listen"
                  ":${toString cfg.port}"
                  "--state"
                  (state "gateway")
                  "--exposure"
                  "${state "gateway"}/exposure.toml"
                  "--console-socket"
                  proposalSocket
                  "--console-url"
                  origin
                  "--rp-id"
                  cfg.hostname
                  "--origin"
                  origin
                ]
                # A connector is a source once it has written a data.db.
                ++ lib.concatMap (n: [
                  "--source"
                  "${n}=${state n}/data.db"
                ]) names
                ++ connectorArgs
              );
            }
          )
          // {
            # Agents and grants come from the console.
            wants = [ "mandate-console.service" ];
            after = [
              "network.target"
              "mandate-console.service"
            ]
            ++ connectorUnits;
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
              ]
              ++ lib.optional (ntfy.tokenFile != null) "ntfy-token:${ntfy.tokenFile}";
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
                  ++ lib.optionals (ntfy.url != null) [
                    "--ntfy-url"
                    ntfy.url
                  ]
                  ++ lib.optionals (cfg.gatewayUrl != null) [
                    "--gateway-url"
                    cfg.gatewayUrl
                  ]
                  ++ connectorArgs
                )
                + credential "--tls-cert" "tls-cert"
                + credential "--tls-key" "tls-key"
                + credential "--admin-password-hash-file" "admin-password-hash"
                + lib.optionalString (ntfy.tokenFile != null) (credential "--ntfy-token-file" "ntfy-token");
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
            if [ ! -e admin-password-hash ]; then
              openssl rand -hex 16 > admin-password
              mkpasswd -m bcrypt -s < admin-password > admin-password-hash.tmp
              mv admin-password-hash.tmp admin-password-hash
            fi
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
