# Locks v1

Locks specification, 14 November 2026

A lock store maps a hashed key to one text value. Several tools can share the
same store. The store can be in a Git worktree, a machine-wide directory, or
behind an HTTP endpoint. A small local store is one readable file. A larger
store divides the same records by digest prefix. A point lookup then reads one
leaf.

The words MUST, SHOULD, and MAY express requirements, recommendations, and
options. Sections 1–7 define v1. Section 8 gives conformance cases. Section 9
lists implementation precedents and references.

The root is the configured local path, usually named `locks`. A leaf is a
regular file that contains records. A guard is a separate local file that
coordinates processes. A local root is configured as v1 and has no format
header. To use an incompatible future format, explicitly select a root or
migrate the existing root.

Reject NUL bytes in configured local paths before path cleanup or resolution.

## 1. Keys and values

### 1.1 Key identity

A raw key is any sequence of bytes, including an empty sequence. To compute an
identifier, hash those exact bytes. Encode the digest in RFC 4648 Base32. Then
append the ASCII colon and the algorithm name. Use uppercase `A–Z` and `2–7`,
with the most significant bits first. Set unused bits to zero and omit all `=`
padding. The algorithm name is part of identity. Callers that supply an
identifier have already completed these steps.

- `blake2`

  Definition: unkeyed, sequential BLAKE2b with digest length 32 bytes. Fanout
  and depth are 1. All other optional parameters, including salt and
  personalization, are zero. Set the output length before hashing. Truncating
  a 64-byte hash produces a different identifier.

  Raw-key support: required

- `sha256`

  Definition: the 32-byte SHA-256 digest defined by FIPS 180-4.

  Raw-key support: optional

An identifier contains exactly one colon. The algorithm name matches
`[a-z][a-z0-9-]{0,31}`. The digest MUST decode to 32–64 whole bytes and
re-encode to exactly the supplied spelling. Enforce the defined digest length
for known algorithms. A 32-byte digest has 52 Base32 characters. The last
character is `A` or `Q`. Reject aliases, case folding, nonzero pad bits, and
padded encodings.

A new algorithm needs a distinct name. Publish its parameters, digest length
and test vectors. Writers MUST preserve structurally valid identifiers with
unfamiliar names when copying, editing or splitting a leaf. A store can look
up such a prehashed identifier without implementing the hash. A deployment MAY
restrict accepted algorithms at its API boundary. If a raw-key algorithm is
unsupported, return an explicit error. Changing algorithms creates a new
identity. To migrate keys, an application must obtain the original bytes and
compute the new identifier. The application must establish the relationship
between the old and new identities.

Tools sharing a store SHOULD publish a stable raw-key schema with a namespace
they control and a schema version. Specify the encoding and framing of every
field. For example, a schema can define the UTF-8 bytes of
`org.example.editor/v1`, one zero byte, then the UTF-8 bytes of one setting
name. With multiple arbitrary byte fields, use lengths or equivalent unambiguous
framing. Store implementations MUST NOT normalize Unicode, paths, URLs, or line
endings in raw keys.

### 1.2 Values and operations

A value is valid UTF-8 text. A value must not contain LF (`0A`) or NUL (`00`).
Remove all trailing CR bytes (`0D`) at every boundary, including API input,
file decoding and comparison. Repeated line-ending conversions then give the
same normalized value. Preserve interior CR bytes, tabs, leading or trailing
spaces, and Unicode spelling byte for byte. The empty value is valid and
distinct from a missing key.

A read returns `found` with the stored value, or `missing`. For a write,
compare the proposed and stored canonical values as follows.

- `create`

  Absent: insert the value and return `created`.

  Present with equal canonical value: keep the stored value and return `same`.

  Present with different canonical value: keep the stored value and return
  `conflict`.

- `replace`

  Absent: insert the value and return `created`.

  Present with equal canonical value: keep the stored value and return `same`.

  Present with different canonical value: update the value and return
  `replaced`.

Each store MUST make the existence test, comparison and update one indivisible
operation for that key. A successful read or write takes effect at a single
instant between the time the store receives the operation and returns its
result. With concurrent `replace` calls, the last serialized update wins. Use
`create` for first observations. Two concurrent callers with different values
cannot both create the entry at the same store. A batch has per-key outcomes.
A chain has per-store outcomes. Neither provides a transaction across its
members.

## 2. Tracked representation

### 2.1 Leaf records

Read a leaf as UTF-8 in binary mode. Split on LF only. Readers MUST accept LF,
CRLF, and a final unterminated line. Writers write LF after every line they
generate and write no byte-order mark. A UTF-8 BOM at offset zero, invalid
UTF-8, or NUL anywhere is a format error. After removing trailing CR bytes
from each line, apply the following rules in order.

1. Reject unresolved Git conflict markers at column one. A run of seven or more
   identical `<`, `|`, `=`, or `>` bytes followed by a space or end of line is
   reserved. This rule detects ordinary, diff3 and longer configured markers.

2. Ignore an empty line or a line whose first byte is outside the Base32
   alphabet. For example, `# comment` is ignored. Readers also ignore lines with
   leading whitespace. There are no inline comments.

3. On a line beginning with a Base32 byte, split at the first ASCII space
   (`20`). Parse the entire left side as an identifier. The right side is the
   value, including any additional spaces. A tab cannot replace the separator. A
   missing separator or malformed identifier is a format error.

Two records with the same identifier in one leaf are a format error, even if
their values agree. A record in a leaf whose path does not match its digest
prefix is also a format error. A point read MUST validate the entire selected
leaf before returning a hit or miss. A writer MUST validate the leaf it will
change. A full validator checks all reachable leaves and the directory
structure. Report a malformed store as an error, never as an empty map.

```text
# Leading and embedded spaces in a value are significant.
XXOYCPDDII4XEMLR54765GCXTOKJMTR3WHFT4QTSMLEMA2GVEMMQ:blake2 blue
FYXQGQ4JSJ7PU5XKXCEGMLN66EB6X6I2XRHRLVOBCOS2HNWW55FQ:blake2  two words
```

### 2.2 Prefix routing and growth

Start with a single leaf at the root. Count distinct records, excluding ignored
lines. A leaf can hold 512 records. Inserting a 513th distinct key splits the
leaf. The first directory level uses two Base32 characters and has up to 1,024
children. Each deeper level uses one more character and has up to 32 children
per split. This reduces the number of small tracked files after the first level
fills. Keep the full identifier in each record. The suffix never contributes to
a path.

For a digest starting with `XXOY`, the successive possible leaf paths are
`locks`, `locks/XX`, `locks/XX/O`, and `locks/XX/O/Y`. At each step, scan a
file as a leaf. For a directory, use the next path component. A missing child
means that the key is absent, subject to the recovery rules in §3. On every
filesystem, verify the actual spelling of each selected shard component and
its recovery sidecars. Reject case aliases, including an alias beside the
exact name. A point lookup MAY read the entries of directories along its path.
It MUST NOT require enumeration of the full store.

Create only nonempty children. Append new records to the leaf. An overwrite
replaces the original record at its position. Preserve unaffected records and
ignored lines when editing an unsplit leaf. If the last line has no
terminating LF, add the separator LF before appending a record. Splitting
discards ignored lines and preserves the relative order of records routed to
each child. Recursively split overfull children before publication.
Implementations MUST NOT automatically merge small leaves back together. An
explicit maintenance request MAY merge a subtree with at most 512 records into
one leaf, subject to resource limits and the merge procedure in §3.2.

Stop splitting after using 52 digest characters, even for longer digests. An
overfull terminal leaf is valid and is scanned normally. This limits path depth
and handles different identifiers with the same routing prefix. Readers MUST
also accept a valid overfull nonterminal leaf, for example after a Git merge.
The next write that changes that leaf MUST split it. An unchanged `same` result
requires no rewrite.

Directory entries in the tracked tree have exactly the specified uppercase
spelling. The root and every tracked child must be regular files or
directories. Local sidecars are defined in §3. A validator reports other
entries, case aliases, links, and directories beyond the depth limit. Writers
MUST leave unrecognized files untouched. An empty store has an empty root file
or an absent root in an accessible parent. Parent access errors are backend
errors.

### 2.3 Scale and resource use

A lookup requires one path traversal and one leaf scan. An ordinary edit
rewrites only that leaf. Group batch requests by prefix to avoid opening and
scanning a leaf once per key. Readers can stream values and keep the set of
identifiers in the target leaf to detect duplicates. Splitters can spool
records to staged children and cap the number of simultaneous file handles. A
point lookup requires no global index, sort, or full-store scan.

The table shows how record counts affect the number and size of tracked leaves.
The input for each row is the integers from zero through *n*−1, encoded as
unsigned 8-byte big-endian raw keys and hashed with `blake2`.

| Records | Tracked leaves | First-level shards split again | Largest leaf |
| ---: | ---: | ---: | ---: |
| 100,000 | 1,024 | 0 | 135 |
| 500,000 | 5,612 | 148 | 512 |
| 1,000,000 | 32,768 | 1,024 | 53 |

The record-count threshold does not limit value length or ignored bytes. Large
input and deliberately chosen prehashed prefixes need explicit resource
budgets (§7). If an operation exceeds a budget, return a limit error.

The Go local store checks that each proposed canonical record fits its line
and leaf scan limits before it accesses the store. The scan limit includes the
final LF. A batch with an oversized record performs no writes. Staging also
checks the complete changed leaf; existing records and ignored lines can cause
a per-leaf limit error after other leaves in the batch have committed.

## 3. Local coordination, publication and recovery

The local protocol protects cooperating readers and writers, including
implementations in different languages. All participants that access a live
store MUST use the same guard and locking family. The protocol requires a
filesystem with coherent byte-range locks and atomic same-directory renames.
The protocol covers process termination. It does not guarantee recovery after
a system crash or power loss.

An explicitly configured single-client mode MAY omit OS file locks when no
other client accesses the store on disk. It MUST still coordinate concurrent
operations within that client and follow the same publication and recovery
rules. The Go option `LocalOptions.NoParallel` selects this mode. It retains
the persistent guard and in-process shared/exclusive ownership. File locks
remain enabled by default. Stop the client before another local client accesses
the store; this mode does not detect access from another process.

### 3.1 One persistent guard

For root `P/N`, the guard is `P/.N.guard`. For example, `.lock/locks` has the
sibling `.lock/.locks.guard`, alongside either form of the root. A root must
have a parent and basename below the volume or share root. Resolve the parent
securely and ignore trailing path separators when you determine the basename.
Configured roots must not overlap another store's tree or reserved sidecar
names, including filesystem aliases.

If the guard is absent, create it atomically. Open the guard without
truncation and keep its identity stable. An empty regular file is sufficient.
Take OS locks on one-byte ranges from the start of the file. The contents of
these bytes do not matter. Never unlink, rename, replace, or remove the guard
because of its age while the store is live. After a process terminates, the
kernel releases its locks when the owning process or handles no longer exist.
A guard left on disk is normal.

Byte 0 protects the root structure. For a first-level prefix `ab`, its slot is
byte 1 + 32 × index(a) + index(b), where the indices follow
`ABCDEFGHIJKLMNOPQRSTUVWXYZ234567` starting at zero. All descendants of a
first-level prefix use that same slot.

| Operation | Byte 0 | Prefix slot |
| --- | --- | --- |
| Read an absent or unsplit root or its recoverable old leaf | Shared | None |
| Create, edit, split or recover the root itself | Exclusive | None |
| Read within a sharded root | Shared | Shared |
| Write or recover within one first-level shard | Shared | Exclusive |

Take byte 0 first, then at most one prefix slot. Hold the applicable locks
through validation and publication. Release the locks in reverse order before
contacting another store. A writer can inspect the root under a shared lock. If
the writer needs an exclusive root lock, it MUST release the shared lock,
acquire an exclusive lock, and read the state again. Never upgrade a live shared
lock in place. If a writer finds root sidecars, it first recovers them under an
exclusive lock on byte 0. After recovery of a sharded root, resume with the
normal slot protocol. Readers in different shards can proceed during a shard
write.

On POSIX use traditional `fcntl` record locks (`F_RDLCK` / `F_WRLCK`,
`F_SETLK` or `F_SETLKW`, `SEEK_SET`, length 1). Do not substitute `flock` for
these locks. Traditional record locks belong to a process. Each process MUST
coordinate its threads and libraries through one guard manager per file
identity, with local shared/exclusive ownership and reference counts. Closing
any descriptor for that guard in the process can drop its locks. Keep one
managed descriptor, avoid opening duplicate descriptors, and close it only
when the process has no active operations. Open read/write when writes are
intended, and open read-only for shared locks when necessary. Child processes
acquire their own locks after a fork.

On Windows use `LockFileEx` / `UnlockFileEx` on the same offsets, a
noninheritable handle, shared or exclusive as indicated, and compatible
read/write sharing on the guard. Allow each shared owner to finish before
releasing the underlying range. To enforce a finite deadline, use nonblocking
attempts or cancellable asynchronous locking.

A fresh Git checkout need not have a guard. A reader that observes an absent
guard MAY do one optimistic lookup, then check the guard again, including
before returning a miss or error. If the guard appeared, discard the result
and restart under the guard. If the guard is still absent and the reader
observed no transaction sidecars, accept the result. This works because every
writer establishes the permanent guard before its first change. Only a
definite name-not-found result counts as guard absence. An existing,
unreadable guard or sidecars without a guard produce a backend error. A
read-only client can use an existing guard with shared locks. Administrators
SHOULD grant data readers read access to the guard and any recovery backup.

A consistent full-store validation or snapshot needs an exclusive lock on byte
0, or a copy that no process is changing. An auditor can validate a live tree
one shard at a time. This validates the leaves at different points in time.

### 3.2 Commit protocol

For each changed node named `N`, reserve two sibling names in the same parent
directory. `.N.tmp` stages its replacement and `.N.old` holds the previous leaf.
These names apply to both the root and deeper nodes. All staging, publication
and recovery for a node take place under the exclusive range applicable to that
node. An offline maintenance tool may take byte 0 exclusively for the whole
operation.

1. Recover interrupted work on the target path as specified below. Reread and
   validate the current leaf and apply the per-key condition while locked.
   Refuse to change a leaf with multiple hard links. Stage the entire new leaf
   or split subtree in `.N.tmp`, creating it exclusively. Preserve intended
   ownership and ACLs without broadening access, including when a file becomes a
   traversable directory. Close and check every output. Apply append and
   replacement semantics to the staged bytes only.

2. For a new node, rename the staged object to the absent `N`. For an existing
   regular file, a same-directory atomic file replacement MAY publish a staged
   file directly. For a split, or where direct atomic file replacement is
   unavailable, rename the old leaf from `N` to the absent `.N.old`, then rename
   the complete stage from `.N.tmp` to the absent `N`. A directory is never
   renamed over a regular file. The rename that installs the stage is the commit
   point.

3. After publication, remove the backup. On failure after the commit point,
   report an indeterminate outcome. A caller can read the value again to
   determine the stored state.

For an explicitly requested directory-to-leaf merge, hold byte 0 exclusively
through validation, publication, and cleanup. Recover existing sidecars first.
Validate all authoritative data before changing the tree. A combined leaf MUST
contain every record in the subtree with its canonical value. The combined
records must fit the leaf scan limit and the 512-record threshold. Refuse to
merge multiply linked leaves or to broaden access. The Go implementation
requires equal ownership, modes, ACLs, and copied extended attributes across
source leaves. Directory metadata must match the source leaf with the traversal
rights used for a split. It reports an error if these checks fail. Empty
directories can become empty leaves when their metadata can apply to a file.

Use the existing sidecars in this order:

1. Create `.N.old` exclusively and write the complete combined leaf there.
   Preserve source metadata. Close and check the output before proceeding.
   While `N` is still present, it remains authoritative; a partial `.N.old` is
   disposable recovery evidence.
2. Rename directory `N` to the absent `.N.tmp`. The complete `.N.old` now
   serves every record. This rename commits the change of representation. No
   value changes at this point.
3. Rename `.N.old` to the absent `N`, then remove `.N.tmp` through checked
   handles. Failures after step 2 have an indeterminate outcome. Ordinary
   recovery can complete the merge using the rules below.

This procedure never puts a directory in `.N.old`. It introduces no new
sidecar names or reader states. A crash before step 2 preserves the original
tree. A crash after step 2 preserves all records in the combined leaf. Merges
discard ignored lines, visit child names in sorted order, and retain relative
record order within each source leaf. Commit each merge separately.

Under the guard, recovery uses names and types, never timestamps, PIDs, or
whether a staged file appears complete.

- `N` present, with or without sidecars

  Reader action: read `N`

  Next exclusive recovery action: keep `N` and discard recognized stage and
  backup.

- `N` absent, `.N.old` present

  Reader action: read the old leaf from `.N.old`, using the prefix of `N`

  Next exclusive recovery action: restore `.N.old` to `N`, then discard the
  stage.

- `N` absent, no backup, stage present or absent

  Reader result: `missing`

  Next exclusive recovery action: discard any stage.

A backup must be a regular leaf. A stage may be incomplete. Before cleanup,
verify object types and containment without following links. If both live data
and a backup exist, validate the live leaf or the complete published subtree
for that node before discarding its backup. If authoritative data is
malformed, or a name or type makes recovery ambiguous, report corruption and
retain the evidence. Validate a present live node before discarding a stage,
including when no backup exists. Readers never publish or clean up a stage. A
read-only client can therefore read existing data after a writer terminates
between the two split renames. The same rules cover further interrupted
attempts at recovery.

The Go implementation limits an incomplete stage to 52 directory levels during
inspection. A deeper stage returns a limit error and remains unchanged. Stage
inspection and removal check the operation deadline between nodes.
Staging also checks the deadline between nodes. Writes and merges check it
again before the first publication rename. Cancellation before this check
leaves the live node unchanged; ordinary recovery can discard the stage.
Cancellation cannot interrupt a filesystem call already in progress.

### 3.3 Filesystem limits

Treat a close, lock or rename failure as an error.

On Windows use handle-based byte-range locks and supported same-volume rename
operations. External open handles can deny rename or delete. Retry sharing
violations within a set limit. If retries fail, return an error and preserve
recoverable data. Never fall back to copy-and-delete.

On NFS, SMB, FUSE, synchronized folders or mixed Windows/Unix access, enable
the live protocol only when locking, visibility and rename semantics
interoperate for every participating client. Otherwise use an immutable
checkout for reads or serve the store through one HTTP backend on supported
storage. Set finite lock and I/O deadlines. Respect host path limits,
including the full root path. If the host cannot represent a path, return an
error.

## 4. HTTP protocol

### 4.1 Endpoint and keys

A server base URL is an absolute `http` or `https` URL with a host. Append
`/lock` to the path after removing any trailing literal slashes. Keep existing
percent-encoded path segments and the base query unchanged. The client
discards the fragment before sending the request. For example, the base
`https://example.com/foo/bar/?tenant=west#note` gives the endpoint
`https://example.com/foo/bar/lock?tenant=west`. Fragments never reach an HTTP
server. Servers MUST ignore unknown query parameters. A base URL MUST NOT
supply the reserved query name `key`, including an equivalent percent-encoded
spelling. A base URL must not contain userinfo.

Both key forms below are ASCII tokens used in the GET query and in POST records.
Percent-encoding applies only inside a URL.

- `id.<identifier>`

  Meaning: use the canonical prehashed identifier.

  Example: `id.XXOYCPDDII4XEMLR54765GCXTOKJMTR3WHFT4QTSMLEMA2GVEMMQ:blake2`

- `raw.<algorithm>.<bytes>`

  Meaning: decode unpadded RFC 4648 Base64url into raw bytes. Hash those bytes
  with the named algorithm.

  Example: `raw.blake2.YWJj` for the three bytes of `abc`, or `raw.blake2.` for
  zero bytes

Use the Base64url alphabet `A–Z a–z 0–9 - _` and require canonical spelling,
including zero pad bits and no padding. In a query, separate fields with `&` and
percent-encode token bytes outside the URI unreserved alphabet. Parse field
names and values with one percent-decoding pass. A literal `+` is never a space
in this protocol. Reject malformed key tokens and malformed percent escapes in
recognized fields. Raw bytes are data to hash. A server MUST NOT dereference a
raw key as a URL or use it as a path.

### 4.2 Body framing

POST bodies, HTTP 200 responses and v1 application error bodies use
`Content-Type: application/octet-stream`. Send the media type without
parameters. Recipients compare its type and subtype case-insensitively. The
first line selects the protocol version and message kind. The remaining lines
carry one record each. All fixed words are case-sensitive ASCII. Values use
the same UTF-8 bytes as leaf records (§1.2).

A sender terminates every line with LF. A receiver splits at byte `0A`,
removes trailing `0D` bytes, and rejects an unterminated final line, NUL
anywhere, or invalid UTF-8. This accepts CRLF. Values have no escapes. Quotes
and backslashes are literal, and an interior CR remains unchanged. Wire bodies
have no BOM, comments, blank lines, or implicit fields.

In the forms below, `SP` means exactly one ASCII space (`20`). `KEY` is a
token from §4.1. `ID` is a canonical identifier from §1.1, without the `id.`
prefix. `VALUE` contains all remaining bytes of the line, including additional
spaces and tabs. The value may be empty. A receiver MUST consume the space
introducing `VALUE` even when the value is empty. Records that end at `KEY`,
`ID` or `CODE` permit no additional bytes.

### 4.3 Requests and limits

Every server offers GET with one or more `key` query parameters in order. GET
has no body. A writable server also accepts both POST forms below. A read-only
server may support GET alone or also accept POST queries. A POST request
carrying `key` query arguments is invalid.

<!-- The HTTP request line must remain on one line. -->
<!-- markdownlint-disable MD013 -->
```http
GET /foo/bar/lock?key=raw.blake2.YWJj&key=id.FYXQGQ4JSJ7PU5XKXCEGMLN66EB6X6I2XRHRLVOBCOS2HNWW55FQ%3Ablake2 HTTP/1.1
Host: example.com
```
<!-- markdownlint-enable MD013 -->

| POST first line | Shape of each following record | Operation |
| --- | --- | --- |
| `locks/1 get` | `KEY` | Look up the key. |
| `locks/1 put` | `create SP KEY SP VALUE` | Create if absent. |
| `locks/1 put` | `replace SP KEY SP VALUE` | Create or overwrite. |

The same GET batch can be sent as this POST body.

```text
locks/1 get
raw.blake2.YWJj
raw.blake2.AAr_
```

A write batch may mix modes. This body requests a create for the first key and a
replacement for the second.

```text
locks/1 put
create raw.blake2.YWJj blue
replace raw.blake2.AAr_ two words
```

Require at least one record. Reject an unknown verb, a malformed key or value,
and duplicate resolved identifiers, including a raw key and its prehashed
alias. Validate the complete request, accepted algorithms, operation
permissions and size limits before applying any writes. After validation, each
key succeeds, conflicts or fails independently. A server can validate records
as they arrive and keep them in a bounded buffer or spool until the server
receives the complete HTTP body.

Each server chooses an entry-count limit of at least 16 for every operation it
offers. With a short base URL, GET must accept 16 canonical `blake2` IDs.
Supported POST operations must accept 16 entries with raw keys of up to 256
bytes or canonical `blake2` IDs, and values of up to 1,024 UTF-8 bytes each.
Request-target and POST-body size limits must be at least 4,096 bytes and 128
KiB respectively. They may be larger. On GET-only servers a client can hash
large or sensitive raw keys locally and send their identifiers.

Return HTTP 413 if the record count, body size or per-item size exceeds its
limit. Return HTTP 414 if the request target exceeds its limit. On a count-limit
rejection, include the `too-many` error record from §4.4. Split a rejected batch
without reordering results. If a proxy rejects a multi-key request without a
usable limit, a client may try smaller batches. For a single oversized GET key,
use a locally computed identifier or a supported POST query. Return a limit
error if neither fits.

The Go HTTP client checks every POST item against its line and single-item
body limits before sending any part of a batch. It checks the encoded body
size before building each request buffer. The Go handler also limits its
response buffer before adding result records. The client checks the encoded
GET target size before adding each key. It splits an oversized batch without
first building the complete target. When splitting a rejected batch, the
client reduces each part to at most half the rejected size. This limits retry
depth even if a peer repeatedly advertises a limit one below the current
request size.

### 4.4 Responses, errors and retries

An HTTP 200 response starts with `locks/1 results` and contains exactly one
result per requested key, in request order. Each result echoes the resolved
`ID`. Its status must be valid for the requested operation and write mode.

| Result record | When valid |
| --- | --- |
| `found SP ID SP VALUE` | Read hit, including an empty value |
| `missing SP ID` | Read miss |
| `created SP ID` or `same SP ID` | Either write mode |
| `replaced SP ID` | `replace` mode only |
| `conflict SP ID` | `create` mode only |
| `error SP ID SP CODE` | Per-key operational error on a read or write |

For the example GET, a hit for the first key and a miss for the second give this
entire body.

```text
locks/1 results
found XXOYCPDDII4XEMLR54765GCXTOKJMTR3WHFT4QTSMLEMA2GVEMMQ:blake2 blue
missing FYXQGQ4JSJ7PU5XKXCEGMLN66EB6X6I2XRHRLVOBCOS2HNWW55FQ:blake2
```

The example write against that state returns the following body.

```text
locks/1 results
same XXOYCPDDII4XEMLR54765GCXTOKJMTR3WHFT4QTSMLEMA2GVEMMQ:blake2
created FYXQGQ4JSJ7PU5XKXCEGMLN66EB6X6I2XRHRLVOBCOS2HNWW55FQ:blake2
```

A client MUST check the media type, first line, record formats, count, IDs,
order, and completion of the HTTP body before returning any final result.
Unexpected statuses, extra records and truncated bodies are protocol errors. A
missing key is a successful read. HTTP 404 for the endpoint is a backend
error.

For a whole-request rejection, a v1 application uses the first line
`locks/1 error` followed by one error record. Usually that record is `CODE`.
Codes match `[a-z][a-z0-9-]{0,31}`. Common per-key codes include `denied`,
`unavailable`, `limit` and `indeterminate`. An unfamiliar well-formed code is
a generic error. The whole-request code `too-many` is reserved for a
count-limit rejection (HTTP 413). Its record is exactly `too-many SP MAX`.
`MAX` is the server's limit for this operation in canonical unsigned decimal,
with no leading zeroes and a value of at least 16. A client can cap the number
at its own batch limit while parsing to avoid integer overflow. For example, a
server that allows 16 entries answers a 17-entry batch with this body.

```text
locks/1 error
too-many 16
```

- 400 / 422

  Meaning: malformed request or duplicate key / unsupported algorithm or
  version. The server applies no entries.

- 401 / 403

  Meaning: authentication or authorization failure. The server rejects the
  request before execution. A POST-capable read-only server rejects a put with
  403.

- 405

  Meaning: unsupported method. Include `Allow`, for example `Allow: GET` on a
  GET-only server.

- 413 / 414 / 415

  Meaning: limit exceeded / target too long / unsupported content type. The
  server applies no entries.

- 429 / 5xx

  Meaning: throttled or temporarily unavailable. Follow `Retry-After` where
  applicable.

Intermediaries may return an empty or unrelated error body. Use the HTTP status
for classification and accept lock records only in a valid v1 body. A small C
implementation can scan a bounded slice with `memchr` for LF and the fixed field
separators, then pass the value as a `(pointer, length)` pair. HTTP transport
chunk boundaries can occur anywhere in a line or a UTF-8 character.

Responses MUST set `Cache-Control: no-store`. Do not accept a shared HTTP
cache as another lock in the chain. If a timeout, broken connection, invalid
response or server failure occurs after sending a write, treat the write as
possibly committed. Reading again and repeating `create` with the same value
is safe, including after a lost response. A retry of `replace` may overwrite a
value written after the original request. Clients MUST NOT retry an
indeterminate replace automatically without a policy that explicitly permits
this. The base protocol offers no compare-and-swap or deletion. A server
documents the durability of its acknowledgements. V1 does not negotiate remote
durability on the wire.

An HTTP response that neither acknowledges a write nor confirms rejection before
execution leaves the write indeterminate. This includes unexpected success
statuses such as 201 or 204, protocol switching (101), and request timeouts (408).
If a backend error includes an indeterminate write outcome, a handler MUST
preserve it with the `indeterminate` error code.
A client MUST retain an explicit `indeterminate` error even when the HTTP status
indicates a limit rejection. It MUST NOT split and retry that write automatically.

The Go handler logs response write failures through the HTTP server's error
logger, or the standard logger when no server logger is available. A partial
response cannot be replaced after its headers have been sent.
The handler rejects a GET with an unknown body length, including an HTTP/2
request without a declared length. GET requests must have no body.

## 5. Ordered chains

Each chain position has two independent properties. Its read role is either a
value source or check-only. A value source may supply the returned value. A
check-only position may confirm or conflict with a value supplied by a source.
Its write policy either permits a write attempt or marks the position
read-only. The store's own permissions still apply.

A store result for a different identifier is a backend protocol error for that
position. It cannot supply a read value or a write acknowledgement. Continue to
the next position under the normal fallback rules.

| `LOCK_PATH` prefix | Read role | Direct write or write-back allowed |
| --- | --- | --- |
| None | Value source | Yes |
| `ro=` | Value source | No |
| `check=` | Check-only | Yes |
| `check,ro=` | Check-only | No |

Choose the read and write policies independently per key. Store errors are
ordered by configured chain position, even if an implementation batches requests
for several keys. Continue past unavailable stores, permission denials, format
errors, timeouts and unsupported operations. Diagnostics MAY identify affected
positions. A confirmed value conflict is a semantic result and takes priority
over operational errors.

### 5.1 Read policy

- `first`

  Search and comparison: visit eligible value sources in order, skipping
  check-only positions. Return the first `found`.

- `verify`

  Search and comparison: visit every position. Keep the first value from an
  eligible source. Compare it with every other value found, including
  check-only observations from earlier positions. Any difference in canonical
  UTF-8 bytes returns `conflict` and no usable value.

If there is no eligible hit, return `missing` when any eligible source
returned `missing` without an error. If every eligible read failed, return the
last eligible error. With no eligible source, return `no-source`. A check-only
hit alone cannot turn a miss or an outage into success. When a value was
found, errors from other positions may be logged or exposed as diagnostics.
Agreement only covers the stores that answered. For immutable values,
applications SHOULD choose `verify` and may separately require successful
contact with named authorities or witnesses before accepting a value or
creating one from a miss. For a locally overridden setting, `first` preserves
precedence.

### 5.2 Write policy

Skip positions marked read-only. Attempt the remaining positions in chain
order, including check-only positions, subject to backend permissions. An
operational error permits trying the next position. Under `first`, return
after the first `created`, `replaced` or `same` result. Under `all`, attempt
every eligible store unless a conflict occurs and retain per-store outcomes. A
confirmed `create` conflict stops that key immediately under either policy.
Return it before attempting any lower-priority store with the competing value.

If at least one store acknowledged success and no conflict occurred, the chain
write succeeds. Under `all`, success can therefore be partial. If none
succeeded, return the last attempted operation's error, or `no-writable-store`
if all positions were skipped. A per-store report distinguishes skipped and
unattempted positions from operation results. An indeterminate attempt remains
indeterminate even when a later store acknowledges success. A later failure or
conflict never rolls back an earlier commit. Callers that need complete
replication MUST inspect the per-store results. Concurrent operations see
independent store snapshots, and can disagree even when every store serializes
its own writes.

A successful write does not change the destination's read role. For example,
with a writable check-only store A followed by an empty value source B, the
`first` write policy may create the key at A. A later read still returns
`missing` because B has no value to supply.

Applications MAY write back a value returned by an eligible value source. Finish
the requested comparisons and choose authorized writable destinations. Then use
`create` so a concurrent or different value causes a conflict. A writable
check-only store is an eligible destination. Check-only observations, misses and
conflicts do not provide a value for replication. Report a replication error
alongside the original read.

## 6. Discovery and Git

### 6.1 LOCK_PATH

`LOCK_PATH` is an ordered list of store locations. A semicolon separates
elements on every platform. This avoids collisions with URL schemes, ports,
IPv6 literals and Windows drive letters. Each element is an absolute native
path or an absolute HTTP(S) base. An optional prefix starts at the beginning
of the element and ends with `=`. It contains either flag `check` or `ro`, or
both separated by a comma in either order. Flags are case-sensitive. Reject
unknown, duplicate or empty flags. An unprefixed element is a write-eligible
value source. An `=` inside a location is literal. Windows paths can be
drive-absolute or fully qualified UNC paths. Drive-relative paths, device
namespaces, a bare drive, relative paths, and unknown schemes are invalid. Do
not expand shell syntax, tildes, or environment variables in an entry.

If a location contains a literal semicolon, put double quotes around the entire
location, after the optional prefix. Within a quoted location, two successive
double quotes encode one literal quote. Backslashes are always literal. An
unquoted location contains neither a semicolon nor a double quote. Do not trim
whitespace. Ignore empty unquoted elements, including leading or trailing ones.
A quoted empty location, an unmatched quote, or text after its closing quote
before the separator is a configuration error. Reject the whole malformed list
so a mistyped trust marker cannot silently change precedence. Examples below
show the environment value, after the calling shell's quoting rules.

```text
/work/editor/.lock/locks;/home/lee/.cache/locks;ro=/var/lib/locks/locks;check=https://witness.example:8443/team
C:\work\editor\.lock\locks;ro=\\fileserver\team\locks;check=https://[2001:db8::1]:8443/team
"/srv/teams;a/locks";check,ro="https://witness.example/team?label=a;b"
```

An explicitly empty list adds no entries and disables the platform fallback.
When `LOCK_PATH` is unset, use the recommendations below. Applications MAY add
explicit project or administrator-configured locations to either chain.
Document their precedence. Deduplicate only identical locations with the same
pair of flags. Preserve the read role and write policy of each other position.

### 6.2 Recommended default order

When operating in a known project, look first in `<project-root>/.lock/locks`.
Then use the per-user and per-host locations for that platform. A project root
comes from the caller or a trusted project-discovery mechanism. Do not infer a
remote office service.

- Linux and other FHS/XDG systems

  Per user: `$XDG_CACHE_HOME/locks`. If unset, empty or relative, use
  `$HOME/.cache/locks`.

  Per host: `/var/lib/locks/locks`

- BSD systems using `/var/db`

  Per user: same XDG rule and fallback

  Per host: `/var/db/locks/locks`

- macOS

  Per user: `~/Library/Caches/Locks/locks`

  Per host: `/Library/Application Support/Locks/locks`

- Windows

  Per user: `FOLDERID_LocalAppData\Locks\locks`

  Per host: `FOLDERID_ProgramData\Locks\locks`

Resolve native known folders with platform APIs. Omit a location if its base
cannot be determined as an absolute path. The system may remove data from user
cache locations. Also keep an irreplaceable first observation in a committed
project store or a persistent host store with backups. Administrators create
host parent directories and set their ACLs. An ordinary client does not
elevate privileges to create them.

Check default base paths before joining or cleaning them. A base containing NUL
is invalid. Treat an invalid `XDG_CACHE_HOME` as unset; omit other invalid bases.

The Go convenience client `Open(paths...)` prepends explicit locations to
`LOCK_PATH` or these defaults. It resolves relative explicit paths at setup
and does not infer a project root. `OpenWithOptions` can replace discovery
with an explicit list. Both retain the order and deduplicate only identical
addresses with identical flags. The client hashes string keys with `blake2` by
default and uses `first` for both reads and writes. Reports retain per-store
outcomes. `GetWithPolicy` selects `first` or `verify` for one call.
`PutWithPolicy` selects `first` or `all` for one call. They do not change the
client defaults. The write policy is independent of the `create` or `replace`
write mode.

Client setup performs no store I/O. Before a valid write, the client can create
missing local parents through checked directory handles. It uses restricted
permissions and leaves existing permissions unchanged. Paths must satisfy the
local trust requirements in §7. Reads and read-only stores do not create parents.
A missing parent remains a backend error. `ExistingParents` disables parent
creation. The lower-level `OpenLocal` API still requires existing parents.

### 6.3 Version control

Track the root leaf or the entire shard tree, including file-to-directory
changes. For the recommended project layout, use these anchored rules in the
repository-root `.gitignore`. For a differently named root, replace
`.lock/locks` and the root basename in the rules. Keep the patterns scoped to
that store.

```gitignore
/.lock/.locks.guard
/.lock/.locks.tmp
/.lock/.locks.old
/.lock/locks/**/.*.tmp
/.lock/locks/**/.*.old
```

For LF in the working tree, these repository-root `.gitattributes` entries cover
both representations.

```gitattributes
.lock/locks text eol=lf
.lock/locks/** text eol=lf
```

Git, editors and backup programs do not take the guard automatically. Stop live
operations before checkout, rebase, merge, cleanup and non-cooperating edits.
Resume operations after this work finishes. Recover pending sidecars before
staging or copying the store. Never run a cleaner that can remove an active
guard. A clean clone creates its own guard. Ordinary point updates require no
reordering, so unrelated records generally keep their diff context.

For a file/directory merge conflict, decode each side as a map and perform a
three-way merge by full identifier. If one side equals the base, use the
other. If both sides agree, use that value. Otherwise report a per-key
conflict, including a deletion on one side and a change on the other. Preserve
independent additions, rebuild affected shards, then validate the whole merged
tree. A Git union merge can hide duplicate keys or leave a value in the wrong
shard. Do not use it as the resolution rule.

### 6.4 Go command-line tool

The `cmd` package builds the `locks` tool. `just run` passes its arguments to
this tool. Commands are `read KEY`, `write KEY VALUE`, `check KEY [VALUE]`,
`doctor`, `fix`, `git`, and `serve`. Repeat `--path PATH` to select multiple
paths. Explicit paths replace discovery. Without explicit paths, discovery
uses `LOCK_PATH`, including an empty value. When `LOCK_PATH` is unset, the
project store precedes the user and host defaults. `--project DIR` selects the
project; otherwise the tool uses the Git worktree root or the current
directory outside Git. Git root discovery preserves native path bytes,
including trailing whitespace on Unix.

Reads use `first`, or `verify` with `--verify`. `check` uses `verify` and also
fails when any position returns an error. It can compare an expected canonical
value. Missing copies do not conflict with a value from an eligible source.
Writes use `replace` and `first`; `--create` selects `create` mode, and
`--all` selects the other write policy. Store errors remain visible on stderr
even if a fallback succeeds. `--id` selects a prehashed key. Otherwise, the
tool hashes literal argument bytes with `blake2`, or with `--algorithm sha256`.

`doctor` calls `Local.Inspect` to validate and measure each local tree while
it holds an exclusive root guard. The report counts leaves, records, the
largest leaf, overfull nonterminal leaves, and recovery sidecars. Inspection
can create a guard but preserves data and sidecars. Counts returned with an
error are incomplete. `fix` calls `Local.Rebalance`: validate the whole
authoritative tree, recover pending publications, then split overfull
nonterminal leaves. The tool holds the exclusive root guard throughout the
repair. Each split or merge uses §3.2 and commits separately. If a repair
fails, earlier completed repairs remain committed. Terminal leaves remain
valid at any record count. `fix --merge` selects `Local.RebalanceWithOptions`
with `RebalanceOptions{Merge: true}`. After recovery and splitting, it merges
eligible subtrees from the deepest directories upward, including the root.
Without this flag, small leaves remain split. Subtrees above the record or
byte limit remain split. Repair does not choose values or relocate malformed
records. The merged leaf uses the existing tracked representation.

Environment maintenance reports and skips HTTP stores. Repair also skips
read-only positions. An explicit HTTP maintenance path is an error. Maintenance
requires existing accessible local parents. `git` adds missing scoped rules
from §6.3 to the root configuration files of selected Git worktrees. It preserves
existing content. Environment paths outside worktrees and remote stores are
skipped; explicit paths in these cases are errors.

`serve` hosts one local store with the Go HTTP handler at `/lock`. It requires
exactly one explicit local `--path`; discovery and `--project` do not apply.
`--listen ADDRESS` selects the TCP address, with `127.0.0.1:8080` as the default.
Port zero selects a free port. The tool prints the base URL to stdout after
opening the listener. `--read-only` rejects writes. Writable stores create
missing parents on writes under the same rules as the Go client. The server
uses the local store's durability rules and the default protocol limits.
It provides plain HTTP without authentication; remote access requires an
authenticated HTTPS reverse proxy.

`--append` permits a write only when its key is absent. It applies to both
`create` and `replace` requests. If the key exists, the server returns a
per-key `denied` error, including when the value is equal. The existence check
and insertion use the local store lock. `--read-only` still rejects all writes
when both options are set.

`--no-parallel` selects `LocalOptions.NoParallel` for `serve` only. Use this
option only if no other client or server accesses the same store on disk while
the server runs. The server omits OS file locks but still coordinates
simultaneous HTTP requests in process. The guard file, stored format,
publication and recovery rules remain unchanged. This option can be combined
with `--read-only` and `--append`.

For `serve`, `--timeout` limits each request, connection reads and writes, idle
connections, and the shutdown wait. It does not limit the server lifetime.
Ctrl-C or SIGTERM stops the listener and waits for active requests. When the
shutdown timeout expires, the server closes remaining connections.

`-v` and `--verbose` log operations and store outcomes to stderr. Values are not
logged. `--timeout` sets a total operation deadline (30 seconds by default).
Exit codes are 0 for success, 1 for a miss, conflict, or maintenance need, and 2
for usage or operation errors. A successful write retains the partial-success
semantics of §5.2. `read` prints the value followed by LF; an empty value thus
prints one LF. Other commands print status or maintenance reports.

## 7. Trust and security

- First observation. A key digest identifies the input bytes. It does not
  authenticate a value, a writer, or the first observation. Applications that
  need to establish the source of a value should authenticate the original
  source or verify a signature before `create`. After an observation, a
  different value is a conflict that warrants investigation. Agreement among
  several replicas under common control provides no independent evidence.
- Witnesses and outages. A check-only service can cause a conflict
  deliberately or suppress its own observation. Record which stores answered
  when decisions depend on verification. Sending values to a writable witness
  discloses them to that service. Use `check,ro=` for a witness that should
  receive queries only. Choose trusted value sources and authorized
  destinations before enabling write-back.
- Shared names and algorithms. The namespace and framing in §1 prevent
  accidental aliasing between tools. An algorithm allowlist prevents unwanted
  downgrade or excessive work. Equality of key hashes cannot establish
  equality of separately supplied raw inputs beyond the collision resistance
  of the selected algorithm. Treat values as untrusted text and escape them
  for logs, terminals, or downstream formats.
- Local trust boundary. Put the parent, guard, stage and backup under
  appropriate ownership and ACLs. Restrict permission to create or replace
  directory entries. A world-writable parent defeats cooperative locking.
  Start from a trusted directory handle and resolve below it without following
  symlinks, Windows reparse points, mount crossings into untrusted storage, or
  special files. Use handle-relative operations and verify opened object
  identity and type. A preflight pathname check alone leaves a race. Refuse
  multiply linked guard or recovery files. A checkout from an untrusted
  repository must never give that repository authority to configure external
  writes.
- Resource bounds. Prehashed input lets a caller choose a shard directly.
  Limit raw key bytes, value and line bytes, total scanned bytes including
  comments, per-shard or tenant growth, outstanding batches and lock time.
  Include decompressed HTTP bytes in limits. Where practical, use streaming to
  parse input and construct stages. A budget error must not turn a partial
  read into `missing`, publish an incomplete shard, or erase existing data.
- Remote permissions and privacy. Use authenticated HTTPS with certificate
  verification for remote stores. Plain HTTP is intended for an explicitly
  trusted transport or loopback. Authorize read, create and replace
  separately, using an authenticated endpoint or tenant as the permission
  boundary. A prehashed key does not prove a caller owns its namespace. Do not
  grant public writers unrestricted replacement. GET URLs can appear in access
  logs. Base64url is reversible, and low-entropy hashed keys can be guessed.
  For private inputs, hash locally and use a POST query where available. Do
  not embed secrets in base URLs.
- Network boundaries. Do not accept untrusted project settings or
  privileged-process environment variables as authorization to contact a host.
  Restrict schemes, destinations, credentials and redirects. Never forward
  credentials, raw keys, or write bodies to a new origin on an automatic
  redirect. Bind local services deliberately, enforce timeouts, authentication
  and quotas, and use CSRF protection if a browser supplies ambient
  credentials.

## 8. Conformance cases

### 8.1 Byte vectors

Each digest below is the complete unpadded Base32 part. Append `:` and the
indicated algorithm name. The `61 62 63` and `00 0A FF` BLAKE2 entries supply the
identifiers used in the HTTP examples.

- Raw key, hex: empty

  Algorithm: `blake2`

  Digest: `BZLVDQBG4VB3F2FLF2YGBGO2UHI6LX2HO6HXPB72VNC434JP4OUA`

- Raw key, hex: `61 62 63`

  Algorithm: `blake2`

  Digest: `XXOYCPDDII4XEMLR54765GCXTOKJMTR3WHFT4QTSMLEMA2GVEMMQ`

- Raw key, hex: `00 0A FF`

  Algorithm: `blake2`

  Digest: `FYXQGQ4JSJ7PU5XKXCEGMLN66EB6X6I2XRHRLVOBCOS2HNWW55FQ`

- Raw key, hex: `61 62 63`

  Algorithm: `sha256`

  Digest: `XJ4BNP4PAHH6UQKBIDPF3LRCEOYAGYNDSYLXVHFUCD7WD4QACWWQ`

In the table below, `K` stands for the complete identifier in the second entry,
and the hex column starts immediately after `K`.

| Bytes after K, hex | Expected result |
| --- | --- |
| `20 0A` | Present, empty value |
| `20 20 78 20 0D 0A` | Value bytes `20 78 20` |
| `20 61 0D 62 0D 0D 0A` | Value bytes `61 0D 62` |
| `20 78` followed by EOF | Value `x` |
| `09 78 0A` or `20 00 0A` | Format error |

### 8.2 Interoperability and fault tests

- Change the last digest character of the empty BLAKE2 vector from `A` to `B`.

  Required result: reject the ID because its unused bits are nonzero.

- Insert a new record at sizes 511 and 512. Also replace an existing record at
  size 512.

  Required result: the first insert leaves a 512-record file. The second
  splits. Replacing at 512 does not split. Ignored lines disappear only on a
  split.

- Split a leaf containing a valid identifier with an unfamiliar algorithm name.
  Feed a reader 513 distinct IDs sharing all 52 routing characters.

  Required result: the split preserves the unfamiliar ID. A depth-52 leaf
  serves all 513 IDs without creating a deeper directory.

- Two different new keys map to the same full leaf. One writer splits while the
  other waits.

  Required result: the waiting writer re-routes under its guard. Both
  acknowledged keys survive.

- Two processes race `create` with different values for one key.

  Required result: exactly one `created` and one `conflict` at that store.
  With equal values the results are `created` and `same`.

- Kill the writer during staging, after the backup rename, after the publish
  rename, and during cleanup. Repeat for root and nested splits.

  Required result: a reader sees the old value before publication and the new
  one afterward. It never returns a false miss for a preexisting key.
  Exclusive recovery restores a usable tracked tree.

- A fresh reader observes an absent guard immediately before another process
  creates it and splits.

  Required result: the reader rechecks the guard and retries the lookup,
  including an apparent miss.

- Hold an exclusive slot in one populated shard, then read or write a different
  populated shard.

  Required result: the second operation can complete. A same-shard operation
  waits or reports a deadline error.

- A Windows open handle denies rename.

  Required result: the operation retries within its deadline or reports a
  recoverable error.

- A request contains an ID and its raw alias, or the final write line lacks its
  terminating LF.

  Required result: the whole request fails before any writes. A correctly
  sized 16-key GET works on a GET-only server.

- After the ID on a `found` line, send hex `20 0A`, then try
  `20 20 78 09 0D 79 20 0D 0A`. Try a bare ID with no value separator.

  Required result: the first value is empty. The second is
  `20 78 09 0D 79 20`. The bare ID on a `found` line is invalid.

- A two-key response has the wrong ID, is reordered, contains an extra line, or
  stops after one line.

  Required result: reject the whole response. No prefix of the batch is a
  final result.

- A check-only store says `red` before a trusted store says `blue`. Then try
  check-only hits plus an eligible miss, and finally only eligible errors.

  Required result: `verify` returns `conflict`, then `missing`, then the last
  eligible error. `first` returns `blue` in the first setup.

- A contains a value and is marked `ro=`. B is empty and marked `check=`. Read
  with `verify` and write the returned value back. Repeat with B marked
  `check,ro=`.

  Required result: A supplies the value. B receives a `create` in the first
  run and no write in the second. B cannot supply the value on a later read in
  either run.

- A write-all create succeeds at A and conflicts at B. Another run succeeds at A
  and loses contact with B.

  Required result: the first returns conflict and reports A's commit. The
  second returns partial success with B's error.

- Merge contains distinct values for the same ID, a misplaced record, or
  unresolved conflict markers.

  Required result: validation fails. No writer resolves these by using the
  last line.

## 9. Implementation precedents and references

These sources describe particular subsystems. The choices in this specification
apply to any tool implementing this format.

- Git

  Loose objects use a prefix directory. Git's file-lock API creates an
  exclusive temporary lockfile and publishes it by rename. Locks v1 uses
  prefix addressing and staged publication. A separate persistent guard
  coordinates the additional file-to-directory transition.
  [Repository layout](https://git-scm.com/docs/gitrepository-layout) ·
  [Lockfile API](https://git-scm.com/docs/api-lockfile)

- Nix

  A flake lock records resolved inputs in a graph. In a separate subsystem,
  the store garbage collector tests a write lock before treating another
  process's temporary roots file as stale. Locks v1 keeps recorded values
  separate from the evidence of current process ownership.
  [Flake locks](https://nix.dev/manual/nix/2.24/command-ref/new-cli/nix3-flake.html)
  ·
  [Store garbage collector](https://github.com/NixOS/nix/blob/master/src/libstore/gc.cc)

- Cargo

  Cargo's lockfile writer opens `Cargo.lock` exclusively, truncates it and
  writes new contents. Its package-cache implementation uses shared and
  exclusive locks with an explicit acquisition order. Locks v1 uses a fixed
  order and stages committed leaf contents so process termination cannot leave
  a truncated live leaf.
  [Lockfile writer](https://doc.rust-lang.org/stable/nightly-rustc/src/cargo/ops/lockfile.rs.html)
  ·
  [Cache locking](https://doc.rust-lang.org/stable/nightly-rustc/cargo/util/cache_lock/index.html)

- npm

  npm's cache uses `cacache`, which verifies content integrity and supports
  concurrent access. npm's separate `write-file-atomic` library writes a
  temporary file, optionally syncs it, and renames it. The library queues
  writes within a process. Independent tools also need a shared OS protocol.
  [npm cache](https://docs.npmjs.com/cli/v11/commands/npm-cache/) ·
  [cacache](https://github.com/npm/cacache) ·
  [write-file-atomic](https://github.com/npm/write-file-atomic)

- SQLite

  Its rollback-journal design distinguishes staging changes, committing, and
  recovering interrupted work. The publication and recovery rules in §3 use
  that distinction.
  [Atomic commit](https://www.sqlite.org/atomiccommit.html)
- Encodings and HTTP.
  [BLAKE2, RFC 7693](https://www.rfc-editor.org/rfc/rfc7693) ·
  [Base encodings, RFC 4648](https://www.rfc-editor.org/rfc/rfc4648) ·
  [SHA-256, FIPS 180-4](https://csrc.nist.gov/pubs/fips/180-4/upd1/final) ·
  [URI syntax, RFC 3986](https://www.rfc-editor.org/rfc/rfc3986) ·
  [Octet-stream media type, RFC 2046](https://www.rfc-editor.org/rfc/rfc2046#section-4.5.1)
  · [HTTP semantics, RFC 9110](https://www.rfc-editor.org/rfc/rfc9110) ·
  [HTTP caching, RFC 9111](https://www.rfc-editor.org/rfc/rfc9111)
- Unix and macOS.
  [Record locking and descriptor lifetime](https://man7.org/linux/man-pages/man2/fcntl_locking.2.html)
  ·
  [Rename and object types](https://man7.org/linux/man-pages/man2/rename.2.html)
- Windows.
  [Byte-range locking](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-lockfileex)
  ·
  [Rename by handle](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-setfileinformationbyhandle)
  ·
  [Move flags](https://learn.microsoft.com/en-us/windows/win32/api/winbase/nf-winbase-movefileexw)
  ·
  [Path length](https://learn.microsoft.com/en-us/windows/win32/fileio/maximum-file-path-limitation)
- Discovery and worktrees.
  [XDG base directories](https://specifications.freedesktop.org/basedir/latest/)
  ·
  [Filesystem Hierarchy Standard](https://specifications.freedesktop.org/fhs/latest-single/)
  ·
  [FreeBSD hierarchy](https://man.freebsd.org/cgi/man.cgi?query=hier&sektion=7&format=html)
  ·
  [Apple file-system directories](https://developer.apple.com/library/archive/documentation/FileManagement/Conceptual/FileSystemProgrammingGuide/FileSystemOverview/FileSystemOverview.html)
  ·
  [Windows known folders](https://learn.microsoft.com/en-us/windows/win32/shell/knownfolderid)
  · [Git attributes](https://git-scm.com/docs/gitattributes) ·
  [Git ignore patterns](https://git-scm.com/docs/gitignore)
- Trust boundaries.
  [Linux path resolution restrictions](https://man7.org/linux/man-pages/man2/openat2.2.html)
  ·
  [Windows reparse points](https://learn.microsoft.com/en-us/windows/win32/fileio/reparse-points)
  ·
  [OWASP guidance on outbound request controls](https://cheatsheetseries.owasp.org/cheatsheets/Server_Side_Request_Forgery_Prevention_Cheat_Sheet.html)
