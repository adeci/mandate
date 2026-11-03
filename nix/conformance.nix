# The connector conformance kit, run on every bundled connector and on the
# tests' connector.
{ self, pkgs }:
let
  inherit (pkgs) lib;
  packages = self.packages.${pkgs.stdenv.hostPlatform.system};
  connectors = lib.filterAttrs (n: _: lib.hasPrefix "mandate-" n) packages;
  testPrograms = import ./test-programs.nix packages.mandate;
  testConfig = pkgs.writeText "testconnector.json" (builtins.toJSON { account = "conformance"; });
in
pkgs.runCommand "mandate-conformance" { } ''
  status=0
  for program in ${lib.escapeShellArgs (map lib.getExe (lib.attrValues connectors))}; do
    ${lib.getExe packages.mandate} conformance "$program" || status=1
  done
  ${lib.getExe packages.mandate} conformance ${lib.getExe testPrograms} --config ${testConfig} || status=1
  if [ "$status" != 0 ]; then
    exit 1
  fi
  touch $out
''
