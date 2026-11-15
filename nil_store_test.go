package locks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNilStoreReceivers(t *testing.T) {
	ctx := context.Background()
	var memory *Memory
	var local *Local
	var remote *HTTPStore
	var client *Client

	for name, store := range map[string]Store{"memory": memory, "local": local, "http": remote} {
		t.Run(name, func(t *testing.T) {
			_, err := store.Get(ctx, abc)
			requireErr(t, err, ErrFormat)
			_, err = store.Put(ctx, abc, "value", Create)
			requireErr(t, err, ErrFormat)
		})
	}
	for _, store := range []BatchStore{local, remote} {
		_, err := store.GetBatch(ctx, []ID{abc})
		requireErr(t, err, ErrFormat)
		_, err = store.PutBatch(ctx, []Write{{ID: abc, Value: "value", Mode: Create}})
		requireErr(t, err, ErrFormat)
	}
	requireErr(t, local.Validate(ctx), ErrFormat)
	requireErr(t, local.Recover(ctx), ErrFormat)
	for _, report := range []Report{
		client.Get(ctx, "key"), client.GetWithPolicy(ctx, "key", First),
		client.Put(ctx, "key", "value", Create), client.PutWithPolicy(ctx, "key", "value", Create, WriteFirst),
		client.Create(ctx, "key", "value"), client.Replace(ctx, "key", "value"),
	} {
		requireErr(t, report.Err, ErrFormat)
	}
	if client.Locations() != nil || client.Chain() != nil {
		t.Fatal("nil client has configuration")
	}
}

func TestUninitializedHTTPStore(t *testing.T) {
	var store HTTPStore
	_, err := store.Get(context.Background(), abc)
	requireErr(t, err, ErrFormat)
}

func TestUninitializedLocalStore(t *testing.T) {
	var store Local
	ctx := context.Background()
	_, err := store.Get(ctx, abc)
	requireErr(t, err, ErrFormat)
	_, err = store.Put(ctx, abc, "value", Create)
	requireErr(t, err, ErrFormat)
	_, err = store.GetBatch(ctx, []ID{abc})
	requireErr(t, err, ErrFormat)
	_, err = store.PutBatch(ctx, []Write{{ID: abc, Value: "value", Mode: Create}})
	requireErr(t, err, ErrFormat)
	requireErr(t, store.Validate(ctx), ErrFormat)
	requireErr(t, store.Recover(ctx), ErrFormat)
}

func TestUninitializedHandler(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/lock?key=id."+string(abc), nil)
	for _, handler := range []*Handler{nil, {}} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusInternalServerError || response.Body.String() != errorBody("unavailable", 0) {
			t.Fatalf("uninitialized handler: HTTP %d %q", response.Code, response.Body.String())
		}
	}
}
