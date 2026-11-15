#!/bin/sh
# Gate regular statement coverage and fuzz-seed coverage of all parsers.
set -eu
go test -coverprofile=coverage.out ./...
go tool cover -func=coverage.out | awk '
    /^total:/ { print; if (($3 + 0) < 95) exit 1 }
'
# Keep command coverage above the same floor as the module.
awk '
    $1 ~ /^github.com\/asciimoth\/locks\/cmd\// {
        total += $2
        if ($3 > 0) covered += $2
    }
    END {
        if (total == 0) { print "Missing CLI coverage"; exit 1 }
        percent = 100 * covered / total
        printf "CLI statement coverage: %.1f%%\n", percent
        if (percent < 95) exit 1
    }
' coverage.out
go test -run '^Fuzz' -coverprofile=fuzz.coverprofile ./...
# OpenLocal has platform-specific branches outside this parser coverage gate.
go tool cover -func=fuzz.coverprofile | awk '
    BEGIN {
        split("ParseID Hash ParseKey NormalizeValue validName normalized validateWrite validateLocalWrite validateBatch Endpoint queryKeys ParsePath ParsePathForOS locationStart httpLocation validLocation wireLines DecodeRequest DecodeResults DecodeError nilInput Read readBounded DecodeLeaf marker EncodeLeaf mediaOK shardName", names)
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
