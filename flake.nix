{
  description = "towa, a coding agent that speaks the Agent Client Protocol";

  # nixos-26.05 ships buildGo127Module with the Go of go.mod (1.27.3).
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (pkgs: rec {
        towa = pkgs.callPackage ./nix/package.nix {
          rev = self.shortRev or self.dirtyShortRev or "unknown";
        };
        default = towa;
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go_1_27
            pkgs.gopls
            pkgs.golangci-lint
            pkgs.ripgrep
          ];
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
