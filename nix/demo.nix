# The demo server: mandate with the fake connector (nix/testconnector), its
# port 8443 forwarded to the host's loopback. `nix run .#demo` boots it, and
# the app's journey check (app/nix/journey.nix) drives it.
self:
{ pkgs, ... }:
{
  imports = [ self.nixosModules.default ];
  services.mandate = {
    enable = true;
    connectors.test = {
      package = import ./test-programs.nix self.packages.${pkgs.stdenv.hostPlatform.system}.mandate;
      config.account = "demo";
    };
  };
  networking.firewall.allowedTCPPorts = [ 8443 ];
  # An Android emulator reaches it at 10.0.2.2:8443.
  virtualisation.forwardPorts = [
    {
      from = "host";
      host.address = "127.0.0.1";
      host.port = 8443;
      guest.port = 8443;
    }
  ];
}
