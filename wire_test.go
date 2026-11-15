package locks

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func requestSeeds() []string {
	return []string{"", "locks/1 get\n", "locks/1 get\nid." + string(abc) + "\n", "locks/1 put\ncreate raw.blake2.YWJj \n", "locks/1 put\r\nreplace raw.blake2.YWJj  x\t\ry \r\n", "locks/1 get\nraw.blake2.YWJj", "locks/1 get\n\n", "locks/2 get\nraw.blake2.YWJj\n", "locks/1 put\nbad raw.blake2.YWJj v\n", "locks/1 put\ncreate\n", "locks/1 put\ncreate raw.blake2.YWJj\n", "locks/1 get\nraw.blake2.YWJj\nid." + string(abc) + "\n", "locks/1 get\nraw.unknown.\n", "locks/1 get\nbad\n", "locks/1 get\nraw.blake2.YWJj\x00\n", "locks/1 get\n\xff\n"}
}
func TestWireRequest(t *testing.T) {
	for _, s := range requestSeeds() {
		_, _ = DecodeRequest(strings.NewReader(s), Limits{})
	}
	req, e := DecodeRequest(strings.NewReader("locks/1 put\r\ncreate raw.blake2.YWJj  x\t\ry \r\r\n"), Limits{})
	if e != nil || req.Items[0].Value != " x\t\ry " {
		t.Fatal(req, e)
	}
	req, e = DecodeRequest(strings.NewReader("locks/1 get\nraw.blake2.YWJj\n"), Limits{})
	if e != nil || req.Items[0].ID != abc || req.Put {
		t.Fatal(req, e)
	}
	for _, l := range []Limits{{BodyBytes: -1}, {BodyBytes: 1}, {LineBytes: 1}} {
		_, e = DecodeRequest(strings.NewReader(requestSeeds()[2]), l)
		requireErr(t, e, ErrLimit)
	}
	_, e = DecodeRequest(strings.NewReader("locks/1 get\nraw.blake2.YQ\nraw.blake2.Yg\n"), Limits{Entries: 1})
	requireErr(t, e, ErrLimit)
	_, e = DecodeRequest(strings.NewReader("locks/1 put\ncreate raw.blake2.YQ long\n"), Limits{ValueBytes: 1})
	requireErr(t, e, ErrLimit)
	_, e = DecodeRequest(failedIO{}, Limits{})
	if e == nil {
		t.Fatal("read error lost")
	}
}
func resultSeeds() []string {
	var out []string
	for _, row := range []string{"found " + string(abc) + " ", "found " + string(abc) + "  x\t\ry \r", "found " + string(abc), "missing " + string(abc), "missing " + string(abc) + " ", "created " + string(abc), "same " + string(abc), "replaced " + string(abc), "conflict " + string(abc), "error " + string(abc) + " denied", "error " + string(abc) + " too-many", "error " + string(abc) + " bad_code", "error " + string(abc), "unknown " + string(abc), "missing bad", "missing " + string(testID(2)), "bad"} {
		out = append(out, "locks/1 results\n"+row+"\n")
	}
	return append(out, "", "locks/2 results\nmissing "+string(abc)+"\n", "locks/1 results\n", "locks/1 results\nmissing "+string(abc), "locks/1 results\nmissing "+string(abc)+"\nmissing "+string(abc)+"\n")
}
func TestWireResults(t *testing.T) {
	for _, s := range resultSeeds() {
		for _, req := range []Request{{Items: []Write{{ID: abc}}}, {Put: true, Items: []Write{{ID: abc, Mode: Create}}}, {Put: true, Items: []Write{{ID: abc, Mode: Replace}}}} {
			_, _ = DecodeResults(strings.NewReader(s), req, Limits{})
		}
	}
	req := Request{Items: []Write{{ID: abc}}}
	out, e := DecodeResults(strings.NewReader(resultSeeds()[1]), req, Limits{})
	if e != nil || out[0].Result.Value != " x\t\ry " {
		t.Fatal(out, e)
	}
	for _, l := range []Limits{{BodyBytes: -1}, {ValueBytes: 1}} {
		_, e = DecodeResults(strings.NewReader("locks/1 results\nfound "+string(abc)+" long\n"), req, l)
		requireErr(t, e, ErrLimit)
	}
	_, e = DecodeResults(strings.NewReader("locks/1 results\n"), Request{}, Limits{})
	requireErr(t, e, ErrProtocol)
}
func errorSeeds() []string {
	return []string{"", "locks/1 error\ndenied\n", "locks/1 error\nnew-code\n", "locks/1 error\ntoo-many 16\n", "locks/1 error\ntoo-many 999999999999999999999999999999999999999\n", "locks/1 error\ntoo-many 15\n", "locks/1 error\ntoo-many 0\n", "locks/1 error\ntoo-many 016\n", "locks/1 error\ntoo-many 1\n", "locks/1 error\ntoo-many \n", "locks/1 error\ntoo-many\n", "locks/1 error\ntoo-many 16a\n", "locks/1 error\nBAD\n", "locks/1 error\ndenied extra\n", "locks/1 results\ndenied\n", "locks/1 error\ndenied\nextra\n"}
}
func TestWireErrors(t *testing.T) {
	for _, s := range errorSeeds() {
		for _, status := range []int{400, 413} {
			_ = DecodeError(strings.NewReader(s), status, Limits{})
		}
	}
	e := DecodeError(strings.NewReader(errorSeeds()[3]), 413, Limits{})
	var count *TooManyError
	if !errors.As(e, &count) || count.Max != 16 {
		t.Fatal(e)
	}
	_ = count.Error()
	e = DecodeError(strings.NewReader(errorSeeds()[4]), 413, Limits{Entries: 32})
	if !errors.As(e, &count) || count.Max != 32 {
		t.Fatal(e)
	}
	e = DecodeError(strings.NewReader(errorSeeds()[3]), 413, Limits{Entries: 1})
	if !errors.As(e, &count) || count.Max != 1 {
		t.Fatal(e)
	}
	requireErr(t, DecodeError(strings.NewReader(errorSeeds()[1]), 400, Limits{BodyBytes: -1}), ErrLimit)
	for _, code := range []string{"denied", "limit", "indeterminate", "unsupported", "unknown"} {
		e := &RemoteError{code}
		if e.Error() == "" {
			t.Fatal(e)
		}
		for _, target := range []error{ErrDenied, ErrLimit, ErrIndeterminate, ErrUnsupported, ErrFormat} {
			_ = errors.Is(e, target)
		}
	}
	if got := errorBody("too-many", 16); got != "locks/1 error\ntoo-many 16\n" {
		t.Fatal(got)
	}
	for _, req := range []Request{{Items: []Write{{ID: abc}}}, {Put: true, Items: []Write{{ID: abc, Mode: Create, Value: "x"}}}} {
		got, e := DecodeRequest(strings.NewReader(encodeRequest(req)), Limits{})
		if e != nil || fmt.Sprint(got) != fmt.Sprint(req) {
			t.Fatal(got, e)
		}
	}
}

func TestResponseBatchIntegrity(t *testing.T) {
	first, second := string(abc), string(testID(1))
	req := Request{Items: []Write{{ID: abc}, {ID: testID(1)}}}
	for _, rows := range []string{"missing " + second + "\nmissing " + first + "\n", "missing " + first + "\n", "missing " + first + "\nmissing " + second + "\nmissing " + second + "\n", "missing " + first + "\nmissing " + second} {
		out, e := DecodeResults(strings.NewReader("locks/1 results\n"+rows), req, Limits{})
		requireErr(t, e, ErrProtocol)
		if out != nil {
			t.Fatal("exposed a partial result")
		}
	}
}

func TestResultEncodingBudgets(t *testing.T) {
	out := []Outcome{
		{Result: Result{ID: abc, Status: Found, Value: " value"}},
		{Result: Result{ID: testID(1)}, Err: ErrIndeterminate},
	}
	want := "locks/1 results\nfound " + string(abc) + "  value\nerror " + string(testID(1)) + " indeterminate\n"
	lineBytes := len("error " + string(testID(1)) + " indeterminate")
	got, err := encodeResults(out, Limits{BodyBytes: len(want), LineBytes: lineBytes})
	if err != nil || got != want {
		t.Fatalf("exact budget: %q, %v", got, err)
	}
	for _, limits := range []Limits{
		{BodyBytes: -1}, {BodyBytes: 1}, {LineBytes: 1},
		{BodyBytes: len(want) - 1}, {LineBytes: lineBytes - 1},
	} {
		got, err := encodeResults(out, limits)
		requireErr(t, err, ErrLimit)
		if got != "" {
			t.Fatal("exposed a partial response")
		}
	}
}
