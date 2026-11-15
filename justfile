set dotenv-load
export GOTOOLCHAIN := "local"
export GOPROXY := env("GOPROXY", "off")
export GOSUMDB := "off"
export GOFLAGS := "-mod=readonly"
export GOWORK := "off"

default:
    @just --list

# Run a main package or Go file. Pass program arguments after the path.
[positional-arguments]
run package *args:
    go run "$@"

# Run all package tests. Extra arguments go to go test.
[positional-arguments]
test *args:
    go test "$@" ./...

# Test with the minimum supported Go version.
test-min:
    nix develop .#go122 --command just test

test-race:
    go test -race -count=1 ./...

coverage:
    scripts/coverage.sh

# Run every fuzz target for the given duration.
[positional-arguments]
fuzz duration="10s":
    FUZZTIME="$1" scripts/fuzz.sh

bench:
    go test -run='^$' -bench=. -benchmem ./...

build:
    go build ./...

# Run all configured hooks and the race tests.
check: hooks test-race

lint:
    go vet ./...
    deadnix --fail flake.nix
    statix check .
    markdownlint README.md

typos:
    typos

fmt-check:
    @files="$(gofmt -l .)"; if [ -n "$files" ]; then printf '%s\n' "$files"; exit 1; fi
    nixfmt --check flake.nix
    just --fmt --check

fmt:
    go fmt ./...
    nixfmt flake.nix
    just --fmt

hooks:
    pre-commit run --all-files

# Check both Go versions and all hooks in the Nix build sandbox.
check-nix:
    nix flake check

# Create a commit with a Conventional Commits message.
commit:
    cz commit
