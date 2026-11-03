# checks.x86_64-linux.journey: the APK in a headless emulator against the
# demo server (nix/demo.nix), driven with adb and the CLI. The emulator runs
# beside the server VM, in the build sandbox, so it needs KVM like the VM.
{
  pkgs,
  mandate,
  android,
  apk,
}:
mandate.inputs.nixpkgs.legacyPackages.x86_64-linux.testers.runNixOSTest {
  name = "mandate-app-journey";
  nodes.server = import "${mandate}/nix/demo.nix" mandate;
  testScript = ''
    sdk = "${android.androidsdk}/libexec/android-sdk"
    apk = "${apk}"
    tesseract = "${pkgs.tesseract.override { enableLanguages = [ "eng" ]; }}/bin/tesseract"
  ''
  + builtins.readFile ./journey.py;
}
