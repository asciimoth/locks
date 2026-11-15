#!/bin/sh
# Run every fuzz target. Set FUZZTIME to increase each target's run time.
set -eu
# Go fuzzing accepts one package per invocation.
packages=$(go list ./...)
for package in $packages; do
    # Keep discovery separate so a build failure stops the script.
    targets=$(go test -list '^Fuzz' "$package")
    for target in $(printf '%s\n' "$targets" | sed -n '/^Fuzz/p'); do
        go test -run '^$' -fuzz "^${target}$" -fuzztime "${FUZZTIME:-10s}" -parallel 2 "$package"
    done
done
