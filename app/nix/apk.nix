# The debug-signed x86_64 APK that ui/android/apk.sh builds, for the emulator.
{
  pkgs,
  rust,
  sdk,
}:
let
  inherit (pkgs) lib;
  rustPlatform = pkgs.makeRustPlatform {
    cargo = rust;
    rustc = rust;
  };
in
pkgs.stdenv.mkDerivation {
  name = "mandate-x86_64.apk";
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../Cargo.toml
      ../Cargo.lock
      ../core
      ../ui
    ];
  };
  cargoDeps = rustPlatform.importCargoLock {
    lockFile = ../Cargo.lock;
    outputHashes."gpui-pre-mobile-0.1.0" = "sha256-OsZGo2xypJdkXHT0Fng1yVAuVG7ZeVnhWSFONTZ3IGk=";
  };
  nativeBuildInputs = [
    rustPlatform.cargoSetupHook
    rust
    pkgs.cargo-ndk
    pkgs.jdk17
    pkgs.zip
  ];
  env = {
    ANDROID_HOME = sdk;
    ANDROID_NDK_HOME = "${sdk}/ndk-bundle";
  };
  buildPhase = "bash ui/android/apk.sh x86_64";
  installPhase = "cp ui/android/build/x86_64/mandate.apk $out";
}
