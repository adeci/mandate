{
  description = "mandate: let agents use your services semi-sanely";

  inputs.nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      treefmtFor =
        pkgs:
        pkgs.treefmt.withConfig {
          runtimeInputs = [
            pkgs.nixfmt
            pkgs.gofumpt
          ];
          settings = {
            tree-root-file = "flake.nix";
            formatter.nixfmt = {
              command = "nixfmt";
              includes = [ "*.nix" ];
            };
            formatter.gofumpt = {
              command = "gofumpt";
              options = [ "-w" ];
              includes = [ "*.go" ];
            };
          };
        };
    in
    {
      packages = forAllSystems (
        pkgs:
        let
          all = pkgs.callPackages ./nix/package.nix { };
        in
        all // { default = all.mandate; }
      );

      nixosModules.default = import ./nix/module.nix self;

      checks = forAllSystems (
        pkgs:
        import ./nix/checks.nix {
          inherit pkgs self;
          treefmt = treefmtFor pkgs;
        }
      );

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go_1_27
            pkgs.gopls
            pkgs.golangci-lint
            pkgs.sqlite-interactive
            pkgs.jq
          ];
          env.GOTOOLCHAIN = "local";
        };
      });

      formatter = forAllSystems treefmtFor;
    };
}
