{
  projectRootFile = "flake.nix";

  programs.gofumpt.enable = true;
  programs.nixfmt.enable = true;
  programs.deadnix.enable = true;
  programs.shellcheck.enable = true;

  settings.global.excludes = [
    "*.lock"
    "go.sum"
    "go.work.sum"
  ];
}
