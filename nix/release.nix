# `nix build .#release`: the files of a GitHub release. A tarball per arch
# holds mandate and the bundled connectors as static binaries, so that they
# run on any systemd Linux; SHA256SUMS covers the tarballs, and install.sh
# fetches them. The other arch is cross-compiled.
{ pkgs }:
let
  inherit (pkgs) lib;
  here = pkgs.stdenv.hostPlatform.parsed.cpu.name;
  native = pkgs.callPackages ./package.nix { };

  tarball =
    {
      arch,
      gnu,
      musl,
      machine,
    }:
    let
      # Package sets for arch: glibc for the pure Go builds, and musl, all
      # static, for Signal.
      p = if arch == here then pkgs else pkgs.pkgsCross.${gnu};
      s = if arch == here then pkgs.pkgsStatic else pkgs.pkgsCross.${musl}.pkgsStatic;
      # The flake checks run the tests; a release only needs the binaries.
      # Go vendors the same modules for every platform, but the vendoring
      # elsewhere doesn't match the hash, so the native build's are reused.
      release =
        set: overrides: name:
        (set.callPackages ./package.nix overrides).${name}.overrideAttrs {
          inherit (native.${name}) goModules;
          doCheck = false;
        };
      # Pure Go, so static already.
      go = release p { };
      # Signal links libsignal-ffi, whose own tests don't link statically.
      libsignal-ffi = s.libsignal-ffi.overrideAttrs { doCheck = false; };
      signal = (release s { inherit libsignal-ffi; } "mandate-signal").overrideAttrs (old: {
        ldflags = old.ldflags ++ [
          "-linkmode=external"
          "-extldflags=-static"
        ];
      });
      connectors =
        lib.genAttrs [ "whatsapp" "discord" "fastmail" "gmessages" "matrix" ] (n: go "mandate-${n}")
        // {
          inherit signal;
        };
    in
    ''
      d=$TMPDIR/${arch}
      mkdir -p $d/connectors
      cp ${lib.getExe (go "mandate")} $d/mandate
      ${lib.concatMapAttrsStringSep "\n" (n: c: "cp ${lib.getExe c} $d/connectors/${n}") connectors}
      for f in $d/mandate $d/connectors/*; do
        if readelf -l "$f" | grep -q 'program interpreter'; then
          echo "$f is linked dynamically" >&2
          exit 1
        fi
        if ! readelf -h "$f" | grep -q 'Machine: *${machine}'; then
          echo "$f is not for ${arch}" >&2
          exit 1
        fi
      done
      chmod 0755 $d/mandate $d/connectors/*
      tar -C $d --sort=name --mtime=@1 --owner=0 --group=0 --numeric-owner -c mandate connectors |
        gzip -9n >$out/mandate-linux-${arch}.tar.gz
    '';
in
pkgs.runCommand "mandate-release" { nativeBuildInputs = [ pkgs.binutils ]; } ''
  mkdir $out
  ${tarball {
    arch = "x86_64";
    gnu = "gnu64";
    musl = "musl64";
    machine = "Advanced Micro Devices X86-64";
  }}
  ${tarball {
    arch = "aarch64";
    gnu = "aarch64-multiplatform";
    musl = "aarch64-multiplatform-musl";
    machine = "AArch64";
  }}
  cp ${../install.sh} $out/install.sh
  cd $out && sha256sum mandate-linux-*.tar.gz >SHA256SUMS
''
