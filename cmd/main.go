// Command locks reads, writes, checks, and repairs Locks v1 stores.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/asciimoth/locks"
)

const usage = `Usage: locks [options] COMMAND [options] [arguments]

Commands:
  read KEY            Print the value and a final LF.
  write KEY VALUE     Create a value. Use --replace to permit an overwrite.
  check KEY [VALUE]   Compare values across the chain and optional expected VALUE.
  doctor              Validate local stores and report pending recovery and size.
  fix                 Recover local stores and split overfull leaves.
  serve               Serve one explicit local store over HTTP at /lock.
  git                 Add scoped .gitignore and .gitattributes rules.

Options:
  --path PATH         Select only this store. Repeat to form an ordered chain.
                      Accepts local paths and HTTP(S) bases.
  --project DIR       Project root. Default: Git worktree root, or current directory.
  -v, --verbose       Log operations and per-store results to stderr.
  --timeout DURATION  Operation or server request deadline (default 30s).
  --listen ADDRESS    With serve, TCP address (default 127.0.0.1:8080).
  --read-only         With serve, reject writes.
  --append            With serve, reject writes to existing keys.
  --no-parallel       With serve, omit OS file locks. No other client may access
                      the same store on disk while this server runs.
  --id                Treat KEY as a canonical prehashed identifier.
  --algorithm NAME    Raw-key hash: blake2 (default) or sha256.
  --verify            With read, compare all stores.
  --replace           With write, replace an existing different value.
  --all               With write, visit every writable store.
  --merge             With fix, merge subtrees that fit in one 512-record leaf.
  -h, --help          Show help.

Without --path, use LOCK_PATH when set (even if empty). Otherwise use
project, user, and host defaults. Options can occur before or after arguments.
Use -- before arguments that start with '-'. Keys and values are literal text.
Exit status: 0 success; 1 missing, conflict, or health issue; 2 operation or usage error.
Check fails on any store error. Read and write retain the library fallback policy;
store errors are always reported, including partial and indeterminate writes.
Doctor and fix skip remote stores. Fix skips read-only stores. Explicit remote
maintenance or read-only repair is an error. Small leaves merge only with fix --merge.
`

type paths []string

func (p *paths) String() string     { return strings.Join(*p, ";") }
func (p *paths) Set(s string) error { *p = append(*p, s); return nil }

type options struct {
	paths                                          paths
	project, algorithm, listen                     string
	verbose, id, verify, replace, all, help, merge bool
	readOnly, appendOnly, noParallel               bool
	timeout                                        time.Duration
}

type app struct {
	out, errOut io.Writer
	options     options
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, out, errOut io.Writer) int {
	a := &app{out: out, errOut: errOut}
	command, operands, err := a.parse(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(out, usage)
		return 0
	}
	if err != nil {
		return a.fail(err)
	}
	if command == "serve" {
		return a.serve(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, a.options.timeout)
	defer cancel()
	client, err := a.client(ctx)
	if err != nil {
		return a.fail(err)
	}
	switch command {
	case "doctor", "fix":
		return a.maintain(ctx, client, command == "fix")
	case "git":
		return a.git(ctx, client)
	default:
		return a.key(ctx, client, command, operands)
	}
}

func (a *app) fail(err error) int {
	fmt.Fprintf(a.errOut, "locks: %v\n", err)
	return 2
}
func (a *app) log(format string, args ...any) {
	if a.options.verbose {
		fmt.Fprintf(a.errOut, "locks: "+format+"\n", args...)
	}
}

func (a *app) parse(args []string) (string, []string, error) {
	o := &a.options
	fs := flag.NewFlagSet("locks", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Var(&o.paths, "path", "store path")
	fs.StringVar(&o.listen, "listen", "127.0.0.1:8080", "server TCP address")
	fs.BoolVar(&o.readOnly, "read-only", false, "reject server writes")
	fs.BoolVar(&o.appendOnly, "append", false, "reject writes to existing keys")
	fs.BoolVar(&o.noParallel, "no-parallel", false, "omit OS file locks; no other client may access the store on disk")
	fs.StringVar(&o.project, "project", "", "project root")
	fs.StringVar(&o.algorithm, "algorithm", "blake2", "hash algorithm")
	fs.BoolVar(&o.verbose, "v", false, "verbose")
	fs.BoolVar(&o.verbose, "verbose", false, "verbose")
	fs.BoolVar(&o.id, "id", false, "prehashed identifier")
	fs.BoolVar(&o.verify, "verify", false, "verify read")
	fs.BoolVar(&o.replace, "replace", false, "replace value")
	fs.BoolVar(&o.all, "all", false, "write all")
	fs.BoolVar(&o.merge, "merge", false, "merge small leaves during repair")
	fs.BoolVar(&o.help, "h", false, "help")
	fs.BoolVar(&o.help, "help", false, "help")
	fs.DurationVar(&o.timeout, "timeout", 30*time.Second, "deadline")
	// Move options ahead of operands, while preserving -- and option values.
	var flags, operands []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			operands = append(operands, args[i+1:]...)
			break
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			operands = append(operands, arg)
			continue
		}
		name, _, assigned := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-"), "=")
		f := fs.Lookup(name)
		if f == nil {
			return "", nil, fmt.Errorf("unknown option %q", arg)
		}
		flags = append(flags, arg)
		boolean, ok := f.Value.(interface{ IsBoolFlag() bool })
		if !assigned && !(ok && boolean.IsBoolFlag()) {
			i++
			if i == len(args) {
				return "", nil, fmt.Errorf("option %s needs a value", arg)
			}
			flags = append(flags, args[i])
		}
	}
	if err := fs.Parse(flags); err != nil {
		return "", nil, err
	}
	if o.help || len(operands) == 0 || operands[0] == "help" {
		return "", nil, flag.ErrHelp
	}
	command, operands := operands[0], operands[1:]
	valid := false
	switch command {
	case "read":
		valid = len(operands) == 1
	case "write":
		valid = len(operands) == 2
	case "check":
		valid = len(operands) == 1 || len(operands) == 2
	case "doctor", "fix", "git", "serve":
		valid = len(operands) == 0
	default:
		return "", nil, fmt.Errorf("unknown command %q; use --help", command)
	}
	if !valid {
		return "", nil, fmt.Errorf("wrong arguments for %s; use --help", command)
	}
	var optionErr error
	fs.Visit(func(f *flag.Flag) {
		allowed := true
		switch f.Name {
		case "listen", "read-only", "append", "no-parallel":
			allowed = command == "serve"
		case "project":
			allowed = command != "serve"
		case "all", "replace":
			allowed = command == "write"
		case "merge":
			allowed = command == "fix"
		case "verify":
			allowed = command == "read"
		case "id", "algorithm":
			allowed = command == "read" || command == "write" || command == "check"
		}
		if !allowed {
			optionErr = fmt.Errorf("--%s does not apply to %s", f.Name, command)
		}
	})
	if optionErr != nil {
		return "", nil, optionErr
	}
	if command == "serve" && (len(o.paths) != 1 || o.paths[0] == "" || strings.HasPrefix(strings.ToLower(o.paths[0]), "http:") || strings.HasPrefix(strings.ToLower(o.paths[0]), "https:")) {
		return "", nil, errors.New("serve requires exactly one local --path")
	}
	if o.timeout <= 0 {
		return "", nil, errors.New("timeout must be positive")
	}
	if o.id && o.algorithm != "blake2" {
		return "", nil, errors.New("--id cannot be combined with --algorithm")
	}
	return command, operands, nil
}

func gitRoot(ctx context.Context, dir string) (string, error) {
	data, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("find Git worktree for %q: %w", dir, err)
	}
	return filepath.Clean(strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")), nil
}

func (a *app) client(ctx context.Context) (*locks.Client, error) {
	o := locks.ClientOptions{}
	if len(a.options.paths) > 0 {
		o.Locations = []locks.Location{}
		for _, path := range a.options.paths {
			o.Prepend = append(o.Prepend, locks.Location{Address: path})
		}
		a.log("use explicit stores")
	} else {
		project := a.options.project
		if project == "" {
			cwd, err := os.Getwd()
			if err != nil {
				return nil, err
			}
			project, err = gitRoot(ctx, cwd)
			if err != nil {
				project = cwd
			}
		} else {
			if strings.ContainsRune(project, 0) {
				return nil, locks.ErrFormat
			}
			var err error
			project, err = filepath.Abs(project)
			if err != nil {
				return nil, err
			}
		}
		a.log("discover stores for project %q", project)
		locations, err := locks.Discover(project, os.LookupEnv)
		if err != nil {
			return nil, err
		}
		// A nil slice from an empty LOCK_PATH must still disable discovery.
		o.Locations = append([]locks.Location{}, locations...)
	}
	o.Algorithm = a.options.algorithm
	o.Local.LockTimeout = a.options.timeout
	client, err := locks.OpenWithOptions(o)
	if err == nil {
		for _, loc := range client.Locations() {
			a.log("store %q (check=%t, ro=%t)", loc.Address, loc.CheckOnly, loc.ReadOnly)
		}
	}
	return client, err
}

func (a *app) key(ctx context.Context, client *locks.Client, command string, args []string) int {
	var id locks.ID
	var err error
	if a.options.id {
		id, err = locks.ParseID(args[0])
	} else {
		if len(args[0]) > 1<<20 {
			return a.fail(locks.ErrLimit)
		}
		id, err = locks.Hash([]byte(args[0]), a.options.algorithm)
	}
	if err != nil {
		return a.fail(err)
	}
	var expected string
	if command == "check" && len(args) == 2 {
		expected, err = locks.NormalizeValue(args[1])
		if err != nil {
			return a.fail(err)
		}
	}
	a.log("%s %s", command, id)
	var report locks.Report
	if command == "write" {
		mode, policy := locks.Create, locks.WriteFirst
		if a.options.replace {
			mode = locks.Replace
		}
		if a.options.all {
			policy = locks.WriteAll
		}
		a.log("write mode=%s policy=%s", mode, policy)
		report = client.Chain().Put(ctx, id, args[1], mode, policy)
	} else {
		policy := locks.First
		if command == "check" || a.options.verify {
			policy = locks.Verify
		}
		a.log("read policy=%s", policy)
		report = client.Chain().Get(ctx, id, policy)
	}
	failed := false
	locations := client.Locations()
	for i, attempt := range report.Attempts {
		address := locations[i].Address
		switch {
		case attempt.Err != nil:
			failed = true
			fmt.Fprintf(a.errOut, "locks: store %q: %v\n", address, attempt.Err)
		case attempt.Skipped:
			a.log("skip %q by policy", address)
		case !attempt.Attempted:
			a.log("did not visit %q", address)
		default:
			a.log("store %q: %s", address, attempt.Result.Status)
		}
	}
	if report.Err != nil {
		return a.fail(report.Err)
	}
	if report.Result.Status == locks.Missing || report.Result.Status == locks.Conflict {
		fmt.Fprintln(a.errOut, "locks:", report.Result.Status)
		return 1
	}
	if command == "check" {
		if failed {
			return a.fail(errors.New("check incomplete: some stores failed"))
		}
		if len(args) == 2 && report.Result.Value != expected {
			fmt.Fprintln(a.errOut, "locks: value differs from expected value")
			return 1
		}
		_, err = fmt.Fprintln(a.out, "ok")
	} else if command == "read" {
		_, err = fmt.Fprintln(a.out, report.Result.Value)
	} else {
		_, err = fmt.Fprintln(a.out, report.Result.Status)
	}
	if err != nil {
		return a.fail(err)
	}
	return 0
}

func remote(address string) bool {
	return strings.HasPrefix(strings.ToLower(address), "http://") || strings.HasPrefix(strings.ToLower(address), "https://")
}

func (a *app) maintain(ctx context.Context, client *locks.Client, repair bool) int {
	code, visited := 0, 0
	for _, loc := range client.Locations() {
		if remote(loc.Address) || repair && loc.ReadOnly {
			reason := "read-only store"
			if remote(loc.Address) {
				reason = "HTTP has no full-tree maintenance operation"
			}
			fmt.Fprintf(a.errOut, "locks: skip %q: %s\n", loc.Address, reason)
			if len(a.options.paths) > 0 {
				code = 2
			}
			continue
		}
		visited++
		store, err := locks.OpenLocal(loc.Address, locks.LocalOptions{ReadOnly: loc.ReadOnly, LockTimeout: a.options.timeout})
		if err == nil && repair {
			a.log("recover and split %q", loc.Address)
			if a.options.merge {
				a.log("merge small subtrees in %q", loc.Address)
			}
			err = store.RebalanceWithOptions(ctx, locks.RebalanceOptions{Merge: a.options.merge})
		}
		var h locks.Health
		if err == nil {
			a.log("inspect %q under the root guard", loc.Address)
			h, err = store.Inspect(ctx)
		}
		if err != nil {
			fmt.Fprintf(a.errOut, "locks: store %q: %v\n", loc.Address, err)
			code = 2
			continue
		}
		_, err = fmt.Fprintf(a.out, "%q: leaves=%d records=%d largest=%d overfull=%d pending=%d\n", loc.Address, h.Leaves, h.Records, h.Largest, h.Overfull, h.Pending)
		if err != nil {
			return a.fail(err)
		}
		if (h.Pending > 0 || h.Overfull > 0) && code == 0 {
			code = 1
		}
	}
	if visited == 0 && code == 0 {
		return a.fail(errors.New("no local stores selected"))
	}
	return code
}
