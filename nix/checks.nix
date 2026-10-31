{
  pkgs,
  self,
  treefmt,
}:
let
  inherit (pkgs) lib;
  packages = lib.removeAttrs self.packages.${pkgs.stdenv.hostPlatform.system} [ "default" ];

  goTests =
    pkg:
    pkg.overrideAttrs (old: {
      pname = "${old.pname}-go-tests";
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

  golangciLint =
    pkg:
    pkg.overrideAttrs (old: {
      pname = "${old.pname}-golangci-lint";
      # Without the config golangci-lint silently falls back to its defaults.
      src = lib.fileset.toSource {
        root = ../.;
        fileset = lib.fileset.unions [
          (lib.fileset.fromSource old.src)
          ../.golangci.yml
        ];
      };
      nativeBuildInputs = old.nativeBuildInputs ++ [ pkgs.golangci-lint ];
      buildPhase = ''
        runHook preBuild
        HOME=$TMPDIR golangci-lint run --config "$NIX_BUILD_TOP/source/.golangci.yml" --concurrency $NIX_BUILD_CORES ./...
        runHook postBuild
      '';
      installPhase = "touch $out";
      doInstallCheck = false;
    });

  perPackage = f: suffix: lib.mapAttrs' (name: pkg: lib.nameValuePair "${name}-${suffix}" (f pkg));
in
packages
// perPackage goTests "tests" packages
// perPackage golangciLint "lint" packages
// {
  treefmt = pkgs.runCommand "mandate-treefmt" { } ''
    cp -r ${self} src
    chmod -R +w src
    cd src
    ${lib.getExe treefmt} --ci --no-cache
    touch $out
  '';

  nixos-test = import ./vm-test.nix { inherit pkgs self; };
}
