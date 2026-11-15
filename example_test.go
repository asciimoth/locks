package locks_test

import (
	"context"
	"fmt"
	"net/http/httptest"

	"github.com/asciimoth/locks"
)

func ExampleMemory() {
	id, _ := locks.Hash([]byte("org.example.editor/v1\x00theme"), "blake2")
	store := &locks.Memory{}
	first, _ := store.Put(context.Background(), id, "blue", locks.Create)
	second, _ := store.Put(context.Background(), id, "red", locks.Create)
	read, _ := store.Get(context.Background(), id)
	fmt.Println(first.Status, second.Status, read.Value)
	// Output: created conflict blue
}
func ExampleChain_Get() {
	ctx := context.Background()
	id, _ := locks.Hash([]byte("key"), "")
	source, witness := &locks.Memory{}, &locks.Memory{}
	_, _ = source.Put(ctx, id, "blue", locks.Create)
	_, _ = witness.Put(ctx, id, "red", locks.Create)
	chain := locks.Chain{{Store: witness, CheckOnly: true, ReadOnly: true}, {Store: source}}
	report := chain.Get(ctx, id, locks.Verify)
	fmt.Println(report.Result.Status, report.Err)
	// Output: conflict <nil>
}

func ExampleClient() {
	// Use an in-process server to keep the example independent of local paths.
	handler, err := locks.NewHandler(&locks.Memory{}, locks.HandlerOptions{})
	if err != nil {
		panic(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	// Applications can use locks.Open() for OS defaults, or
	// locks.Open(".lock/locks") for a project store first.
	client, err := locks.OpenWithOptions(locks.ClientOptions{
		Locations: []locks.Location{{Address: server.URL}},
	})
	if err != nil {
		panic(err)
	}
	ctx := context.Background()
	written := client.Create(ctx, "org.example.editor/v1\x00theme", "blue")
	if written.Err != nil {
		panic(written.Err)
	}
	read := client.Get(ctx, "org.example.editor/v1\x00theme")
	if read.Err != nil {
		panic(read.Err)
	}
	fmt.Println(written.Result.Status, read.Result.Status, read.Result.Value)
	// Output: created found blue
}
