#!/bin/sh
# Run every fuzz target. Set FUZZTIME to increase each target's run time.
set -eu
# Keep discovery separate from the pipeline so a build failure stops the script.
targets=$(go test -list '^Fuzz' ./...)
for target in $(printf '%s\n' "$targets" | sed -n '/^Fuzz/p'); do
    go test -run '^$' -fuzz "^${target}$" -fuzztime "${FUZZTIME:-10s}" -parallel 2 ./...
done
