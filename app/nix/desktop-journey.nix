# checks.x86_64-linux.desktop: the Linux app against the demo server
# (nix/demo.nix), in a VM with a CanoKey (a FIDO2 authenticator QEMU
# emulates) and SoftHSM2 for PKCS#11. Two runs of the app, each on its own
# Xvfb display and session bus, are driven with xdotool and OCR.
{
  mandate,
  desktop,
}:
mandate.inputs.nixpkgs.legacyPackages.x86_64-linux.testers.runNixOSTest {
  name = "mandate-desktop-journey";
  # The demo server, plus the fake connector under the brands' names the demo
  # doesn't seed, so that the screenshots show every connector's mark.
  nodes.server =
    { lib, pkgs, ... }:
    {
      imports = [ (import "${mandate}/nix/demo.nix" mandate) ];
      virtualisation.memorySize = 2048;
      services.mandate.connectors = lib.genAttrs [ "whatsapp" "matrix" "gmessages" ] (_: {
        package =
          import "${mandate}/nix/test-programs.nix"
            mandate.packages.${pkgs.stdenv.hostPlatform.system}.mandate;
        config.account = "demo";
      });
    };
  nodes.desktop =
    { lib, pkgs, ... }:
    {
      virtualisation = {
        memorySize = 4096;
        cores = 4;
        qemu.package = lib.mkForce (pkgs.qemu_test.override { canokeySupport = true; });
        # The key's state lives beside the VM's, so each run starts blank.
        qemu.options = [
          "-usb"
          "-device canokey,file=canokey"
        ];
      };
      # Software Vulkan for gpui.
      hardware.graphics.enable = true;
      environment.etc."softhsm2.conf".text = "directories.tokendir = /var/lib/softhsm\n";
      environment.etc."dunstrc".text = ''
        [global]
        origin = bottom-left
        offset = (10, 10)
        mouse_left_click = do_action, close_current

        [urgency_normal]
        timeout = 0
      '';
      environment.systemPackages = [
        desktop
        pkgs.xvfb
        pkgs.xdotool
        pkgs.imagemagick
        (pkgs.tesseract.override { enableLanguages = [ "eng" ]; })
        pkgs.dunst
        pkgs.dbus
        pkgs.gnome-keyring
        pkgs.softhsm
        pkgs.xdg-utils
        pkgs.curl
        (pkgs.python3.withPackages (p: [ p.fido2 ]))
        # A stand-in for xdg-desktop-portal's Settings, to switch the app
        # between light and dark: the color scheme comes from the file
        # /tmp/scheme<display>.
        (pkgs.writers.writePython3Bin "fake-portal"
          {
            libraries = [ pkgs.python3Packages.dbus-next ];
            flakeIgnore = [ "E501" ];
          }
          ''
            import asyncio
            import os
            import sys

            from dbus_next import PropertyAccess, Variant
            from dbus_next.aio import MessageBus
            from dbus_next.errors import DBusError
            from dbus_next.service import ServiceInterface, dbus_property, method, signal

            path = sys.argv[1]


            def scheme():
                try:
                    return int(open(path).read())
                except (OSError, ValueError):
                    return 0


            class Settings(ServiceInterface):
                def __init__(self):
                    super().__init__("org.freedesktop.portal.Settings")

                @method()
                def Read(self, namespace: "s", key: "s") -> "v":  # noqa: F821
                    if (namespace, key) != ("org.freedesktop.appearance", "color-scheme"):
                        raise DBusError("org.freedesktop.portal.Error.NotFound", key)
                    return Variant("v", Variant("u", scheme()))

                @method()
                def ReadAll(self, namespaces: "as") -> "a{sa{sv}}":  # noqa: F821, F722
                    return {"org.freedesktop.appearance": {"color-scheme": Variant("u", scheme())}}

                @signal()
                def SettingChanged(self, namespace, key, value) -> "ssv":  # noqa: F821
                    return [namespace, key, value]

                @dbus_property(access=PropertyAccess.READ)
                def version(self) -> "u":  # noqa: F821
                    return 1


            async def main():
                bus = await MessageBus().connect()
                settings = Settings()
                bus.export("/org/freedesktop/portal/desktop", settings)
                await bus.request_name("org.freedesktop.portal.Desktop")
                last = scheme()
                while True:
                    await asyncio.sleep(0.2)
                    if os.path.exists(path) and scheme() != last:
                        last = scheme()
                        settings.SettingChanged(
                            "org.freedesktop.appearance", "color-scheme", Variant("u", last)
                        )


            asyncio.run(main())
          ''
        )
      ];
    };
  testScript = ''
    module = "${mandate.inputs.nixpkgs.legacyPackages.x86_64-linux.softhsm}/lib/softhsm/libsofthsm2.so"
  ''
  + builtins.readFile ./desktop-journey.py;
}
