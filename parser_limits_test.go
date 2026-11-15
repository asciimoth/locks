package locks

import (
	"errors"
	"strings"
	"testing"
)

func TestLeafIgnoredLineStorage(t *testing.T) {
	ignored := strings.Repeat("\n# comment\r\n \r\r\n", 10000)
	original := ignored + string(abc) + " old\r\n" + ignored + "# final"
	leaf, err := DecodeLeaf(strings.NewReader(original), "", Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.lines) != 3 {
		t.Fatalf("ignored lines use %d entries, want 3", len(leaf.lines))
	}
	_, data, _ := leaf.edit(Write{abc, "new", Replace})
	want := ignored + string(abc) + " new\n" + ignored + "# final"
	if string(data) != want {
		t.Fatal("edit changed ignored bytes")
	}
	id, err := Hash([]byte("new key"), "")
	if err != nil {
		t.Fatal(err)
	}
	_, data, _ = leaf.edit(Write{id, "value", Create})
	if string(data) != original+"\n"+string(id)+" value\n" {
		t.Fatal("append changed existing bytes")
	}
}

func TestWireExcessLines(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n", "\r\r\n"} {
		body := "locks/1 get" + ending + strings.Repeat("raw.blake2.YQ"+ending, 10000)
		_, err := DecodeRequest(strings.NewReader(body), Limits{Entries: 16})
		var many *TooManyError
		if !errors.As(err, &many) || many.Max != 16 {
			t.Fatalf("count rejection: %v", err)
		}
		_, err = DecodeResults(strings.NewReader("locks/1 results"+ending+strings.Repeat("missing "+string(abc)+ending, 10000)), Request{Items: []Write{{ID: abc}}}, Limits{})
		requireErr(t, err, ErrProtocol)
		err = DecodeError(strings.NewReader("locks/1 error"+ending+strings.Repeat("denied"+ending, 10000)), 413, Limits{})
		requireErr(t, err, ErrProtocol)
		if errors.As(err, &many) {
			t.Fatal("malformed error permits retry")
		}
	}
	// The framing scan must not allocate an entry for every empty record.
	l, err := (Limits{Entries: 16}).normalized()
	if err != nil {
		t.Fatal(err)
	}
	lines, err := wireLines(strings.NewReader("locks/1 get\n"+strings.Repeat("\n", 1<<20)), l, 17)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 18 {
		t.Fatalf("excess line storage: %d", len(lines))
	}
}
