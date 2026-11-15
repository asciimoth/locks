package locks

import (
	"context"
	"errors"
	"testing"
)

type stubStore struct {
	read       Result
	readErr    error
	write      Result
	writeErr   error
	gets, puts int
}

func (s *stubStore) Get(_ context.Context, id ID) (Result, error) {
	s.gets++
	r := s.read
	if r.ID == "" {
		r.ID = id
	}
	return r, s.readErr
}
func (s *stubStore) Put(_ context.Context, id ID, _ string, _ Mode) (Result, error) {
	s.puts++
	r := s.write
	if r.ID == "" {
		r.ID = id
	}
	return r, s.writeErr
}
func valueStore(value string) *Memory {
	m := &Memory{}
	_, _ = m.Put(context.Background(), abc, value, Create)
	return m
}
func TestChainReads(t *testing.T) {
	ctx := context.Background()
	fail := errors.New("last eligible")
	other := errors.New("witness failed")
	tests := []struct {
		name   string
		chain  Chain
		policy ReadPolicy
		status Status
		value  string
		err    error
	}{
		{"first ignores witness", Chain{{Store: valueStore("red"), CheckOnly: true}, {Store: valueStore("blue")}}, First, Found, "blue", nil},
		{"verify earlier witness", Chain{{Store: valueStore("red"), CheckOnly: true}, {Store: valueStore("blue")}}, Verify, Conflict, "", nil},
		{"matching witness", Chain{{Store: valueStore("blue\r"), CheckOnly: true}, {Store: valueStore("blue")}}, Verify, Found, "blue", nil},
		{"only check hits", Chain{{Store: valueStore("red"), CheckOnly: true}, {Store: &Memory{}}}, Verify, Missing, "", nil},
		{"only eligible errors", Chain{{Store: &stubStore{readErr: fail}}, {Store: valueStore("red"), CheckOnly: true}, {Store: &stubStore{readErr: other}, CheckOnly: true}}, Verify, "", "", fail},
		{"eligible miss beats error", Chain{{Store: &Memory{}}, {Store: &stubStore{readErr: fail}}}, Verify, Missing, "", nil},
		{"eligible hit beats error", Chain{{Store: &stubStore{readErr: fail}}, {Store: valueStore("blue")}}, First, Found, "blue", nil},
		{"no source", Chain{{Store: valueStore("blue"), CheckOnly: true}}, Verify, "", "", ErrNoSource},
		{"empty", nil, First, "", "", ErrNoSource},
		{"bad value", Chain{{Store: &stubStore{read: Result{Status: Found, Value: "\x00"}}}}, Verify, "", "", ErrFormat},
		{"bad check value", Chain{{Store: &stubStore{read: Result{Status: Found, Value: "\x00"}}, CheckOnly: true}, {Store: &Memory{}}}, Verify, Missing, "", nil},
		{"bad backend status", Chain{{Store: &stubStore{read: Result{Status: Created}}}}, Verify, "", "", ErrProtocol},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.chain.Get(ctx, abc, tt.policy)
			if !errors.Is(r.Err, tt.err) || r.Result.Status != tt.status || r.Result.Value != tt.value {
				t.Fatalf("%+v", r)
			}
		})
	}
	r := Chain{}.Get(ctx, "bad", First)
	requireErr(t, r.Err, ErrFormat)
	r = Chain{}.Get(ctx, abc, "bad")
	requireErr(t, r.Err, ErrFormat)
}
func TestChainWritesAndReplication(t *testing.T) {
	ctx := context.Background()
	a, b := &Memory{}, valueStore("red")
	third := &stubStore{write: Result{ID: abc, Status: Created}}
	c := Chain{{Store: a}, {Store: b}, {Store: third}}
	r := c.Put(ctx, abc, "blue", Create, WriteAll)
	if r.Err != nil || r.Result.Status != Conflict || r.Attempts[0].Result.Status != Created || r.Attempts[2].Attempted || third.puts != 0 {
		t.Fatal(r)
	}
	getWant(t, a, abc, Found, "blue")
	failure := errors.New("offline")
	c = Chain{{Store: &Memory{}}, {Store: &stubStore{writeErr: failure}}}
	r = c.Put(ctx, abc, "blue", Create, WriteAll)
	if r.Err != nil || r.Attempts[1].Err != failure {
		t.Fatal(r)
	}
	c = Chain{{Store: &stubStore{writeErr: ErrIndeterminate}}, {Store: &Memory{}}}
	r = c.Put(ctx, abc, "blue", Create, WriteFirst)
	if r.Err != nil || !errors.Is(r.Attempts[0].Err, ErrIndeterminate) {
		t.Fatal(r)
	}
	for _, policy := range []WritePolicy{WriteFirst, WriteAll} {
		c = Chain{{Store: &Memory{}, ReadOnly: true}, {Store: &stubStore{writeErr: failure}}}
		r = c.Put(ctx, abc, "blue", Create, policy)
		requireErr(t, r.Err, failure)
		if !r.Attempts[0].Skipped {
			t.Fatal(r)
		}
		r = (Chain{{Store: &Memory{}, ReadOnly: true}}).Put(ctx, abc, "blue", Create, policy)
		requireErr(t, r.Err, ErrNoWritableStore)
	}
	r = (Chain{{Store: &stubStore{write: Result{Status: Found}}}}).Put(ctx, abc, "", Create, WriteAll)
	requireErr(t, r.Err, ErrProtocol)
	r = Chain{}.Put(ctx, "bad", "", Create, WriteFirst)
	requireErr(t, r.Err, ErrFormat)
	r = Chain{}.Put(ctx, abc, "", Create, "bad")
	requireErr(t, r.Err, ErrFormat)
	for _, readonly := range []bool{false, true} {
		a, b := valueStore("blue"), &Memory{}
		c := Chain{{Store: a, ReadOnly: true}, {Store: b, CheckOnly: true, ReadOnly: readonly}}
		read := c.Get(ctx, abc, Verify)
		write := c.WriteBack(ctx, read, WriteAll)
		if readonly {
			requireErr(t, write.Err, ErrNoWritableStore)
			getWant(t, b, abc, Missing, "")
		} else {
			if write.Err != nil {
				t.Fatal(write)
			}
			getWant(t, b, abc, Found, "blue")
		}
		c[0].Store = &Memory{}
		read = c.Get(ctx, abc, Verify)
		if read.Result.Status != Missing {
			t.Fatal(read)
		}
	}
	requireErr(t, c.WriteBack(ctx, Report{Err: failure}, WriteAll).Err, failure)
	requireErr(t, c.WriteBack(ctx, Report{Result: Result{Status: Conflict}}, WriteAll).Err, ErrNoSource)
}

func TestChainInvalidBackends(t *testing.T) {
	c := Chain{{Store: nil}}
	ctx := context.Background()
	requireErr(t, c.Get(ctx, abc, Verify).Err, ErrUnsupported)
	requireErr(t, c.Put(ctx, abc, "", Create, WriteAll).Err, ErrUnsupported)
	for _, mode := range []Mode{Create, Replace} {
		status := Replaced
		if mode == Replace {
			status = Conflict
		}
		c = Chain{{Store: &stubStore{write: Result{Status: status}}}}
		requireErr(t, c.Put(ctx, abc, "", mode, WriteFirst).Err, ErrProtocol)
	}
	wrong := testID(1)
	c = Chain{{Store: &stubStore{read: Result{ID: wrong, Status: Found, Value: "wrong"}}}, {Store: valueStore("right")}}
	r := c.Get(ctx, abc, First)
	if r.Err != nil || r.Result.Status != Found || r.Result.Value != "right" {
		t.Fatal(r)
	}
	requireErr(t, r.Attempts[0].Err, ErrProtocol)
	c = Chain{{Store: &stubStore{write: Result{ID: wrong, Status: Conflict}}}, {Store: &Memory{}}}
	r = c.Put(ctx, abc, "right", Create, WriteFirst)
	if r.Err != nil || r.Result.ID != abc || r.Result.Status != Created || !r.Attempts[1].Attempted {
		t.Fatal(r)
	}
	requireErr(t, r.Attempts[0].Err, ErrProtocol)
}
