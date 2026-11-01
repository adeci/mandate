{
  lib,
  buildGoModule,
  go_1_27,
}:
let
  # Connector modules replace github.com/adeci/mandate with ../.., so every
  # build needs the core sources. go.work is left out: vendoring refuses
  # workspace mode.
  core = [
    ../go.mod
    ../go.sum
    ../cmd
    ../internal
    ../connector
    ../approval
    ../store
  ];
  hashes = lib.importJSON ./vendor-hashes.json;

  module =
    {
      pname,
      modRoot ? ".",
      files ? [ ],
      postInstall ? "",
      description,
    }:
    # Discord's tls-client needs Go 1.27; everything builds with one toolchain.
    (buildGoModule.override { go = go_1_27; }) {
      inherit pname modRoot postInstall;
      version = "0.1.0";

      src = lib.fileset.toSource {
        root = ../.;
        fileset = lib.fileset.unions (core ++ files);
      };

      # Updated by nix/update-vendor-hash.sh.
      vendorHash = hashes.${pname};

      env.CGO_ENABLED = 0;
      ldflags = [
        "-s"
        "-w"
      ];

      # The Go tests run in checks (with -race) so they cache separately.
      doCheck = false;

      meta = {
        inherit description;
        mainProgram = pname;
        platforms = lib.platforms.linux;
      };
    };

  connector =
    name: description:
    module {
      pname = "mandate-${name}";
      modRoot = "connectors/${name}";
      files = [ (../connectors + "/${name}") ];
      # Go names the binary after its directory.
      postInstall = "mv $out/bin/${name} $out/bin/mandate-${name}";
      inherit description;
    };
in
{
  mandate = module {
    pname = "mandate";
    description = "Gateway, console and agent CLI for mandate";
  };
  mandate-whatsapp = connector "whatsapp" "WhatsApp connector for mandate";
  mandate-discord = connector "discord" "Discord connector for mandate";
  mandate-http = connector "http" "HTTP API connector for mandate";
  # mautrix's pure-Go olm is behind the goolm tag. GOFLAGS rather than tags,
  # so the checks' go test and golangci-lint build it too.
  mandate-matrix = (connector "matrix" "Matrix connector for mandate").overrideAttrs (old: {
    env = old.env // {
      GOFLAGS = "${old.env.GOFLAGS} -tags=goolm";
    };
  });
}
