package locks

import (
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func pathSeeds() []string {
	return []string{"", ";;", "HTTP://example.com/a", "check=Https://example.com/a", "/a;/b;ro=/c;check=https://example.com/team", "/a=/b", "check,ro=\"https://example.com/?label=a;b\"", "\"/a;b\"", "\"/a\"\"b\"", "ro,check=/a", "/a;/a;ro=/a", "ro,ro=/a", "check,check=/a", "bad=/a", ",ro=/a", "ro,=/a", "ro=", "relative", "\"\"", "\"/a", "\"/a\"x", "/a\"b", " ;/a", "ro=/a;;", `C:\a;ro=\\server\share\locks`, `C:relative`, `C:`, `\\?\C:\a`, `\\.\a`, `\\host\share`, `\\host`, `\\host\`, `\rooted`, "https://example.com/?x=a=b", "ro=https://example.com/?key=x"}
}
func endpointSeeds() []string {
	return []string{"https://example.com/foo/bar/?tenant=west#note", "https://example.com/a%2Fb/%2F/?x=%2f", "http://[::1]:123/a", "http://example.com", "https://example.com/?key=x", "https://example.com/?%6bey=x", "https://example.com/?other=%zz", "https://example.com/?%zz=x", "https://a:b@example.com/a", "ftp://example.com/a", "/relative", "http:", "http:///x", "https://%zz/", "http://x/%zz", "https://x/a?x=+", "http://x/?key=%zz"}
}
func TestPath(t *testing.T) {
	for _, s := range pathSeeds() {
		for _, os := range []string{"linux", "windows"} {
			_, _ = ParsePathForOS(s, os)
		}
	}
	p, e := ParsePathForOS("/a;/a;ro=/a;check,ro=\"https://example.com/a;b\";;", "linux")
	if e != nil || len(p) != 3 || !p[2].CheckOnly || !p[2].ReadOnly {
		t.Fatal(p, e)
	}
	p, e = ParsePathForOS(`C:\work\locks;ro=\\fileserver\team\locks`, "windows")
	if e != nil || len(p) != 2 {
		t.Fatal(p, e)
	}
	for _, s := range []string{`C:relative`, `C:`, `\\?\C:\a`, `\\.\a`, `\\host`, `\\host\`, `\rooted`, `/posix`, `\\.\share`, `\\host\..`} {
		if _, e = ParsePathForOS(s, "windows"); e == nil {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"bad=/a", "ro,ro=/a", "check,check=/a", "ro=", `"/a"x`, `""`, `"/a`, "relative", "/a\"b", "ro=/a;check=", "/a\x00"} {
		if _, e = ParsePathForOS(s, "linux"); e == nil {
			t.Fatal(s)
		}
	}
}

func TestDiscoverNilLookup(t *testing.T) {
	_, err := Discover("", nil)
	requireErr(t, err, ErrFormat)
}
func TestEndpoint(t *testing.T) {
	for _, s := range endpointSeeds() {
		_, _ = Endpoint(s)
	}
	for _, tt := range []struct{ base, want string }{{"https://example.com/foo/bar/?tenant=west#note", "https://example.com/foo/bar/lock?tenant=west"}, {"https://example.com/a%2Fb/%2F/?x=%2f", "https://example.com/a%2Fb/%2F/lock?x=%2f"}} {
		got, e := Endpoint(tt.base)
		if e != nil || got != tt.want {
			t.Fatal(got, e)
		}
	}
	keys, e := queryKeys("x=%zz&key=raw.blake2.YWJj&key=a+b&%6bey=a%2520b")
	if e != nil || strings.Join(keys, "|") != "raw.blake2.YWJj|a+b|a%20b" {
		t.Fatal(keys, e)
	}
}
func TestDiscover(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux defaults")
	}
	get := func(vars map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { s, ok := vars[k]; return s, ok }
	}
	for _, tt := range []struct {
		env     map[string]string
		project string
		want    string
	}{{map[string]string{"LOCK_PATH": ""}, "/p", ""}, {map[string]string{"LOCK_PATH": "/explicit"}, "/p", "/explicit"}, {map[string]string{"HOME": "/home/a"}, "/p", "/p/.lock/locks;/home/a/.cache/locks;/var/lib/locks/locks"}, {map[string]string{"HOME": "/home/a", "XDG_CACHE_HOME": "relative"}, "", "/home/a/.cache/locks;/var/lib/locks/locks"}, {map[string]string{"XDG_CACHE_HOME": "/cache"}, "", "/cache/locks;/var/lib/locks/locks"}, {map[string]string{}, "", "/var/lib/locks/locks"}} {
		p, e := Discover(tt.project, get(tt.env))
		var ss []string
		for _, x := range p {
			ss = append(ss, x.Address)
		}
		if e != nil || strings.Join(ss, ";") != tt.want {
			t.Fatal(p, e)
		}
	}
	_, e := Discover("relative", get(nil))
	requireErr(t, e, ErrFormat)
	_, e = Discover("/tmp/\x00/../project", get(nil))
	requireErr(t, e, ErrFormat)
}

func TestOtherPlatformDefaults(t *testing.T) {
	env := func(k string) (string, bool) {
		if k == "HOME" {
			return "/home/a", true
		}
		return "", false
	}
	for _, os := range []string{"darwin", "freebsd", "openbsd", "netbsd", "dragonfly", "windows"} {
		t.Run(os, func(t *testing.T) {
			if runtime.GOOS == "windows" && os != "windows" {
				t.Skip("discovery uses native path joins; POSIX defaults need a POSIX host")
			}
			for _, folders := range []func() (string, string){func() (string, string) { return `C:\Users\a\AppData\Local`, `C:\ProgramData` }, func() (string, string) { return "", "" }, knownFolders} {
				p, e := discover(os, "", env, folders)
				if e != nil {
					t.Fatal(e)
				}
				for _, loc := range p {
					if !validLocation(loc.Address, os) {
						t.Fatal(loc)
					}
				}
			}
		})
	}
	for _, s := range []string{`//?/C:/a`, `\\?/C:/a`, `//./C:/a`} {
		if validLocation(s, "windows") {
			t.Fatal("device alias", s)
		}
	}
	if _, e := Endpoint("http://:80"); e == nil {
		t.Fatal("empty host")
	}
}

func TestDiscoverRejectsInvalidDefaultBases(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX environment paths")
	}
	for _, tt := range []struct {
		name, goos, home, cache, want string
	}{
		{"home-nul", "linux", "/tmp/\x00/../home", "", "/var/lib/locks/locks"},
		{"cache-nul", "linux", "/home/a", "/tmp/\x00/../cache", "/home/a/.cache/locks;/var/lib/locks/locks"},
		{"mac-home-nul", "darwin", "/tmp/\x00/../home", "", "/Library/Application Support/Locks/locks"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(key string) (string, bool) {
				value, ok := map[string]string{"HOME": tt.home, "XDG_CACHE_HOME": tt.cache}[key]
				return value, ok
			}
			locations, err := discover(tt.goos, "", getenv, knownFolders)
			var addresses []string
			for _, loc := range locations {
				addresses = append(addresses, loc.Address)
			}
			if err != nil || strings.Join(addresses, ";") != tt.want {
				t.Fatal(locations, err)
			}
		})
	}
}

func TestWindowsDefaultFolders(t *testing.T) {
	for _, tt := range []struct {
		name, user, host string
		want             []string
	}{
		{"both", `C:\Users\a\AppData\Local`, `C:\ProgramData`, []string{"C:/Users/a/AppData/Local/Locks/locks", "C:/ProgramData/Locks/locks"}},
		{"user-only", `C:\Users\a\AppData\Local`, "", []string{"C:/Users/a/AppData/Local/Locks/locks"}},
		{"host-only", "", `C:\ProgramData`, []string{"C:/ProgramData/Locks/locks"}},
		{"unc", `\\server\share\user`, `\\server\share\host`, []string{"//server/share/user/Locks/locks", "//server/share/host/Locks/locks"}},
		{"absent", "", "", nil},
		{"relative", `C:user`, `host`, nil},
		{"nul", "C:/tmp/\x00/../user", "C:/tmp/\x00/../host", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			locations, err := discover("windows", "", func(string) (string, bool) { return "", false },
				func() (string, string) { return tt.user, tt.host })
			var addresses []string
			for _, loc := range locations {
				if !validLocation(loc.Address, "windows") || loc.CheckOnly || loc.ReadOnly {
					t.Fatalf("invalid default location: %+v", loc)
				}
				addresses = append(addresses, strings.ReplaceAll(loc.Address, `\`, "/"))
			}
			if err != nil || !reflect.DeepEqual(addresses, tt.want) {
				t.Fatalf("locations = %v, error = %v; want %v", addresses, err, tt.want)
			}
		})
	}
}

func TestHTTPSchemeCase(t *testing.T) {
	for _, os := range []string{"linux", "windows"} {
		locations, e := ParsePathForOS("HTTP://example.com/a;check=Https://example.com/b", os)
		if e != nil || len(locations) != 2 {
			t.Fatal(locations, e)
		}
	}
}
