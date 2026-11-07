{
  lib,
  buildGoModule,
  buildPackages,
  libsignal-ffi,
  zlib,
}:
let
  # Connector modules replace github.com/adeci/mandate with ../.., so every
  # build needs the packages they import from it. go.work is left out:
  # vendoring refuses workspace mode.
  core = [
    ../go.mod
    ../go.sum
    ../connector
    ../record
    ../store
    ../internal/peer
  ];
  hashes = lib.importJSON ./vendor-hashes.json;

  module =
    {
      pname,
      modRoot ? ".",
      files ? [ ],
      excludedPackages ? [ ],
      postInstall ? "",
      # C libraries linked with cgo; without them the build is pure Go.
      buildInputs ? [ ],
      description,
    }:
    # The Go that runs on the build machine, which differs when cross
    # compiling or building static (nix/release.nix).
    (buildGoModule.override { go = buildPackages.go_1_27; }) {
      inherit
        pname
        modRoot
        excludedPackages
        postInstall
        buildInputs
        ;
      version = "0.1.0";

      src = lib.fileset.toSource {
        root = ../.;
        fileset = lib.fileset.unions (core ++ files);
      };

      # Updated by nix/update-vendor-hash.sh.
      vendorHash = hashes.${pname};

      env.CGO_ENABLED = if buildInputs == [ ] then 0 else 1;
      ldflags = [
        "-s"
        "-w"
      ];

      # -race needs cgo.
      preCheck = ''
        export CGO_ENABLED=1
      '';
      checkFlags = [ "-race" ];

      meta = {
        inherit description;
        mainProgram = pname;
        license = lib.licenses.agpl3Only;
        platforms = lib.platforms.linux;
      };
    };

  # The vendored modules are a fixed-output derivation: Nix reuses it while
  # vendorHash stays the same. Our own module (the replace to ../..) would
  # go stale in it, so it is vendored from the source at build time instead,
  # and vendorHash covers third-party modules only.
  ownModule = "vendor/github.com/adeci/mandate";
  connectorWith =
    {
      buildInputs ? [ ],
    }:
    name: description:
    (module {
      pname = "mandate-${name}";
      modRoot = "connectors/${name}";
      files = [ (../connectors + "/${name}") ];
      # Go names the binary after its directory.
      postInstall = "mv $out/bin/${name} $out/bin/mandate-${name}";
      inherit buildInputs description;
    }).overrideAttrs
      {
        modPostBuild = "rm -r ${ownModule}";
        # As go mod vendor does: each package's files, without tests. The
        # vendoring derivation runs preBuild too, before vendor/ exists.
        preBuild = ''
          if [ -f vendor/modules.txt ]; then
            chmod -R u+w vendor
            sed -n 's|^github.com/adeci/mandate/||p' vendor/modules.txt | while read -r pkg; do
              mkdir -p "${ownModule}/$pkg"
              find "../../$pkg" -maxdepth 1 -type f ! -name '*_test.go' -exec cp -t "${ownModule}/$pkg" {} +
            done
          fi
        '';
      };
  connector = connectorWith { };
in
{
  # The VM test's programs are linted and tested with the rest of the root
  # module; vm-test.nix builds them.
  mandate = module {
    pname = "mandate";
    files = [
      ../cmd
      ../internal
      # Test vectors, and the chats each connector stores.
      ../docs/spec
      ../connectors/discord/testdata/chats.json
      ../connectors/fastmail/testdata/chats.json
      ../connectors/gmessages/testdata/chats.json
      ../connectors/matrix/testdata/chats.json
      ../connectors/signal/testdata/chats.json
      ../connectors/whatsapp/testdata/chats.json
      ./testdevice
      ./testconnector
    ];
    excludedPackages = [
      "testdevice"
      "testconnector"
    ];
    description = "mandate's server processes, setup and agent CLI";
  };

  mandate-whatsapp = connector "whatsapp" "WhatsApp connector for mandate";
  mandate-discord = connector "discord" "Discord connector for mandate";
  mandate-fastmail = connector "fastmail" "Fastmail connector for mandate";
  mandate-gmessages = connector "gmessages" "Google Messages connector for mandate";
  # libsignalgo is written against one libsignal-ffi version; the build stops
  # if nixpkgs has another.
  mandate-signal =
    (connectorWith {
      buildInputs = [
        libsignal-ffi
        zlib
      ];
    } "signal" "Signal connector for mandate").overrideAttrs
      {
        postConfigure = ''
          want=$(sed -n 's/^const Version = "v\(.*\)"$/\1/p' vendor/go.mau.fi/mautrix-signal/pkg/libsignalgo/signalversion/version.go)
          if [ "$want" != "${libsignal-ffi.version}" ]; then
            echo "libsignalgo needs libsignal-ffi '$want', nixpkgs has ${libsignal-ffi.version}" >&2
            exit 1
          fi
        '';
      };

  # mautrix's pure-Go olm is behind the goolm tag. GOFLAGS rather than tags,
  # so the tests and golangci-lint build it too.
  mandate-matrix = (connector "matrix" "Matrix connector for mandate").overrideAttrs (old: {
    env = old.env // {
      GOFLAGS = "${old.env.GOFLAGS} -tags=goolm";
    };
  });
}
