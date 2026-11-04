# `nix run .#demo`: boots nix/demo.nix as a throwaway VM (its root is a
# tmpfs) and prints the setup password. The VM runs until Ctrl-C.
{
  self,
  nixpkgs,
  pkgs,
}:
let
  vm =
    (nixpkgs.lib.nixosSystem {
      inherit (pkgs.stdenv.hostPlatform) system;
      modules = [
        "${nixpkgs}/nixos/modules/virtualisation/qemu-vm.nix"
        (import ./demo.nix self)
        (
          { config, lib, ... }:
          {
            networking.hostName = "mandate-demo";
            virtualisation = {
              graphics = false;
              diskImage = null;
            };
            # The serial console is the launcher's log; this puts the password in it.
            systemd.services.mandate-demo = {
              wantedBy = [ "multi-user.target" ];
              after = [ "mandate.target" ];
              serviceConfig = {
                Type = "oneshot";
                StandardOutput = "tty";
                TTYPath = "/dev/ttyS0";
              };
              script = "${lib.getExe config.services.mandate.package} setup";
            };
            system.stateVersion = "26.05";
          }
        )
      ];
    }).config.system.build.vm;
in
pkgs.writeShellApplication {
  name = "mandate-demo";
  runtimeInputs = [
    pkgs.coreutils
    pkgs.gnugrep
  ];
  text = ''
    log=$(mktemp -t mandate-demo.XXXXXX)
    ${vm}/bin/run-mandate-demo-vm >"$log" 2>&1 </dev/null &
    vm=$!
    trap 'kill $vm 2>/dev/null || true; wait $vm || true; rm -f "$log"' EXIT
    echo "Booting the mandate demo VM; its console is in $log."
    until grep -q '^Key ' "$log"; do
      kill -0 $vm 2>/dev/null || { tail -n 20 "$log" >&2; exit 1; }
      sleep 1
    done
    # The VM's own name for itself means nothing outside it.
    grep -E '^(Initial setup|Key) ' "$log" | tr -d '\r' | sed -E 's#https://[^/]+/#https://localhost:8443/#'
    echo "In an Android emulator, use 10.0.2.2 instead of localhost."
    echo "Ctrl-C stops the VM."
    wait $vm
  '';
}
