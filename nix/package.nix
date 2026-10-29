{
  lib,
  buildGoModule,
  go_1_27,
}:

# tls-client (Discord's Chrome fingerprint) requires Go 1.27.
(buildGoModule.override { go = go_1_27; }) {
  pname = "sieve";
  version = "0.1.0";

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../cmd
      ../internal
    ];
  };

  # Updated by nix/update-vendor-hash.sh.
  vendorHash = lib.fileContents ./goVendorHash.txt;

  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
  ];

  # The Go tests run in checks.go-tests (with -race) so they cache separately.
  doCheck = false;

  meta = {
    description = "Messaging ingestion and read-only SQL gateway for agents";
    mainProgram = "sieve";
    platforms = lib.platforms.linux;
  };
}
