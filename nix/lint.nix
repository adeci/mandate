{ golangci-lint }:
# golangci-lint over a package's Go module, with the package's vendored
# dependencies, build tags and C libraries.
pkg:
pkg.overrideAttrs (old: {
  # name, not pname, so the vendored modules are the package's own.
  name = "${old.pname}-lint";
  nativeBuildInputs = old.nativeBuildInputs ++ [ golangci-lint ];
  buildPhase = ''
    runHook preBuild
    HOME=$TMPDIR golangci-lint run --config ${../.golangci.yml} --concurrency $NIX_BUILD_CORES ./...
    runHook postBuild
  '';
  doCheck = false;
  installPhase = "touch $out";
  fixupPhase = ":";
})
