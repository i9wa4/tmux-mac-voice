{
  description = "Reproducible checks for the tmux-mac-voice prototype";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs = { nixpkgs, ... }:
    let
      systems = [ "aarch64-darwin" "x86_64-darwin" "aarch64-linux" "x86_64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems
        (system: f (import nixpkgs { inherit system; }));
    in {
      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [ pkgs.go pkgs.nodejs_24 ];
        };
      });

      checks = forAllSystems (pkgs: {
        go = pkgs.buildGoModule {
          pname = "tmux-mac-voice-check";
          version = "0";
          src = ./.;
          vendorHash = "sha256-1ckWAhiyEgGgIk/9Y/bgj4msc4eNVpsRgyIpxK8hDks=";
          checkPhase = ''
            runHook preCheck
            go test ./...
            go test -race ./...
            runHook postCheck
          '';
        };

        browser = pkgs.runCommand "tmux-mac-voice-browser-check"
          { nativeBuildInputs = [ pkgs.nodejs_24 ]; src = ./.; } ''
            cd "$src"
            node --check web/app.js
            node --check web/worklet.js
            node --test app_test.cjs
            touch "$out"
          '';
      });
    };
}
