{
  description = "mandate: let agents use your services semi-sanely";

  inputs.nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
  inputs.treefmt-nix = {
    url = "github:numtide/treefmt-nix";
    inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs =
    {
      self,
      nixpkgs,
      treefmt-nix,
    }:
    let
      inherit (nixpkgs) lib;
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      treefmtFor = pkgs: treefmt-nix.lib.evalModule pkgs ./nix/treefmt.nix;
    in
    {
      packages = forAllSystems (
        pkgs:
        let
          all = pkgs.callPackages ./nix/package.nix { };
        in
        all
        // {
          default = all.mandate;
          release = import ./nix/release.nix { inherit pkgs; };
        }
      );

      nixosModules.default = import ./nix/module.nix self;

      apps = forAllSystems (pkgs: {
        demo = {
          type = "app";
          program = lib.getExe (import ./nix/demo-vm.nix { inherit self nixpkgs pkgs; });
          meta.description = "Boot a throwaway mandate server VM and print its setup password";
        };
      });

      checks = forAllSystems (
        pkgs:
        let
          packages = removeAttrs self.packages.${pkgs.stdenv.hostPlatform.system} [
            "default"
            "release"
          ];
          lint = pkgs.callPackage ./nix/lint.nix { };
          prefix = p: lib.mapAttrs' (n: lib.nameValuePair "${p}-${n}");
        in
        prefix "package" packages
        // prefix "lint" (lib.mapAttrs (_: lint) packages)
        // {
          treefmt = (treefmtFor pkgs).config.build.check self;
          conformance = import ./nix/conformance.nix { inherit self pkgs; };
        }
        // lib.optionalAttrs (pkgs.stdenv.hostPlatform.system == "x86_64-linux") {
          vm = pkgs.testers.runNixOSTest (import ./nix/vm-test.nix { inherit self pkgs; });
        }
      );

      formatter = forAllSystems (pkgs: (treefmtFor pkgs).config.build.wrapper);

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go_1_27
            pkgs.gopls
            pkgs.golangci-lint
            pkgs.sqlite-interactive
            pkgs.jq
            (treefmtFor pkgs).config.build.wrapper
          ];
          env.GOTOOLCHAIN = "local";
        };
      });
    };
}
