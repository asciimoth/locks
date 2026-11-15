#!/bin/sh
# Run every fuzz target. Set FUZZTIME to increase each target's run time.
set -eu
for target in $(go test -list '^Fuzz' ./... | sed -n '/^Fuzz/p'); do
    go test -run '^$' -fuzz "^${target}$" -fuzztime "${FUZZTIME:-10s}" -parallel 2 ./...
done
