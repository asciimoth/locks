package locks

import (
	"context"
	"path/filepath"
	"testing"
)

type pointerContext struct{ context.Context }

func TestNilContexts(t *testing.T) {
	remote, err := OpenHTTP("https://example.invalid", HTTPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stores := []Store{&Memory{}, remote}
	var local *Local
	if localSupported {
		local, err = OpenLocal(filepath.Join(t.TempDir(), "locks"), LocalOptions{})
		if err != nil {
			t.Fatal(err)
		}
		stores = append(stores, local)
		client, err := OpenWithOptions(ClientOptions{Locations: []Location{{Address: filepath.Join(t.TempDir(), "parent", "locks")}}})
		if err != nil {
			t.Fatal(err)
		}
		stores = append(stores, client.Chain()[0].Store)
	}
	for _, ctx := range []context.Context{nil, (*pointerContext)(nil)} {
		for _, store := range stores {
			_, err := store.Get(ctx, abc)
			requireErr(t, err, ErrFormat)
			_, err = store.Put(ctx, abc, "value", Create)
			requireErr(t, err, ErrFormat)
			if batch, ok := store.(BatchStore); ok {
				_, err = batch.GetBatch(ctx, []ID{abc})
				requireErr(t, err, ErrFormat)
				_, err = batch.PutBatch(ctx, []Write{{abc, "value", Create}})
				requireErr(t, err, ErrFormat)
			}
		}
		c := Chain{{Store: &Memory{}}}
		requireErr(t, c.Get(ctx, abc, First).Err, ErrFormat)
		requireErr(t, c.Put(ctx, abc, "value", Create, WriteFirst).Err, ErrFormat)
		if local != nil {
			requireErr(t, local.Validate(ctx), ErrFormat)
			requireErr(t, local.Recover(ctx), ErrFormat)
		}
	}
}

func TestTypedNilStores(t *testing.T) {
	var memory *Memory
	_, err := NewHandler(memory, HandlerOptions{})
	requireErr(t, err, ErrFormat)
	c := Chain{{Store: memory}}
	requireErr(t, c.Get(context.Background(), abc, First).Err, ErrUnsupported)
	requireErr(t, c.Put(context.Background(), abc, "value", Create, WriteFirst).Err, ErrUnsupported)
	c = append(c, Position{Store: &Memory{}})
	if r := c.Put(context.Background(), abc, "value", Create, WriteFirst); r.Err != nil || r.Result.Status != Created {
		t.Fatalf("nil store fallback: %+v", r)
	}
}
