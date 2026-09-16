{
  description = "OCSF Explorer development and CI tools";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
  outputs = { self, nixpkgs }:
    let
      systems = [ "aarch64-darwin" "x86_64-darwin" "x86_64-linux" "aarch64-linux" ];
      eachSystem = nixpkgs.lib.genAttrs systems;
    in {
      devShells = eachSystem (system:
        let pkgs = import nixpkgs { inherit system; }; in {
          default = pkgs.mkShell {
            packages = with pkgs; [ nodejs_24 go direnv git gh openssh curl jq shellcheck ];
            shellHook = ''
              export OCSF_DEV_SHELL=1
            '';
          };
        });
    };
}
