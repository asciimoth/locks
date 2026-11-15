// Package locks implements the Locks v1 format and protocol in spec.md.
//
// Hash produces a canonical ID from exact input bytes. Store separates missing
// keys from empty values and supports conditional create and unconditional
// replace. Conflict is a semantic Result, not an operational error.
//
// Local implements the local publication protocol on Linux. HTTPStore
// and Handler implement the HTTP protocol on all Go platforms. Chain supplies
// ordered reads, verification, writes and explicit write-back with per-store
// reports. DecodeLeaf, DecodeRequest and DecodeResults validate complete inputs.
//
// Local roots require existing trusted parents on a filesystem
// with coherent fcntl locks and atomic same-directory rename. Local storage
// provides process-crash recovery and returns ErrUnsupported on other operating
// systems. No background cleanup, implicit replication, remote discovery or
// automatic replace retry runs.
package locks
