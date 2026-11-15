# locks

A Go library for [Locks v1](spec.md). Requires Go 1.22 or later.

The library includes canonical key hashing, leaf files, automatic shard growth,
local recovery, an HTTP client and server, ordered store chains, and `LOCK_PATH`
parsing. It supports BLAKE2b-256 and SHA-256 raw keys. It preserves valid
prehashed identifiers with unknown algorithm names.

## Basic use

```go
id, err := locks.Hash([]byte("org.example.editor/v1\x00theme"), "blake2")
if err != nil {
    return err
}
store, err := locks.OpenLocal("/absolute/project/.lock/locks", locks.LocalOptions{})
if err != nil {
    return err
}
result, err := store.Put(ctx, id, "blue", locks.Create)
```

Create the parent directory before opening the store. An absent root is an empty
store. `OpenLocal` checks configuration and does not create files. Operations
report parent access errors. A `Local` holds no file handles between operations
and needs no `Close` call. Its configuration is fixed after construction.

`Get` returns `Found` with a value or `Missing`. An empty value is a valid hit.
`Put` returns `Created`, `Same`, `Replaced`, or `Conflict`. A conflict leaves the
stored value intact and has no operational error. `Replace` permits overwrite;
`Create` records a first observation. Values must be UTF-8 with no NUL or LF.
Every boundary removes trailing CR bytes. Other bytes retain their spelling.

Use `ParseID` to validate an external identifier. `Hash` hashes the exact supplied
bytes. It does not normalize paths, Unicode, URLs, or line endings. An empty
algorithm name selects `blake2`. Applications must bound input to `Hash` before
calling it if they accept untrusted raw keys.

`Memory` is an in-process store. Its zero value is ready for concurrent use.

## Local storage and recovery

`OpenLocal` supports Linux, macOS, OpenBSD and Windows. Use an absolute native
path on a filesystem with coherent byte-range locks and atomic same-directory
renames. Stores use the same tracked format and support **process-crash
recovery** on all four platforms. Recovery after power loss requires filesystem
guarantees beyond this protocol. Other targets return `ErrUnsupported` from
`OpenLocal`. The HTTP, memory, format and chain APIs are portable.

| Platform | Guard locking | Access controls on writes |
| --- | --- | --- |
| Linux | `fcntl` record locks | Ownership, modes, xattrs and POSIX ACLs |
| macOS | `fcntl` record locks | Ownership, modes, xattrs and native ACLs |
| OpenBSD | `fcntl` record locks | Ownership and permission bits |
| Windows | `LockFileEx` and `UnlockFileEx` | Owner, group and DACL |

Windows access controls require a filesystem with persistent ACLs, such as
NTFS. On Windows, new leaves use `FileMode` to grant rights to the owner, primary
group and everyone. During publication and recovery, sharing violations from
external handles are retried within the operation's lock timeout. On OpenBSD,
use a filesystem that represents access through Unix ownership and modes. On shared
storage, all clients must observe each other's locks and renames.

The root starts as one file. A changed leaf with more than 512 records splits by
Base32 prefix. Splits stop at 52 digest characters. Ordinary edits preserve
ignored lines and record positions. Splits preserve record order within each
child. Leaves never merge automatically.

All local operations use the persistent sibling guard and the v1 byte-range
protocol. A process-wide manager shares one descriptor per guard identity.
Shard writes hold one prefix slot, so other shards remain available. A reader
can use a fresh immutable checkout without creating a guard. It retries if a
writer creates the guard during that read.

Publication uses `.NAME.tmp` and `.NAME.old`. Readers can read a backup when the
live node is absent. `Recover(ctx)` restores or cleans interrupted work.
`Validate(ctx)` checks the full tree under an exclusive root lock. Neither
operation discards malformed authoritative data. On a split, the new directory
gives existing readers the traversal permission they need to reach its leaves.

Parents must already exist and be trusted. Parent and child resolution rejects
symbolic links and Windows reparse points. Tracked entries must be regular
files or directories. The
backend rejects child filesystem crossings and multiply linked guards or
recovery files. It refuses to change a multiply linked leaf. Configure separate,
non-overlapping roots; do not configure roots inside another store or its
reserved names. Stop operations before Git checkout, merge, or external edits.
See the scoped Git ignore and attribute rules in [the spec](spec.md#63-version-control).

`GetBatch` and `PutBatch` validate input and scan each selected leaf once.
Results remain in input order. Different leaves can succeed or fail separately.
A batch is not a transaction. A returned operation error can follow earlier
commits; check `errors.Is(err, locks.ErrIndeterminate)` and read the values again.
A rejected input batch performs no writes.

## HTTP

```go
handler, err := locks.NewHandler(store, locks.HandlerOptions{
    Authorize: authorize, // func(*http.Request, locks.Write) error
})
if err != nil {
    return err
}
mux.Handle("/team/lock", handler)

remote, err := locks.OpenHTTP("https://locks.example/team", locks.HTTPOptions{})
```

Mount the handler at the derived `/lock` path. The handler supports GET and both
POST forms, ordered results, strict framing, algorithm admission, and permission
checks before writes. `ReadOnly` rejects puts. `GETOnly` restricts the endpoint
to
GET. An empty `Write.Mode` in `Authorize` means a read. Without an authorization
callback, the handler permits all supported operations. Configure authentication,
TLS, server timeouts, connection limits, and storage quotas in the host service.
The server's durability is that of its underlying store.

The client uses GET for queries by default, including on GET-only servers.
`POSTQueries` keeps identifiers out of query URLs. Both forms send prehashed IDs;
raw keys can be hashed locally. The handler also accepts canonical raw tokens.
Redirects are disabled, including on a supplied client. The default client timeout
is 30 seconds. The client validates complete responses before returning results.
It splits rejected oversized batches without changing result order. It never
retries a write after an indeterminate response. A `replace` retry requires an
application policy because it can overwrite an intervening update.

Use `errors.Is` with `ErrFormat`, `ErrLimit`, `ErrDenied`, `ErrUnsupported`,
`ErrProtocol`, and `ErrIndeterminate`. `HTTPError` retains a rejected HTTP status.
`RemoteError` retains unknown well-formed peer error codes. A missing endpoint
is an error; a missing key is a successful read.

## Chains and discovery

```go
chain := locks.Chain{
    {Store: project},
    {Store: witness, CheckOnly: true, ReadOnly: true},
    {Store: remote},
}
report := chain.Get(ctx, id, locks.Verify)
```

`First` returns the first hit from a value source. `Verify` compares all hits,
including witnesses before the first source. A witness cannot supply a returned
value. A conflict takes priority over operational errors. Reports retain every
attempt, skipped position, and unattempted position.

`WriteFirst` stops at the first acknowledgement. `WriteAll` attempts each writable
position until a conflict. Earlier commits remain committed. Inspect the report
for partial success and indeterminate attempts. `WriteBack` takes a successful
source read and uses `Create`; replication is always explicit.

`ParsePath` parses `LOCK_PATH` with semicolon separators and quoted locations.
`check` controls the read role; `ro` controls writes. `ParsePathForOS` can parse
another platform's native path syntax. `Discover(project, os.LookupEnv)` uses an
explicit `LOCK_PATH` when set, including an empty string. Otherwise, the project
store precedes platform user and host defaults. Windows defaults use known-folder
APIs. Discovery supplies locations only; it does not authorize network access or
writes from untrusted project settings.

## Limits and tests

Zero `Limits` fields select defaults: 1 MiB per raw key or value, 2 MiB per line,
64 MiB per scanned leaf, 1,024 batch entries, 16 MiB per HTTP body, and 16 KiB per
request target. Negative or excessively large budgets are rejected. Comment
bytes count toward leaf scan budgets. HTTP body limits include bytes read after
transport decompression. Local locks have a default 30-second deadline. Contexts
can impose shorter deadlines. Local filesystem calls still depend on the host
filesystem; a context cannot interrupt a blocked kernel filesystem call.

The implementation buffers one bounded leaf or HTTP body. Configure filesystem
or tenant quotas for total stored data. A handler cannot infer those quotas.

```sh
go test ./...
go test -race ./...
go vet ./...
scripts/coverage.sh
FUZZTIME=30s scripts/fuzz.sh
```

The coverage script requires at least 95% overall statement coverage and exactly
100% statement coverage for all parsing and validation functions from fuzz seeds.
The fuzz script runs every fuzz target. These measures describe executed code;
they do not prove correctness for every possible input.

Tests include spec byte vectors, malformed inputs, HTTP rejection before writes,
response truncation and reordering, shard thresholds and terminal leaves,
independent shard locks, process races, killed publishers, metadata preservation,
and injected filesystem failures at publication and recovery boundaries. CI also
runs the local store suite with Go 1.22 and current Go on Linux, Windows and
macOS, and cross-compiles it for OpenBSD.
