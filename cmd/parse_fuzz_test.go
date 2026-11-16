package main

import (
	"reflect"
	"testing"
)

// The separator must keep key and value bytes separate from options.
func FuzzLiteralOperands(f *testing.F) {
	for _, seed := range [][2]string{{"", ""}, {"--path", "--verbose"}, {"-", "--"}, {"key with spaces", "value\twith\rwhitespace"}, {"雪\nkey", "value\x00bytes"}} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, key, value string) {
		a := &app{}
		command, operands, err := a.parse([]string{"--path=store", "write", "--", key, value})
		if err != nil || command != "write" || !reflect.DeepEqual(operands, []string{key, value}) || !reflect.DeepEqual(a.options.paths, paths{"store"}) || a.options.verbose || a.options.all || a.options.create {
			t.Fatalf("operands changed: command=%q operands=%q options=%+v error=%v", command, operands, a.options, err)
		}
	})
}
