# packages.x86_64-linux.desktop: the Linux app, with its .desktop file (which
# also takes mandate:// links), icon, and the notifier's user unit and
# autostart entry.
{
  pkgs,
  rust,
  runtime,
}:
let
  inherit (pkgs) lib;
  rustPlatform = pkgs.makeRustPlatform {
    cargo = rust;
    rustc = rust;
  };
in
rustPlatform.buildRustPackage {
  pname = "mandate-app";
  version = "0.0.0";
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../Cargo.toml
      ../Cargo.lock
      ../core
      ../ui
    ];
  };
  cargoLock = {
    lockFile = ../Cargo.lock;
    outputHashes."gpui-pre-mobile-0.1.0" = "sha256-OsZGo2xypJdkXHT0Fng1yVAuVG7ZeVnhWSFONTZ3IGk=";
  };
  cargoBuildFlags = [
    "-p"
    "mandate-app"
  ];
  # The checks run the tests.
  doCheck = false;
  nativeBuildInputs = [ pkgs.pkg-config ];
  buildInputs = runtime ++ [ pkgs.fontconfig ];
  postInstall = ''
    install -Dm644 ui/linux/dev.mandate.app.desktop -t $out/share/applications
    install -Dm644 ui/linux/dev.mandate.app.svg -t $out/share/icons/hicolor/scalable/apps
    install -Dm644 ui/linux/dev.mandate.app.background.desktop -t $out/etc/xdg/autostart
    mkdir -p $out/lib/systemd/user
    substitute ui/linux/mandate-background.service $out/lib/systemd/user/mandate-background.service \
      --subst-var out
  '';
  # gpui dlopens some of these, so the linker never records them.
  postFixup = ''
    patchelf --add-rpath ${lib.makeLibraryPath runtime} $out/bin/mandate-app
  '';
  meta.mainProgram = "mandate-app";
}
