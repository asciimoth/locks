# Usage:
# - Build the CLI: nix build github:asciimoth/locks#locks
# - Run the CLI: nix run github:asciimoth/locks#locks -- --help
# - Add the CLI to a temporary shell: nix shell github:asciimoth/locks#locks
# - Add the CLI to a profile: nix profile add github:asciimoth/locks#locks
# - Install the CLI in a NixOS configuration:
#     inputs.locks.url = "github:asciimoth/locks";
#     environment.systemPackages = [ inputs.locks.packages.${pkgs.system}.locks ];
{
  description = "Development environment for locks";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    git-hooks.url = "github:cachix/git-hooks.nix";
    git-hooks.inputs.nixpkgs.follows = "nixpkgs";
    # Keep Go 1.22 available for compatibility checks.
    nixpkgs-go122.url = "github:NixOS/nixpkgs/nixos-24.11";
  };

  outputs =
    {
      self,
      nixpkgs,
      nixpkgs-go122,
      git-hooks,
      ...
    }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      eachSystem = nixpkgs.lib.genAttrs systems;
      goEnv = {
        GOTOOLCHAIN = "local";
        GOPROXY = "off";
        GOSUMDB = "off";
        GOFLAGS = "-mod=readonly";
        GOWORK = "off";
      };
      goVendorHash = "sha256-Qo/tWy27wr09LzVeTQPEvI+LMvyzpvcCT9R7odsFJto=";
    in
    {
      packages = eachSystem (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          locks = pkgs.buildGoModule {
            pname = "locks";
            version = self.shortRev or self.dirtyShortRev or "dev";
            src = pkgs.lib.cleanSource ./.;
            subPackages = [ "cmd" ];
            proxyVendor = true;
            vendorHash = goVendorHash;
            postInstall = ''
              mv "$out/bin/cmd" "$out/bin/locks"
            '';
            meta.mainProgram = "locks";
          };
        in
        {
          inherit locks;
          default = locks;
        }
      );

      apps = eachSystem (system: {
        locks = {
          type = "app";
          program = "${self.packages.${system}.locks}/bin/locks";
          meta.description = "Locks command-line tool";
        };
        default = self.apps.${system}.locks;
      });

      checks = eachSystem (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          moduleProxy =
            (pkgs.buildGoModule {
              pname = "locks-dependencies";
              version = "1";
              src = pkgs.lib.cleanSource ./.;
              proxyVendor = true;
              vendorHash = goVendorHash;
              modBuildPhase = ''
                export GOPROXY=https://proxy.golang.org
                go mod download
              '';
            }).goModules;
          checkGo =
            go:
            pkgs.runCommand "locks-go-${go.version}-check"
              (
                goEnv
                // {
                  nativeBuildInputs = [
                    go
                    pkgs.git
                    pkgs.stdenv.cc
                  ];
                  src = pkgs.lib.cleanSource ./.;
                  GOPROXY = "file://${moduleProxy}";
                  passthru = { inherit moduleProxy; };
                }
              )
              ''
                export HOME="$TMPDIR"
                export GOCACHE="$TMPDIR/go-cache"
                cp -r "$src" source
                chmod -R u+w source
                cd source
                go build ./...
                go vet ./...
                go test -race -count=1 ./...
                touch "$out"
              '';
          goHook =
            command:
            "${pkgs.coreutils}/bin/env GOTOOLCHAIN=local GOPROXY=file://${moduleProxy} GOSUMDB=off GOFLAGS=-mod=readonly GOWORK=off ${pkgs.go}/bin/go ${command} ./...";
        in
        {
          go = checkGo pkgs.go;
          go122 = checkGo nixpkgs-go122.legacyPackages.${system}.go_1_22;
          pre-commit-check = git-hooks.lib.${system}.run {
            src = ./.;
            default_stages = [ "pre-commit" ];
            hooks = {
              check-added-large-files.enable = true;
              check-case-conflicts.enable = true;
              check-merge-conflicts.enable = true;
              check-toml.enable = true;
              check-yaml.enable = true;
              commitizen = {
                enable = true;
                stages = [ "commit-msg" ];
              };
              deadnix.enable = true;
              end-of-file-fixer.enable = true;
              gofmt.enable = true;
              markdownlint.enable = true;
              nixfmt.enable = true;
              statix.enable = true;
              trim-trailing-whitespace.enable = true;
              typos.enable = true;
              just-fmt = {
                enable = true;
                name = "Justfile format";
                entry = "${pkgs.just}/bin/just --fmt --check";
                files = "^justfile$";
                pass_filenames = false;
              };
              go-vet = {
                enable = true;
                name = "Go vet";
                entry = goHook "vet";
                files = "(\\.go$|^go\\.(mod|sum)$)";
                pass_filenames = false;
                require_serial = true;
              };
              go-test = {
                enable = true;
                name = "Go tests";
                entry = goHook "test";
                files = "(\\.go$|^go\\.(mod|sum)$)";
                pass_filenames = false;
                require_serial = true;
              };
            };
          };
        }
      );

      formatter = eachSystem (system: nixpkgs.legacyPackages.${system}.nixfmt);

      devShells = eachSystem (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          hook = self.checks.${system}.pre-commit-check;
          shellFor =
            go:
            pkgs.mkShell (
              goEnv
              // {
                GOPROXY = "file://${self.checks.${system}.go.moduleProxy}";
                packages = [
                  go
                  pkgs.just
                ]
                ++ builtins.filter (p: p != pkgs.go) hook.enabledPackages;
                inherit (hook) shellHook;
              }
            );
        in
        {
          default = shellFor pkgs.go;
          go122 = shellFor nixpkgs-go122.legacyPackages.${system}.go_1_22;
        }
      );
    };
}
