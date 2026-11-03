# The programs the tests run: nix/testdevice and nix/testconnector, built from
# the mandate package's sources.
mandate:
mandate.overrideAttrs {
  name = "mandate-test-programs";
  subPackages = [
    "nix/testdevice"
    "nix/testconnector"
  ];
  excludedPackages = [ ];
  doCheck = false;
  meta.mainProgram = "testconnector";
}
