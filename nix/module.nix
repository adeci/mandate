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
  inherit (pkgs.stdenv.hostPlatform) system;
  exe = lib.getExe cfg.package;
  json = pkgs.formats.json { };
  toml = pkgs.formats.toml { };

  configFile = toml.generate "mandate.toml" {
    inherit (cfg) listen;
    tls = lib.filterAttrs (_: v: v != null) cfg.tls;
    connectors = lib.mapAttrs (_: c: {
      command = lib.getExe c.package;
      inherit (c) config;
    }) cfg.connectors;
  };
  connectorConfig = name: c: json.generate "mandate-${name}.json" c.config;
  connectorUnits = map (n: "mandate-${n}.service") (lib.attrNames cfg.connectors);

  # DynamicUser= implies ProtectSystem=strict, ProtectHome=read-only,
  # PrivateTmp, RemoveIPC, NoNewPrivileges and RestrictSUIDSGID.
  hardening = {
    DynamicUser = true;
    ProtectHome = true;
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
    LockPersonality = true;
    MemoryDenyWriteExecute = true;
    SystemCallArchitectures = "native";
    SystemCallFilter = [
      "@system-service"
      "~@privileged"
    ];
    CapabilityBoundingSet = "";
    UMask = "0027";
    Restart = "always";
    RestartSec = "2s";
  };

  unit = description: serviceConfig: {
    inherit description;
    partOf = [ "mandate.target" ];
    wantedBy = [ "mandate.target" ];
    restartTriggers = [ configFile ];
    serviceConfig = hardening // serviceConfig;
  };

  # gateway and owner talk over Unix sockets only.
  unixOnly = {
    PrivateNetwork = true;
    RestrictAddressFamilies = [ "AF_UNIX" ];
  };

  # Connectors talk to their service on the internet, not to the LAN or
  # localhost. A connector for a LAN or self-hosted service takes an
  # IPAddressAllow in systemd.services.mandate-NAME.serviceConfig.
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
    let
      # Where DynamicUser= puts StateDirectory=mandate/connectors/NAME.
      state = "/var/lib/private/mandate/connectors/${name}";
    in
    unit "mandate connector ${name}" {
      User = "mandate-${name}";
      StateDirectory = "mandate/connectors/${name}";
      StateDirectoryMode = "0750";
      RuntimeDirectory = "mandate/connectors/${name}";
      # The gateway reads data.db: its group gets the directory and, by the
      # default ACL, what the connector creates group-readable in it.
      # private/ is 0700, so its mask keeps the gateway out. The unit's own
      # view of the directory is idmapped, where the gateway's group has no
      # ID, so the ACL goes on through PID 1's root.
      ExecStartPre = "+${pkgs.acl}/bin/setfacl -m g:mandate-gateway:r-X,d:g:mandate-gateway:r-X /proc/1/root${state}";
      ExecStart = utils.escapeSystemdExecArgs [
        (lib.getExe c.package)
        "--name"
        name
        "--socket"
        "/run/mandate/connectors/${name}/socket"
        "--state"
        "/var/lib/mandate/connectors/${name}"
        "--config"
        (connectorConfig name c)
        "--gateway-group"
        "mandate-gateway"
        "--owner-group"
        "mandate-owner"
      ];
      RestrictAddressFamilies = [
        "AF_UNIX"
        "AF_INET"
        "AF_INET6"
      ];
      IPAddressDeny = privateNets;
      IPAddressAllow = dnsStub;
      # A connector may buffer whole media files; a cap keeps a runaway one
      # from taking the host's memory with it.
      MemoryMax = "1G";
    }
    // {
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
    };
in
{
  options.services.mandate = {
    enable = lib.mkEnableOption "mandate";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${system}.mandate;
      defaultText = lib.literalExpression "mandate.packages.\${system}.mandate";
      description = "The mandate package.";
    };

    listen = lib.mkOption {
      type = lib.types.str;
      default = ":8443";
      description = "Address of mandate's one HTTPS port, for agents and the app.";
    };

    tls = {
      mode = lib.mkOption {
        type = lib.types.enum [
          "own"
          "acme"
          "cert"
        ];
        default = "own";
        description = ''
          `own`: a key and self-signed certificate mandate makes itself,
          which clients pin. `acme`: a certificate for
          {option}`services.mandate.tls.domain` by TLS-ALPN-01, so the CA
          must reach port 443. `cert`: the files
          {option}`services.mandate.tls.cert` and
          {option}`services.mandate.tls.key`.
        '';
      };
      domain = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        example = "mandate.example.com";
        description = "Domain for `acme`.";
      };
      cert = lib.mkOption {
        type = lib.types.nullOr lib.types.externalPath;
        default = null;
        description = "Certificate chain for `cert`, readable by the group mandate-front. Reloaded when it changes.";
      };
      key = lib.mkOption {
        type = lib.types.nullOr lib.types.externalPath;
        default = null;
        description = "Key for `cert`, readable by the group mandate-front.";
      };
    };

    connectors = lib.mkOption {
      type = lib.types.attrsOf (
        lib.types.submodule (
          { name, ... }:
          {
            options = {
              package = lib.mkOption {
                type = lib.types.package;
                default =
                  self.packages.${system}."mandate-${name}"
                    or (throw "services.mandate.connectors.${name}.package must be set: ${name} isn't a built-in connector");
                defaultText = lib.literalMD "the built-in connector of that name";
                description = "The connector program.";
              };
              config = lib.mkOption {
                inherit (json) type;
                default = { };
                description = "The connector's config, passed to it as JSON and checked at build time.";
              };
            };
          }
        )
      );
      default = { };
      example = lib.literalExpression ''
        {
          whatsapp = { };
          signal = { };
        }
      '';
      description = "Connectors by name. Each runs as its own dynamic user, and the owner links it in the app.";
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion =
          {
            own = cfg.tls.domain == null && cfg.tls.cert == null && cfg.tls.key == null;
            acme = cfg.tls.domain != null && cfg.tls.cert == null && cfg.tls.key == null;
            cert = cfg.tls.domain == null && cfg.tls.cert != null && cfg.tls.key != null;
          }
          .${cfg.tls.mode};
        message = "services.mandate.tls: mode own takes nothing else, acme a domain, cert a cert and a key";
      }
    ]
    ++ map (name: {
      assertion =
        builtins.match "[a-z][a-z0-9-]{0,23}" name != null
        && !lib.elem name [
          "front"
          "gateway"
          "owner"
        ];
      message = "services.mandate.connectors.${name}: names are a-z, 0-9 and -, start with a letter, have at most 24 characters, and are not front, gateway or owner";
    }) (lib.attrNames cfg.connectors);

    environment.etc."mandate/mandate.toml".source = configFile;
    # mandate setup and mandate reset, as root.
    environment.systemPackages = [ cfg.package ];

    system.checks = lib.mapAttrsToList (
      name: c:
      pkgs.runCommand "mandate-${name}-config-check" { } ''
        ${lib.getExe c.package} --check-config ${connectorConfig name c}
        touch $out
      ''
    ) cfg.connectors;

    # Fixed groups, so that the others can tell each process by its group.
    users.groups = lib.genAttrs [
      "mandate-front"
      "mandate-gateway"
      "mandate-owner"
    ] (_: { });

    systemd.targets.mandate = {
      description = "mandate";
      wantedBy = [ "multi-user.target" ];
    };

    systemd.services =
      lib.mapAttrs' (n: c: lib.nameValuePair "mandate-${n}" (connectorService n c)) cfg.connectors
      // {
        mandate-front =
          unit "mandate front" {
            # A dynamic user may not share its name with the static group.
            User = "mandate_front";
            Group = "mandate-front";
            ExecStart = "${exe} serve front";
            StateDirectory = "mandate/front";
            StateDirectoryMode = "0700";
            AmbientCapabilities = [ "CAP_NET_BIND_SERVICE" ];
            CapabilityBoundingSet = [ "CAP_NET_BIND_SERVICE" ];
            RestrictAddressFamilies = [
              "AF_UNIX"
              "AF_INET"
              "AF_INET6"
            ];
          }
          // {
            after = [ "network-online.target" ];
            wants = [ "network-online.target" ];
          };

        mandate-gateway =
          unit "mandate gateway" (
            unixOnly
            // {
              User = "mandate_gateway";
              Group = "mandate-gateway";
              ExecStart = "${exe} serve gateway";
              StateDirectory = "mandate/gateway";
              StateDirectoryMode = "0700";
              RuntimeDirectory = "mandate/gateway";
              # Connector state lives under /var/lib/private, which only root
              # may enter; the ACLs then let the gateway read each data.db.
              BindReadOnlyPaths = lib.optional (
                cfg.connectors != { }
              ) "/var/lib/private/mandate/connectors:/var/lib/mandate/connectors";
            }
          )
          // {
            after = connectorUnits;
          };

        mandate-owner =
          unit "mandate owner" (
            unixOnly
            // {
              User = "mandate_owner";
              Group = "mandate-owner";
              ExecStart = "${exe} serve owner";
              StateDirectory = "mandate/owner";
              StateDirectoryMode = "0700";
              RuntimeDirectory = "mandate/owner";
            }
          )
          // {
            after = [ "mandate-gateway.service" ] ++ connectorUnits;
          };
      };
  };
}
