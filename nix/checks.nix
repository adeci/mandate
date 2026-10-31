{
  pkgs,
  self,
  treefmt,
}:
let
  inherit (pkgs) lib;
  mandate = self.packages.${pkgs.stdenv.hostPlatform.system}.mandate;
in
{
  package = mandate;

  go-tests = mandate.overrideAttrs (old: {
    pname = "mandate-go-tests";
    doCheck = true;
    # -race needs cgo.
    env = old.env // {
      CGO_ENABLED = 1;
    };
    checkPhase = ''
      runHook preCheck
      go test -race -p $NIX_BUILD_CORES ./...
      runHook postCheck
    '';
    installPhase = "touch $out";
    doInstallCheck = false;
  });

  golangci-lint = mandate.overrideAttrs (old: {
    pname = "mandate-golangci-lint";
    src = lib.fileset.toSource {
      root = ../.;
      # Without the config golangci-lint silently falls back to its defaults.
      fileset = lib.fileset.unions [
        ../go.mod
        ../go.sum
        ../cmd
        ../internal
        ../.golangci.yml
      ];
    };
    nativeBuildInputs = old.nativeBuildInputs ++ [ pkgs.golangci-lint ];
    buildPhase = ''
      runHook preBuild
      HOME=$TMPDIR golangci-lint run --concurrency $NIX_BUILD_CORES ./...
      runHook postBuild
    '';
    installPhase = "touch $out";
    doInstallCheck = false;
  });

  treefmt = pkgs.runCommand "mandate-treefmt" { } ''
    cp -r ${self} src
    chmod -R +w src
    cd src
    ${lib.getExe treefmt} --ci --no-cache
    touch $out
  '';

  nixos-test = import ./vm-test.nix { inherit pkgs self; };
}
