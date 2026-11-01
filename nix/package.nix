{
  lib,
  buildGoModule,
  go_1_27,
  libsignal-ffi,
  zlib,
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
      # C libraries linked with cgo; without them the build is pure Go.
      buildInputs ? [ ],
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

      inherit buildInputs;
      env.CGO_ENABLED = if buildInputs == [ ] then 0 else 1;
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

  connectorWith =
    {
      buildInputs ? [ ],
    }:
    name: description:
    module {
      pname = "mandate-${name}";
      modRoot = "connectors/${name}";
      files = [ (../connectors + "/${name}") ];
      # Go names the binary after its directory.
      postInstall = "mv $out/bin/${name} $out/bin/mandate-${name}";
      inherit buildInputs description;
    };
  connector = connectorWith { };
in
{
  mandate = module {
    pname = "mandate";
    description = "Gateway, console and agent CLI for mandate";
  };
  mandate-whatsapp = connector "whatsapp" "WhatsApp connector for mandate";
  mandate-discord = connector "discord" "Discord connector for mandate";
  mandate-http = connector "http" "HTTP API connector for mandate";
  # libsignal-ffi must be the version mautrix-signal's libsignalgo is built for.
  mandate-signal = connectorWith {
    buildInputs = [
      libsignal-ffi
      zlib
    ];
  } "signal" "Signal connector for mandate";
}
