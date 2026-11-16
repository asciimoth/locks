# locks

Locks is a shared store for values that tools need to keep consistent across
runs and machines, such as dependency versions, file checksums, or settings.
A tool records a value under a key, then reads or checks that value on later
runs. For example, a build tool can record a dependency's checksum and report
a conflict if it later receives different content for that dependency.

This project provides the `locks` command-line tool and a Go library. Stores
can be local files, shared through Git, or served over HTTP. The text format
works with Git and grows from one file to a tree of small files. It can handle
hundreds of thousands of entries without loading or scanning the whole database
for a lookup. A local lookup reads only one leaf, the record file selected by
the key hash.

The [Locks v1 specification](spec.md) defines the format, operations, local
locking and recovery rules, HTTP protocol, and store discovery.

## Command-line tool

### Installation

Install the command-line tool with Nix:

```sh
nix profile add github:asciimoth/locks#locks
```

In a NixOS flake, add `inputs.locks.url = "github:asciimoth/locks";` and add
`inputs.locks.packages.${pkgs.system}.locks` to `environment.systemPackages`.

To build the tool from a source checkout, use Go 1.22 or later:

```sh
go build -o locks ./cmd
```

Put the `locks` binary in a directory on your `PATH` . Run `locks --help` for
all commands and options.

### Read, write, and check values

```sh
locks write org.example/theme blue
locks read org.example/theme
locks check org.example/theme blue
```

The first command records `blue` and reports `created` . If you repeat the
write, the tool reports `same` . If you write a different value, the tool
reports `replaced` and updates the stored value. Use `--create` to keep an
existing value and report a conflict:

```sh
locks write --create org.example/theme green
```

| Command | Behavior |
| --- | --- |
| `read KEY` | Print the first value, followed by LF. |
| `write KEY VALUE` | Create or replace a value and report the result. |
| `check KEY [VALUE]` | Compare all stores and an optional expected value. |
| `doctor` | Validate local trees and report sizes and pending recovery. |
| `fix` | Recover and split; add `--merge` to merge small subtrees. |
| `serve` | Serve one explicit local store at `/lock` over HTTP. |
| `git` | Add scoped ignore and LF attribute rules in each selected worktree. |

### Store selection and options

Without `--path` , the tool uses `LOCK_PATH` when set. An empty `LOCK_PATH`
disables all stores. If `LOCK_PATH` is not set, the tool uses the project,
user, and host defaults. The project root is the Git worktree root. Outside
Git, the project root is the current directory. Use `--project DIR` to select a
project root.

Repeat `--path PATH` to select an ordered chain of individual paths or HTTP(S)
base URLs. These paths replace environment discovery. Relative paths use the
current directory. `LOCK_PATH` supports the `check` and `ro` flags.

Keys and values are literal command arguments. Empty strings are valid. The key
hash defaults to `blake2` . Use `--algorithm sha256` to select SHA-256. Use
`--id` for a canonical prehashed identifier. Options can appear before or after
arguments. Use `--` before keys or values that start with `-` .

Use `read --verify` to compare all stores. `check` also compares all stores,
but fails if any store returns an error. A copy can be missing if an eligible
source supplies a value. A check-only store cannot supply that value.

`write --create` keeps an existing different value and reports a conflict.
`write --all` visits all writable stores unless a create conflict stops the
operation. Reads and writes use the library's fallback policy. Store errors
always go to stderr, even when another store succeeds. A successful write can
be partial. Completed writes stay in place. An indeterminate error means a
write may have committed.

Use `-v` or `--verbose` to log discovery, policies, operations, and store
results to stderr. Values are not included in logs. `--timeout 30s` sets the
total operation deadline. The tool returns these exit codes:

- `0`: success.
- `1`: a missing key, a conflict, or a reported maintenance need.
- `2`: a usage or operation error.

### Maintenance and Git setup

```sh
locks --verbose doctor
locks --path .lock/locks fix
locks --path .lock/locks fix --merge
locks git
```

`doctor` reports the leaf count, record count, largest leaf, leaves that need a
split, and pending recovery sidecars.

`fix` validates the full tree before repair. It preserves malformed or
ambiguous data and reports an error. It does not choose between values in
different stores or move misplaced records.

Add `--merge` to merge subtrees with at most 512 records into one leaf,
including the root. Merging is off by default. The combined records must fit
the leaf scan limit. Larger subtrees stay split. Merges discard comments and
other ignored lines. Merges visit child names in sorted order and preserve
record order within each leaf.

A merge requires matching ownership, permissions, ACLs, and copied extended
attributes. Directories must have the required traversal rights. If metadata
differs or leaves have hard links, the merge returns an error. Empty
directories become empty leaves with their existing metadata when that metadata
can apply to a file. A leaf with more than 512 records at depth 52 needs no
split.

Both `doctor` and `fix` acquire the exclusive root guard. They can create the
guard if it is missing. Inspection preserves data and sidecars. Repair commits
each split or merge separately. An error can occur after other repairs have
completed.

For stores selected through environment discovery, maintenance skips HTTP
stores. The HTTP protocol has no operation to maintain a full tree. `fix` also
skips read-only positions. The tool reports these skips. Explicit HTTP paths
for maintenance are errors. Missing or inaccessible local parent directories
cause errors. Maintenance does not create parent directories.

`git` skips environment paths outside Git worktrees and remote stores. For
explicit paths, these cases are errors. Git rules cover the selected root and
its shards and sidecars. Existing rules and comments stay in place.

### Serve a store over HTTP

Host a local store over HTTP:

```sh
locks serve --path .lock/locks --listen 127.0.0.1:8080
locks --path http://127.0.0.1:8080 write org.example/theme blue
```

`serve` requires exactly one local `--path` and does not use discovery. The
default listen address is `127.0.0.1:8080` . Use port `0` to select a free
port. The server prints its base URL to stdout when the listener opens. Add
`--read-only` to reject writes. Missing parent directories are created on
writes, as with the client. The endpoint uses the Locks v1 handler and the
local store's durability rules.

Add `--append` to permit writes only for keys that do not exist. Both `create`
and `replace` requests can create a new key. A write to an existing key returns
a per-key `denied` error, even when the value is the same. The check and write
use the local store lock for that key.

Add `--no-parallel` only when this server is the only client that accesses the
store on disk. This option omits OS file locks. It keeps in-process locks, so
HTTP clients can still send requests at the same time. The server still uses
the guard file and recovery rules. Stop the server before another local client
or server accesses the same store. File locks remain enabled by default.

`--timeout` limits each request, connection reads and writes, idle connections,
and the time allowed for shutdown. It does not limit the server lifetime.
Ctrl-C or SIGTERM stops the listener. Shutdown waits up to `--timeout` for
active requests, then closes their connections.

The server provides plain HTTP without authentication. For remote access,
configure an authenticated HTTPS reverse proxy.

## Library usage

Add the library to your Go module. Go 1.22 or later is required.

```sh
go get github.com/asciimoth/locks
```

### High-level API

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

Import `github.com/asciimoth/locks` and `fmt` . Supply a `context.Context` as
`ctx` . The client hashes the exact key bytes with BLAKE2b-256. `Create` keeps
an existing value and reports `Conflict` if it differs. Use `Replace` to allow
an overwrite. Use `Put(ctx, key, value, mode)` to select the mode at run time.
`Get` distinguishes `Found` with an empty value from `Missing` .

To check every store or write to each writable store in the chain, select a
policy for the call:

```go
checked := client.GetWithPolicy(ctx, key, locks.Verify)
written := client.PutWithPolicy(ctx, key, "blue", locks.Create, locks.WriteAll)
```

`Verify` reads every location, including check-only locations, and reports
`Conflict` when values differ. `WriteAll` attempts to write to every writable
location unless a `Create` finds a conflict. Check `written.Attempts` to see
which stores accepted the value. `Get` and `Put` use the client's configured
defaults. `Create` and `Replace` are shortcuts for `Put` with the same default
write policy. The read policy (`First` or `Verify` ) is separate from the write
policy (`WriteFirst` or `WriteAll` ) and from the write mode (`Create` or
`Replace` ).

`Open` arguments are individual local paths or HTTP(S) base URLs, in priority
order. Relative paths use the working directory at setup. Arguments precede
`LOCK_PATH` even when it is explicitly empty. An empty `LOCK_PATH` disables OS
defaults. `Open` does not infer a project path. Arguments do not use
`LOCK_PATH` flags or shell expansion. During setup, the client records the
environment and working directory and validates the configuration. Setup does
not read or write stores. The client needs no `Close` call.

By default, reads return the first value found. Writes stop at the first
acknowledgement or confirmed conflict. After a backend error, the client can
try the next store. Each operation returns a `Report` . Inspect `Attempts` for
errors at earlier stores. `Locations()` gives the matching location order.

A valid write creates missing local parent directories with restricted
permissions. Existing permissions stay unchanged. Reads and read-only stores do
not create parent directories. A missing parent is a backend error. Local paths
must have trusted owners and contain no links. The client does not elevate
privileges.

Use `ClientOptions` to configure witnesses, verification, replication, limits,
or a custom HTTP client:

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

`Locations: []locks.Location{}` disables discovery. A nonempty `Locations`
slice replaces discovery and requires absolute paths or HTTP(S) base URLs.
`Prepend` still comes first. The client removes duplicate locations only when
both the address and flags match. Use `LookupEnv` to supply a custom
environment source. `ExistingParents: true` disables parent creation. `Local`
and `HTTP` configure backend options. `RawKeyBytes` limits key size before
hashing. Use `Algorithm` to select `sha256` .

With `Verify`, a read can return `Conflict`. With `WriteAll`, success can be
partial. Inspect `Attempts` when every store must answer or accept a write.
`client.Chain()` supplies a copy of the positions for prehashed IDs and explicit
write-back. The client does not replicate on reads.

### Lower-level features

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

Create the parent directory before opening the store. A missing root is an
empty store. `OpenLocal` checks configuration and does not create files.
Operations report parent access errors. A `Local` holds no file handles between
operations and needs no `Close` call. Its configuration is fixed after
construction.

`Get` returns `Found` with a value or `Missing` . An empty value is valid.
`Put` returns `Created` , `Same` , `Replaced` , or `Conflict` . A conflict
keeps the stored value and does not return an operational error. `Replace`
allows an overwrite. `Create` records a first observation. Values must be UTF-8
with no NUL or LF. Every boundary removes trailing CR bytes. Other bytes stay
unchanged.

Use `ParseID` to validate an external identifier. `Hash` hashes the exact
supplied bytes. It does not normalize paths, Unicode, URLs, or line endings. An
empty algorithm name selects `blake2` . If applications accept untrusted raw
keys, they must limit input size before calling `Hash` .

`Memory` is an in-process store. Its zero value is ready for concurrent use.

#### Local storage and recovery

`OpenLocal` supports Linux, macOS, OpenBSD and Windows. Use an absolute native
path on a filesystem with coherent byte-range locks and atomic same-directory
renames. Stores use the same tracked format and support recovery after a
process crash on all four platforms. Recovery after power loss requires
filesystem guarantees beyond this protocol. Other targets return
`ErrUnsupported` from `OpenLocal` . The HTTP, memory, format and chain APIs are
portable.

| Platform | Guard locking | Access controls on writes |
| --- | --- | --- |
| Linux | `fcntl` record locks | Ownership, modes, xattrs and POSIX ACLs |
| macOS | `fcntl` record locks | Ownership, modes, xattrs and native ACLs |
| OpenBSD | `fcntl` record locks | Ownership and permission bits |
| Windows | `LockFileEx` and `UnlockFileEx` | Owner, group and DACL |

Windows access controls require a filesystem with persistent ACLs, such as
NTFS. On Windows, new leaves use `FileMode` to grant rights to the owner,
primary group and everyone. During publication and recovery, the store retries
sharing violations from external handles within the operation's lock timeout.
On OpenBSD, use a filesystem that represents access through Unix ownership and
modes. On shared storage, each client must see the locks and renames made by
other clients.

The root starts as one file. A changed leaf with more than 512 records splits by
Base32 prefix. Splits stop at 52 digest characters. Ordinary edits preserve
ignored lines and record positions. Splits preserve record order within each
child. Leaves never merge automatically.

By default, local operations use the persistent sibling guard and the v1
byte-range protocol. A process-wide manager shares one descriptor per guard
identity. Each shard write holds one prefix slot. Other shards remain
available. A reader can use a fresh immutable checkout without creating a
guard. It retries if a writer creates the guard during that read.

`LocalOptions.NoParallel` omits OS file locks when no other client accesses the
store on disk. It keeps the same in-process coordination and guard file.

Publication uses `.NAME.tmp` and `.NAME.old` . Readers can read a backup when
the live node is missing. `Recover(ctx)` restores or cleans interrupted work.
`Validate(ctx)` checks the full tree under an exclusive root lock.
`Inspect(ctx)` also returns a `Health` report with leaf and record counts, the
largest leaf, overfull nonterminal leaves, and recovery sidecar counts.
`Rebalance(ctx)` validates the full tree, recovers pending work, and splits
overfull nonterminal leaves under the same root lock.
`RebalanceWithOptions(ctx, RebalanceOptions{Merge: true})` also merges small
subtrees. Each merge uses the existing sidecars and recovery rules. The merge
stages a combined leaf in `.NAME.old` . It moves the original directory to
`.NAME.tmp` before it moves the combined leaf into place. Readers can use the
combined backup if the process stops between the renames.

These operations preserve malformed authoritative data. After a split, the new
directory gives existing readers the traversal permission they need to reach
its leaves.

Parent directories must already exist and be trusted. When the backend resolves
parent and child paths, it rejects symbolic links and Windows reparse points.
Tracked entries must be regular files or directories. The backend rejects child
filesystem crossings and multiply linked guards or recovery files. It refuses
to change a multiply linked leaf. Configure separate, non-overlapping roots. Do
not configure a root inside another store or its reserved names. Stop
operations before Git checkout, merge, or external edits. See the scoped Git
ignore and attribute rules in [the spec](spec.md#63-version-control).

`GetBatch` and `PutBatch` validate input and scan each selected leaf once.
Results remain in input order. Different leaves can succeed or fail separately.
A batch is not a transaction. An operation can return an error after earlier
writes have committed. Check `errors.Is(err, locks.ErrIndeterminate)` and read
the values again. A rejected input batch performs no writes. Local writes check
each canonical record against the line and leaf scan limits before creating
parents or a guard. Limits on the complete changed leaf can still cause a
per-leaf failure.

#### HTTP

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

Mount the handler at the derived `/lock` path. The handler supports GET and
both POST forms, ordered results, strict framing, algorithm admission, and
permission checks before writes. `ReadOnly` rejects puts. `GETOnly` restricts
the endpoint to GET. An empty `Write.Mode` in `Authorize` means a read. Without
an authorization callback, the handler permits all supported operations.
Configure authentication, TLS, server timeouts, connection limits, and storage
quotas in the host service. The server provides the same durability as its
underlying store.

The client uses GET for queries by default, including on GET-only servers.
`POSTQueries` keeps identifiers out of query URLs. Both forms send prehashed
IDs; raw keys can be hashed locally. The handler also accepts canonical raw
tokens. The client disables redirects, including on a supplied HTTP client. The
default client timeout is 30 seconds. The client validates complete responses
before returning results. It splits rejected oversized batches without changing
result order. It never retries a write after an indeterminate response. A
`replace` retry requires an application policy because it can overwrite an
update made after the first attempt. The client rejects POST items that exceed
its line or single-item body limit before sending any part of the batch.
Request and response encoding enforce body limits before adding records to
their buffers. GET request encoding checks the target limit before adding each
key.

Use `errors.Is` with `ErrFormat` , `ErrLimit` , `ErrDenied` , `ErrUnsupported`
, `ErrProtocol` , and `ErrIndeterminate` . `HTTPError` retains a rejected HTTP
status. `RemoteError` retains unknown well-formed peer error codes. A missing
endpoint is an error. A missing key is a successful read.

#### Chains and discovery

```go
chain := locks.Chain{
    {Store: project},
    {Store: witness, CheckOnly: true, ReadOnly: true},
    {Store: remote},
}
report := chain.Get(ctx, id, locks.Verify)
```

`First` returns the first value found in a value source. `Verify` compares all
values found, including witnesses before the first source. A witness cannot
supply a returned value. A conflict takes priority over operational errors.
Reports retain every attempt, skipped position, and unattempted position.

`WriteFirst` stops at the first acknowledgement. `WriteAll` attempts to write
to each writable position until a conflict. Completed writes stay in place.
Inspect the report for partial success and indeterminate attempts. `WriteBack`
uses `Create` to copy the value from a successful source read. Replication
requires an explicit call.

`ParsePath` parses `LOCK_PATH` with semicolon separators and quoted locations.
`check` controls the read role. `ro` controls writes. `ParsePathForOS` can
parse another platform's native path syntax. `Discover(project, os.LookupEnv)`
uses an explicit `LOCK_PATH` when set, including an empty string. Otherwise,
the project store precedes platform user and host defaults. Windows defaults
use known-folder APIs. Discovery supplies locations only; it does not authorize
network access or writes from untrusted project settings.

#### Limits and errors

Zero `Limits` fields select defaults: 1 MiB per raw key or value, 2 MiB per
line, 64 MiB per scanned leaf, 1,024 batch entries, 16 MiB per HTTP body, and
16 KiB per request target. The library rejects negative or excessively large
limits. Comment bytes count toward leaf scan limits. HTTP body limits include
bytes read after transport decompression. Local locks have a default 30-second
deadline. Contexts can impose shorter deadlines. Local filesystem calls still
depend on the host filesystem. A context cannot interrupt a blocked kernel
filesystem call.

The implementation buffers one bounded leaf or HTTP body. Configure filesystem
or tenant quotas for total stored data. The handler cannot infer those quotas.
Adjacent ignored leaf lines share one stored span. Wire decoding limits the
number of line entries before splitting an input body.

Nil contexts and nil parser readers or writers return `ErrFormat` , including
typed nil interfaces. `NewHandler` rejects a nil store. If a chain contains a
nil store, the chain records `ErrUnsupported` and continues to the next store.
Parsers reject invalid reader byte counts and return `io.ErrNoProgress` after
100 consecutive empty reads. Custom readers, writers, stores, and callbacks
must obey their Go interfaces. The library does not recover panics raised
inside caller code.

## How Locks works

### Keys, values, and consistency

Each record maps a hashed key to one text value. The default hash is
BLAKE2b-256. The library also supports SHA-256. Raw keys are not stored. Tools
that share a store should define a stable key schema with their own namespace
and version. The library example uses `org.example.editor/v1` , a NUL
separator, and the setting name `theme` .

Use create operations to record a first observation and detect later changes.
To update a value, use a replace operation. At one store, the existence check,
comparison, and write are atomic for each key. Store chains can compare copies
and write to several locations, but they do not provide a transaction across
stores. A recorded value establishes consistency with that observation. It does
not prove that the original value was correct.

### Small files and direct lookups

A local store starts as one readable text file. Each record contains the full
hashed identifier, a space, and the value. When a changed leaf exceeds 512
records, the store splits it by hash prefix. The first directory level uses two
Base32 characters. Each later level uses one more character.

For a digest that starts with `XXOY`, the possible leaf paths are `locks`,
`locks/XX`, `locks/XX/O`, and `locks/XX/O/Y`. A lookup follows that path and
reads and validates one leaf file. It does not need a global index, a scan of
the whole database, or a copy of the whole database in memory. An ordinary
update rewrites only the selected leaf. Batches scan each selected leaf once.

This layout supports hundreds of thousands of entries while each lookup reads
only a small part of the store. The [specification's scale
examples](spec.md#23-scale-and-resource-use) show leaf counts and sizes for
defined key sets. Actual leaf sizes depend on the hashes and value sizes.
Splits stop at 52 digest characters, so a terminal leaf can exceed 512 records.
Full validation and maintenance scan the whole tree.

### Git-friendly storage

The stored records are plain text with one record per line. Ordinary updates
preserve record positions and comments, so unrelated records keep their diff
context. Splits preserve record order within each child but discard comments
and other ignored lines. Track the root file or the complete shard tree in Git.

Run `locks git` to add the scoped ignore rules for local guard and recovery
files, plus the LF rules for tracked data. Commit the resulting `.gitignore`
and `.gitattributes` changes with the store. Stop store operations before Git
checkout, rebase, merge, or external edits. Recover pending work before staging
the store.

Git merges can still produce conflicting values, duplicate keys, or misplaced
records. Resolve these conflicts, then run `locks doctor` to validate the tree.
Do not use a Git union merge to choose values. The
[version-control specification](spec.md#63-version-control) defines the rules
for merging records, including file-to-directory changes.

### Specification

The [Locks v1 specification](spec.md) is the reference for compatible tools and
stores. It defines key identity, value encoding, prefix routing, concurrent
access, process-crash recovery, HTTP messages, store chains, and discovery.
It also includes byte vectors and conformance cases for implementations.
