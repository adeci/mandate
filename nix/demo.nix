# The demo server: mandate with the fake connector (nix/testconnector), its
# port 8443 forwarded to the host's loopback. `nix run .#demo` boots it, and
# the app's journeys (app/nix/) drive it. Besides `test`, the fake runs under
# three brands' names with their shapes of chats, the worst cases real
# accounts showed: every UI round is judged on these.
self:
{ lib, pkgs, ... }:
let
  fake = import ./test-programs.nix self.packages.${pkgs.stdenv.hostPlatform.system}.mandate;
in
{
  imports = [ self.nixosModules.default ];
  services.mandate = {
    enable = true;
    connectors = {
      test = {
        package = fake;
        config.account = "demo";
      };
    }
    // lib.genAttrs [ "discord" "fastmail" "signal" ] (shape: {
      package = fake;
      config = {
        account = "demo";
        inherit shape;
      };
    });
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
