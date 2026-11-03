self:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.mandate;
  inherit (pkgs.stdenv.hostPlatform) system;
  json = pkgs.formats.json { };
  toml = pkgs.formats.toml { };

  configFile = toml.generate "mandate.toml" (
    lib.filterAttrs (_: v: v != null) {
      inherit (cfg) listen;
      agent_listen = cfg.agentListen;
      tls = lib.filterAttrs (_: v: v != null) cfg.tls;
      connectors = lib.mapAttrs (_: c: {
        command = lib.getExe c.package;
        inherit (c) config;
      }) cfg.connectors;
    }
  );
  connectorConfig = name: c: json.generate "mandate-${name}.json" c.config;

  # The units, sandbox and all, come from mandate itself, as on any other
  # host (internal/owner/systemd.tmpl).
  units = pkgs.runCommand "mandate-units" { } ''
    ${lib.getExe cfg.package} units --config ${configFile} --out $out/lib/systemd/system
  '';
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

    agentListen = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = ":8444";
      description = ''
        Address of an extra HTTPS port that serves only the agent API, so a
        network rule can keep agent machines away from the owner side.
      '';
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

    environment.etc = {
      "mandate/mandate.toml".source = configFile;
    }
    // lib.mapAttrs' (
      n: c: lib.nameValuePair "mandate/connectors/${n}.json" { source = connectorConfig n c; }
    ) cfg.connectors;
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

    # The units' own [Install] sections are ignored on NixOS. Anything set
    # here becomes a drop-in, such as the IPAddressAllow= of a connector for a
    # LAN or self-hosted service in systemd.services.mandate-NAME.serviceConfig.
    systemd.packages = [ units ];
    systemd.targets.mandate.wantedBy = [ "multi-user.target" ];
    systemd.services =
      lib.genAttrs
        (map (n: "mandate-${n}") (
          [
            "front"
            "gateway"
            "owner"
          ]
          ++ lib.attrNames cfg.connectors
        ))
        (_: {
          wantedBy = [ "mandate.target" ];
        });
  };
}
