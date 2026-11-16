# locks

A Go library and command-line tool for [Locks v1](spec.md).
Requires Go 1.22 or later.

## Command-line tool

Run `just run --help`, or build the tool with `go build -o locks ./cmd`.
`just run` passes all arguments to the tool. For example:

```sh
just run write org.example/theme blue
just run read org.example/theme
just run check org.example/theme blue
just run --path .lock/locks write --replace org.example/theme green
just run --verbose doctor
just run --path .lock/locks fix
just run --path .lock/locks fix --merge
just run git
```

| Command | Behavior |
| --- | --- |
| `read KEY` | Print the first value, followed by LF. |
| `write KEY VALUE` | Create a value; report `created`, `same`, or `conflict`. |
| `check KEY [VALUE]` | Compare all stores and an optional expected value. |
| `doctor` | Validate local trees and report sizes and pending recovery. |
| `fix` | Recover and split; add `--merge` to merge small subtrees. |
| `serve` | Serve one explicit local store at `/lock` over HTTP. |
| `git` | Add scoped ignore and LF attribute rules in each selected worktree. |

Without `--path`, the tool uses `LOCK_PATH` when set. An empty `LOCK_PATH`
disables all stores. Otherwise, it uses the project, user, and host defaults.
The project is the Git worktree root, or the current directory outside Git.
Use `--project DIR` to select a project root. Repeat `--path PATH` to select
an ordered chain of individual paths or HTTP(S) bases. These paths replace
environment discovery. Relative paths use the current directory. `LOCK_PATH`
retains its `check` and `ro` flags.

Keys and values are literal command arguments. Empty strings are valid. The
key hash defaults to `blake2`; `--algorithm sha256` selects SHA-256. Use `--id`
for a canonical prehashed identifier. Options can occur before or after
arguments. Use `--` before keys or values that start with `-`.

Use `read --verify` to compare all stores. `check` also compares all stores,
but fails if any store returns an error. A missing copy is permitted when an
eligible source supplies a value. A check-only store cannot supply that value.
`write --replace` permits an overwrite. `write --all` visits all writable stores
unless a create conflict stops the operation. Reads and writes retain the
library's fallback policy. Store errors always go to stderr, even when another
store succeeds. A successful write can be partial; earlier writes are not
rolled back. An indeterminate error means a write may have committed.

`doctor` reports leaf count, record count, largest leaf, leaves that need a
split, and pending recovery sidecars. `fix` validates the full tree before
repair. It preserves malformed or ambiguous data and reports an error. It does
not choose between values in different stores or move misplaced records.
Add `--merge` to merge subtrees with at most 512 records into one leaf, including
the root. Merging is off by default. The combined records must fit the leaf scan
limit; larger subtrees stay split. Merges discard comments and other ignored
lines. They visit child names in sorted order and retain record order within
each leaf. A merge requires matching ownership, permissions, ACLs, and copied
extended attributes, with the usual traversal rights on directories. Differing
metadata or hard-linked leaves cause an error. Empty directories become empty
leaves with their existing metadata when that metadata can apply to a file.
A leaf with more than 512 records at depth 52 needs no split.
Both commands take the existing exclusive root guard and can create a missing
guard. Inspection preserves data and sidecars. Repair commits each split or merge
separately; an error can follow earlier completed repairs.

Environment maintenance skips HTTP stores, whose protocol has no full-tree
maintenance operation. `fix` also skips read-only positions. The tool reports
these skips. Explicit HTTP paths for maintenance are errors. Missing or
inaccessible local parent directories are errors; maintenance does not create
parents. `git` skips environment paths outside Git worktrees and remote stores.
For explicit paths, these cases are errors. Git rules cover the selected root
and its shards and sidecars. Existing rules and comments stay in place.

Use `-v` or `--verbose` to log discovery, policies, operations, and store results
to stderr. Values are not included in logs. `--timeout 30s` sets the total
operation deadline. Exit status is 0 for success, 1 for a missing key, conflict,
or reported maintenance need, and 2 for usage or operation errors.

To host a store, run:

```sh
just run serve --path .lock/locks --listen 127.0.0.1:8080
just run --path http://127.0.0.1:8080 write org.example/theme blue
```

`serve` requires exactly one local `--path` and does not use discovery.
The default listen address is `127.0.0.1:8080`. Use port `0` to select a free
port. The server prints its base URL to stdout when the listener opens.
Add `--read-only` to reject writes. Missing parent directories are created on
writes, as with the client. The endpoint uses the existing Locks v1 handler
and the local store's durability rules.

Add `--append` to permit writes only for keys that do not exist. Both `create`
and `replace` requests can create a new key. A write to an existing key returns
a per-key `denied` error, even when the value is the same. The check and write
use the local store lock for that key.

`--timeout` limits each request and the shutdown wait, not the server lifetime.
Ctrl-C or SIGTERM stops the listener and allows active requests to finish.
The server provides plain HTTP without authentication. For remote access,
configure an authenticated HTTPS reverse proxy.

## Quick start

Open a client, then read or write with string keys. `Open()` uses `LOCK_PATH`
when set, or the current OS user and host paths. To put a project store first,
pass its path to `Open`:

```go
client, err := locks.Open(".lock/locks")
if err != nil {
    return err
}
key := "org.example.editor/v1\x00theme"
written := client.Create(ctx, key, "blue")
if written.Err != nil {
    return written.Err
}
if written.Result.Status == locks.Conflict {
    return fmt.Errorf("theme already has a different value")
}
read := client.Get(ctx, key)
if read.Err != nil {
    return read.Err
}
if read.Result.Status == locks.Found {
    fmt.Println(read.Result.Value) // blue
}
```

Import `github.com/asciimoth/locks` and `fmt`. Supply a `context.Context` as `ctx`.
The client hashes the exact key bytes with BLAKE2b-256. `Create` keeps an existing
value and reports `Conflict` if it differs. Use `Replace` to permit an overwrite,
or `Put(ctx, key, value, mode)` to select the mode at run time. `Get` distinguishes
`Found` with an empty value from `Missing`.

Choose a policy for one call when you need to check every store or write through
the chain:

```go
checked := client.GetWithPolicy(ctx, key, locks.Verify)
written := client.PutWithPolicy(ctx, key, "blue", locks.Create, locks.WriteAll)
```

`Verify` reads every location, including check-only locations, and reports
`Conflict` when values differ. `WriteAll` attempts every writable location
unless a `Create` finds a conflict. Check `written.Attempts` to see which stores
accepted the value. `Get` and `Put` use the client's configured defaults.
`Create` and `Replace` are shortcuts for `Put` with the same default write
policy. The read policy (`First` or `Verify`) is separate from the write policy
(`WriteFirst` or `WriteAll`) and from the write mode (`Create` or `Replace`).

`Open` arguments are individual local paths or HTTP(S) bases, in priority order.
Relative paths use the working directory at setup. Arguments precede `LOCK_PATH`
even when it is explicitly empty. An empty `LOCK_PATH` suppresses OS defaults.
No project path is inferred. Arguments do not use `LOCK_PATH` flags or shell
expansion. Setup captures the environment and working directory, validates
configuration, and performs no store I/O. The client needs no `Close` call.

Reads return the first hit. Writes stop at the first acknowledgement or confirmed
conflict. Backend errors permit fallback. Each operation returns a `Report`;
inspect `Attempts` for errors at earlier stores. `Locations()` gives the matching
location order. A valid write creates missing local parent directories with
restricted permissions. Existing permissions stay unchanged. Reads and read-only
stores do not create parents; a missing parent is a backend error. Local paths
must have trusted owners and contain no links. No privileges are elevated.

Use options for witnesses, verification, replication, limits, or a custom HTTP
client:

```go
client, err := locks.OpenWithOptions(locks.ClientOptions{
    Prepend: []locks.Location{
        {Address: ".lock/locks"},
        {Address: "https://witness.example/team", CheckOnly: true, ReadOnly: true},
    },
    ReadPolicy:  locks.Verify,
    WritePolicy: locks.WriteAll,
})
```

`Locations: []locks.Location{}` disables discovery. A nonempty `Locations` slice
replaces discovery and requires absolute paths or HTTP(S) bases. `Prepend` still
comes first. Only identical addresses with identical flags are removed.
`LookupEnv` permits a custom environment source. `ExistingParents: true` disables
parent creation. `Local` and `HTTP` configure backend options. `RawKeyBytes`
bounds keys before hashing; `Algorithm` can select `sha256`.

With `Verify`, a read can return `Conflict`. With `WriteAll`, success can be
partial. Inspect `Attempts` when every store must answer or accept a write.
`client.Chain()` supplies a copy of the positions for prehashed IDs and explicit
write-back. The client does not replicate on reads.

## Lower-level API

The library also includes canonical key hashing, leaf files, automatic shard
growth, local recovery, an HTTP client and server, ordered store chains, and
`LOCK_PATH` parsing. It preserves valid prehashed identifiers with unknown
algorithm names.

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
`Validate(ctx)` checks the full tree under an exclusive root lock.
`Inspect(ctx)` also returns a `Health` report with leaf and record counts,
the largest leaf, overfull nonterminal leaves, and recovery sidecar counts.
`Rebalance(ctx)` validates the full tree, recovers pending work, and splits
overfull nonterminal leaves under the same root lock.
`RebalanceWithOptions(ctx, RebalanceOptions{Merge: true})` also merges small
subtrees. Each merge uses the existing sidecars and recovery rules. A combined
leaf is staged in `.NAME.old`; the original directory moves to `.NAME.tmp`
before the combined leaf moves into place. Readers can use the combined backup
if the process stops between the renames.
These operations preserve malformed authoritative data. On a split, the new directory
gives existing readers the traversal permission they need to reach its leaves.

Parents must already exist and be trusted. Parent and child resolution rejects
symbolic links and Windows reparse points. Tracked entries must be regular
files or directories. The backend rejects child filesystem crossings and
multiply linked guards or recovery files. It refuses to change a multiply linked
leaf. Configure separate, non-overlapping roots. Do not configure a root inside
another store or its reserved names. Stop operations before Git checkout, merge,
or external edits.
See the scoped Git ignore and attribute rules in [the spec](spec.md#63-version-control).

`GetBatch` and `PutBatch` validate input and scan each selected leaf once.
Results remain in input order. Different leaves can succeed or fail separately.
A batch is not a transaction. A returned operation error can follow earlier
commits; check `errors.Is(err, locks.ErrIndeterminate)` and read the values again.
A rejected input batch performs no writes.
Local writes check each canonical record against the line and leaf scan limits
before creating parents or a guard. Limits on the complete changed leaf can
still cause a per-leaf failure.

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
to GET. An empty `Write.Mode` in `Authorize` means a read. Without an authorization
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
The client rejects POST items that exceed its line or single-item body limit
before sending any part of the batch. Request and response encoding enforce
body limits before adding records to their buffers.

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
Adjacent ignored leaf lines share one stored span. Wire decoding bounds the
number of line entries before splitting an input body.

Nil contexts and nil parser readers or writers return `ErrFormat`, including
typed nil interfaces. A nil store is invalid in `NewHandler`; a chain records
`ErrUnsupported` and continues to the next store. Parsers reject invalid reader
byte counts and return `io.ErrNoProgress` after 100 consecutive empty reads.
Custom readers, writers, stores, and callbacks must obey their Go interfaces;
the library does not recover panics raised inside caller code.

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
macOS, and runs it in an OpenBSD VM.
