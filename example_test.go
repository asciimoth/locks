package locks_test

import (
	"context"
	"fmt"

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
