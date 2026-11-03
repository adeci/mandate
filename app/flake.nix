{
  description = "mandate app";

  inputs.nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
  inputs.rust-overlay = {
    url = "github:oxalica/rust-overlay";
    inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs =
    { nixpkgs, rust-overlay, ... }:
    let
      pkgs = import nixpkgs {
        system = "x86_64-linux";
        overlays = [ rust-overlay.overlays.default ];
        config = {
          allowUnfree = true;
          android_sdk.accept_license = true;
        };
      };
      rust = pkgs.rust-bin.stable.latest.default.override {
        targets = [
          "aarch64-linux-android"
          "x86_64-linux-android"
        ];
      };
      android = pkgs.androidenv.composeAndroidPackages {
        platformVersions = [ "35" ];
        buildToolsVersions = [ "35.0.0" ];
        includeNDK = true;
        includeEmulator = true;
        includeSystemImages = true;
        systemImageTypes = [ "default" ];
        abiVersions = [ "x86_64" ];
      };
      sdk = "${android.androidsdk}/libexec/android-sdk";
      # gpui dlopens these, so the linker never records them.
      runtime = with pkgs; [
        vulkan-loader
        wayland
        libxkbcommon
        libxcb
        libx11
      ];
    in
    {
      devShells.x86_64-linux.default = pkgs.mkShell {
        packages = with pkgs; [
          rust
          cargo-ndk
          android.androidsdk
          jdk17
          pkg-config
          fontconfig
          freetype
          xvfb-run
          imagemagick
          zip
        ];
        buildInputs = runtime;
        env = {
          ANDROID_HOME = sdk;
          ANDROID_NDK_HOME = "${sdk}/ndk-bundle";
          LD_LIBRARY_PATH = pkgs.lib.makeLibraryPath runtime;
        };
      };
    };
}
