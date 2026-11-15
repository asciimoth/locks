package locks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// ClientOptions configures a Client. Its zero value uses the current environment,
// blake2 keys, First reads and WriteFirst writes.
type ClientOptions struct {
	// Prepend adds explicit locations before Locations or discovery results.
	// Relative local paths are resolved when the client opens. Flags are retained.
	Prepend []Location
	// Locations replaces discovery when non-nil. An empty slice disables it.
	// These locations must be absolute, as in LOCK_PATH.
	Locations []Location
	// LookupEnv defaults to os.LookupEnv. It must distinguish unset and empty values.
	LookupEnv   func(string) (string, bool)
	Algorithm   string
	ReadPolicy  ReadPolicy
	WritePolicy WritePolicy
	// RawKeyBytes limits raw keys before hashing. Zero selects 1 MiB.
	RawKeyBytes int
	Local       LocalOptions
	HTTP        HTTPOptions
	// ExistingParents disables creation of missing local parent directories.
	// By default, writes create parents through checked directory handles.
	// Reads and read-only positions never create directories.
	ExistingParents bool
}

// Client hashes string keys and operates on an ordered chain. Use Open or
// OpenWithOptions to construct one. A Client is safe for concurrent use if its
// HTTP transport is safe for concurrent use. It needs no Close call.
type Client struct {
	chain       Chain
	locations   []Location
	algorithm   string
	readPolicy  ReadPolicy
	writePolicy WritePolicy
	rawKeyBytes int
}

// Open uses LOCK_PATH, including an explicitly empty value, or OS defaults.
// Each argument is one local path or HTTP(S) base to prepend, in argument order.
// Relative paths are resolved against the current directory. Arguments are not
// LOCK_PATH expressions and have no shell or environment expansion.
// Open does not access stores or create directories. It snapshots configuration;
// later changes to the environment or working directory do not change the client.
func Open(prepend ...string) (*Client, error) {
	options := ClientOptions{}
	for _, address := range prepend {
		options.Prepend = append(options.Prepend, Location{Address: address})
	}
	return OpenWithOptions(options)
}

// OpenWithOptions validates all locations and constructs local and HTTP stores.
// It removes only identical locations with identical flags, keeping the first.
// Configuration errors fail the whole setup. Backend access errors occur during
// operations and follow Chain fallback rules. No project root is inferred.
func OpenWithOptions(options ClientOptions) (*Client, error) {
	if _, err := Hash(nil, options.Algorithm); err != nil {
		return nil, err
	}
	limits, err := (Limits{RawKeyBytes: options.RawKeyBytes}).normalized()
	if err != nil {
		return nil, err
	}
	if options.ReadPolicy == "" {
		options.ReadPolicy = First
	}
	if options.WritePolicy == "" {
		options.WritePolicy = WriteFirst
	}
	if options.ReadPolicy != First && options.ReadPolicy != Verify || options.WritePolicy != WriteFirst && options.WritePolicy != WriteAll {
		return nil, fmt.Errorf("%w: client policy", ErrFormat)
	}
	locations := options.Locations
	if locations == nil {
		getenv := options.LookupEnv
		if getenv == nil {
			getenv = os.LookupEnv
		}
		locations, err = Discover("", getenv)
		if err != nil {
			return nil, err
		}
	}
	all := make([]Location, 0, len(options.Prepend)+len(locations))
	for _, loc := range options.Prepend {
		if loc.Address == "" {
			return nil, fmt.Errorf("%w: empty location", ErrFormat)
		}
		if !httpLocation(loc.Address) {
			loc.Address, err = filepath.Abs(loc.Address)
			if err != nil {
				return nil, err
			}
		}
		all = append(all, loc)
	}
	all = append(all, locations...)
	client := &Client{algorithm: options.Algorithm, readPolicy: options.ReadPolicy, writePolicy: options.WritePolicy, rawKeyBytes: limits.RawKeyBytes}
	seen := map[Location]bool{}
	for _, loc := range all {
		if !validLocation(loc.Address, runtime.GOOS) {
			return nil, fmt.Errorf("%w: location %q", ErrFormat, loc.Address)
		}
		if seen[loc] {
			continue
		}
		seen[loc] = true
		var store Store
		if httpLocation(loc.Address) {
			store, err = OpenHTTP(loc.Address, options.HTTP)
		} else {
			localOptions := options.Local
			localOptions.ReadOnly = localOptions.ReadOnly || loc.ReadOnly
			var local *Local
			local, err = OpenLocal(loc.Address, localOptions)
			store = local
			if err == nil && !options.ExistingParents && !localOptions.ReadOnly {
				store = &parentStore{local}
			}
		}
		if err != nil {
			return nil, fmt.Errorf("location %q: %w", loc.Address, err)
		}
		client.locations = append(client.locations, loc)
		client.chain = append(client.chain, Position{Store: store, CheckOnly: loc.CheckOnly, ReadOnly: loc.ReadOnly})
	}
	return client, nil
}

// Locations returns a copy in report-attempt order.
func (c *Client) Locations() []Location { return append([]Location(nil), c.locations...) }

// Chain returns a copy of the positions for prehashed IDs, per-call policies and
// explicit WriteBack. Changes to the returned slice do not change the client.
func (c *Client) Chain() Chain { return append(Chain(nil), c.chain...) }

// Get hashes the exact bytes of key and reads with the configured policy.
// Empty keys and arbitrary bytes in strings are valid. Missing and Conflict are
// semantic results, not errors. Inspect Attempts for backend failures.
func (c *Client) Get(ctx context.Context, key string) Report {
	return c.GetWithPolicy(ctx, key, c.readPolicy)
}

// GetWithPolicy reads one key using First or Verify for this call.
// Verify visits all positions, including check-only positions, and reports
// Conflict if found values differ. It does not change the client's default.
func (c *Client) GetWithPolicy(ctx context.Context, key string, policy ReadPolicy) Report {
	id, err := c.keyID(key)
	if err != nil {
		return Report{Err: err}
	}
	return c.chain.Get(ctx, id, policy)
}

// Put hashes key and writes with the configured policy. Create preserves an
// existing different value and reports Conflict. Replace permits overwrite.
// Success can be partial with WriteAll; inspect Attempts for all outcomes.
func (c *Client) Put(ctx context.Context, key, value string, mode Mode) Report {
	return c.PutWithPolicy(ctx, key, value, mode, c.writePolicy)
}

// PutWithPolicy writes one key using WriteFirst or WriteAll for this call.
// WriteAll visits every writable position unless Create finds a conflict.
// Inspect Attempts: an acknowledgement does not mean every store succeeded.
// This method does not change the client's default.
func (c *Client) PutWithPolicy(ctx context.Context, key, value string, mode Mode, policy WritePolicy) Report {
	id, err := c.keyID(key)
	if err != nil {
		return Report{Err: err}
	}
	return c.chain.Put(ctx, id, value, mode, policy)
}

// Create records a first observation. It is equivalent to Put with Create mode.
func (c *Client) Create(ctx context.Context, key, value string) Report {
	return c.Put(ctx, key, value, Create)
}

// Replace inserts or overwrites a value. It is equivalent to Put with Replace mode.
func (c *Client) Replace(ctx context.Context, key, value string) Report {
	return c.Put(ctx, key, value, Replace)
}

func (c *Client) keyID(key string) (ID, error) {
	if len(key) > c.rawKeyBytes {
		return "", ErrLimit
	}
	return Hash([]byte(key), c.algorithm)
}

// parentStore prepares parents only for valid, writable operations.
type parentStore struct{ local *Local }

func (s *parentStore) Get(ctx context.Context, id ID) (Result, error) {
	return s.local.Get(ctx, id)
}

func (s *parentStore) Put(ctx context.Context, id ID, value string, mode Mode) (Result, error) {
	if _, err := validateWrite(Write{id, value, mode}, s.local.limits); err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.local.options.LockTimeout)
	defer cancel()
	if err := prepareParent(ctx, s.local.parent); err != nil {
		return Result{}, err
	}
	return s.local.Put(ctx, id, value, mode)
}
