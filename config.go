package locks

import (
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
)

// Location defines independent read and write policies for a store.
type Location struct {
	Address             string
	CheckOnly, ReadOnly bool
}

// Endpoint constructs the v1 endpoint while preserving encoded paths and query bytes.
func Endpoint(base string) (string, error) {
	u, e := url.Parse(base)
	if e != nil || u == nil {
		return "", ErrFormat
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return "", ErrFormat
	}
	keys, e := queryKeys(u.RawQuery)
	if e != nil || len(keys) > 0 {
		return "", ErrFormat
	}
	p := strings.TrimRight(u.EscapedPath(), "/") + "/lock"
	// EscapedPath returns a valid encoding, including when RawPath is invalid.
	u.Path, _ = url.PathUnescape(p)
	u.RawPath = p
	u.Fragment = ""
	u.RawFragment = ""
	return u.String(), nil
}

// queryKeys decodes once and treats '+' as a literal. Unknown query values are ignored.
func queryKeys(query string) ([]string, error) {
	var keys []string
	for _, f := range strings.Split(query, "&") {
		k, v, _ := strings.Cut(f, "=")
		name, e := url.PathUnescape(k)
		if e != nil {
			return nil, ErrFormat
		}
		if name == "key" {
			s, e := url.PathUnescape(v)
			if e != nil {
				return nil, ErrFormat
			}
			keys = append(keys, s)
		}
	}
	return keys, nil
}

// ParsePath parses LOCK_PATH without expansion or whitespace removal. Empty entries are ignored.
func ParsePath(s string) ([]Location, error) { return ParsePathForOS(s, runtime.GOOS) }

// ParsePathForOS permits deterministic parsing of another platform's native paths.
func ParsePathForOS(s, goos string) ([]Location, error) {
	var result []Location
	seen := map[Location]bool{}
	for len(s) > 0 {
		loc := Location{}
		// A prefix precedes a location. An '=' inside an absolute location is data.
		if s[0] != '"' && !locationStart(s, goos) {
			flags, rest, ok := strings.Cut(s, "=")
			if !ok {
				return nil, ErrFormat
			}
			s = rest
			for _, flag := range strings.Split(flags, ",") {
				switch flag {
				case "check":
					if loc.CheckOnly {
						return nil, ErrFormat
					}
					loc.CheckOnly = true
				case "ro":
					if loc.ReadOnly {
						return nil, ErrFormat
					}
					loc.ReadOnly = true
				default:
					return nil, ErrFormat
				}
			}
		}
		quoted := strings.HasPrefix(s, "\"")
		if quoted {
			s = s[1:]
			var b strings.Builder
			closed := false
			for len(s) > 0 {
				if s[0] != '"' {
					b.WriteByte(s[0])
					s = s[1:]
					continue
				}
				if len(s) > 1 && s[1] == '"' {
					b.WriteByte('"')
					s = s[2:]
					continue
				}
				s = s[1:]
				closed = true
				break
			}
			if !closed || len(s) > 0 && s[0] != ';' {
				return nil, ErrFormat
			}
			loc.Address = b.String()
			if loc.Address == "" {
				return nil, ErrFormat
			}
		} else {
			loc.Address, s, _ = strings.Cut(s, ";")
			if strings.ContainsRune(loc.Address, '"') {
				return nil, ErrFormat
			}
		}
		if quoted && strings.HasPrefix(s, ";") {
			s = s[1:]
		}
		if loc.Address == "" {
			if loc.ReadOnly || loc.CheckOnly {
				return nil, ErrFormat
			}
			continue
		}
		if !validLocation(loc.Address, goos) {
			return nil, fmt.Errorf("%w: location %q", ErrFormat, loc.Address)
		}
		if !seen[loc] {
			result = append(result, loc)
			seen[loc] = true
		}
	}
	return result, nil
}
func httpLocation(s string) bool {
	scheme, _, ok := strings.Cut(s, ":")
	return ok && (strings.EqualFold(scheme, "http") || strings.EqualFold(scheme, "https"))
}
func locationStart(s, goos string) bool {
	return strings.HasPrefix(s, "/") || httpLocation(s) || strings.HasPrefix(s, ";") || goos == "windows" && (strings.HasPrefix(s, `\`) || len(s) > 1 && s[1] == ':')
}
func validLocation(s, goos string) bool {
	if httpLocation(s) {
		_, e := Endpoint(s)
		return e == nil
	}
	if strings.ContainsRune(s, 0) {
		return false
	}
	if goos != "windows" {
		return strings.HasPrefix(s, "/")
	}
	s = strings.ReplaceAll(s, "/", `\`)
	if strings.HasPrefix(s, `\\?\`) || strings.HasPrefix(s, `\\.\`) {
		return false
	}
	if len(s) >= 3 && ((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z')) && s[1] == ':' && (s[2] == '\\' || s[2] == '/') {
		return true
	}
	if !strings.HasPrefix(s, `\\`) {
		return false
	}
	parts := strings.Split(strings.ReplaceAll(s[2:], "/", `\`), `\`)
	return len(parts) >= 2 && parts[0] != "" && parts[1] != "" && parts[0] != "." && parts[0] != ".." && parts[1] != "." && parts[1] != ".."
}

// Discover uses LOCK_PATH when set, including when empty. Project precedes user and host defaults.
// getenv must distinguish an absent variable from an explicitly empty variable.
func Discover(project string, getenv func(string) (string, bool)) ([]Location, error) {
	return discover(runtime.GOOS, project, getenv, knownFolders)
}
func discover(goos, project string, getenv func(string) (string, bool), folders func() (string, string)) ([]Location, error) {
	if s, ok := getenv("LOCK_PATH"); ok {
		return ParsePathForOS(s, goos)
	}
	var result []Location
	add := func(s string) {
		if validLocation(s, goos) {
			result = append(result, Location{Address: s})
		}
	}
	if project != "" {
		if !filepath.IsAbs(project) {
			return nil, ErrFormat
		}
		add(filepath.Join(project, ".lock", "locks"))
	}
	home, _ := getenv("HOME")
	switch goos {
	case "windows":
		user, host := folders()
		if user != "" {
			add(filepath.Join(user, "Locks", "locks"))
		}
		if host != "" {
			add(filepath.Join(host, "Locks", "locks"))
		}
	case "darwin":
		if filepath.IsAbs(home) {
			add(filepath.Join(home, "Library", "Caches", "Locks", "locks"))
		}
		add("/Library/Application Support/Locks/locks")
	default:
		cache, _ := getenv("XDG_CACHE_HOME")
		if !filepath.IsAbs(cache) && filepath.IsAbs(home) {
			cache = filepath.Join(home, ".cache")
		}
		if filepath.IsAbs(cache) {
			add(filepath.Join(cache, "locks"))
		}
		switch goos {
		case "freebsd", "openbsd", "netbsd", "dragonfly":
			add("/var/db/locks/locks")
		default:
			add("/var/lib/locks/locks")
		}
	}
	return result, nil
}
