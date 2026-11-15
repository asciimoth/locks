#!/bin/sh
# Gate regular statement coverage and fuzz-seed coverage of all parsers.
set -eu
go test -coverprofile=coverage.out ./...
go tool cover -func=coverage.out | awk '
    /^total:/ { print; if (($3 + 0) < 95) exit 1 }
'
go test -run '^Fuzz' -coverprofile=fuzz.coverprofile ./...
# OpenLocal is a constructor with a non-Linux branch that Linux CI cannot cover.
go tool cover -func=fuzz.coverprofile | awk '
    BEGIN {
        split("ParseID Hash ParseKey NormalizeValue validName normalized validateWrite validateBatch Endpoint queryKeys ParsePath ParsePathForOS locationStart httpLocation validLocation wireLines DecodeRequest DecodeResults DecodeError readBounded DecodeLeaf marker EncodeLeaf mediaOK shardName", names)
        for (i in names) required[names[i]] = 1
    }
    $2 in required {
        print
        seen[$2] = 1
        if ($3 != "100.0%") failed = 1
    }
    END {
        for (name in required) if (!(name in seen)) { print "Missing function: " name; failed = 1 }
        exit failed
    }
'
