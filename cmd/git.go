package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/asciimoth/locks"
)

func (a *app) git(ctx context.Context, client *locks.Client) int {
	code, selected := 0, 0
	for _, loc := range client.Locations() {
		if remote(loc.Address) {
			a.log("skip remote store %q for Git rules", loc.Address)
			if len(a.options.paths) > 0 {
				code = a.fail(errors.New("Git rules require local paths"))
			}
			continue
		}
		// Find an existing ancestor so a new .lock directory needs no creation.
		dir := filepath.Dir(loc.Address)
		for {
			_, err := os.Stat(dir)
			if !errors.Is(err, os.ErrNotExist) || dir == filepath.Dir(dir) {
				break
			}
			dir = filepath.Dir(dir)
		}
		root, err := gitRoot(ctx, dir)
		if err != nil {
			if len(a.options.paths) > 0 {
				code = a.fail(err)
			} else {
				a.log("skip %q: no Git worktree", loc.Address)
			}
			continue
		}
		rel, err := filepath.Rel(root, loc.Address)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			code = a.fail(fmt.Errorf("store %q must be inside its Git worktree", loc.Address))
			continue
		}
		selected++
		ignore, attributes, err := gitRules(filepath.ToSlash(rel))
		if err != nil {
			code = a.fail(err)
			continue
		}
		for _, item := range []struct {
			name  string
			rules []string
		}{{".gitignore", ignore}, {".gitattributes", attributes}} {
			path := filepath.Join(root, item.name)
			a.log("check Git rules in %q for %q", path, loc.Address)
			changed, err := addRules(path, item.rules)
			if err != nil {
				code = a.fail(err)
				continue
			}
			status := "unchanged"
			if changed {
				status = "updated"
			}
			if _, err = fmt.Fprintf(a.out, "%s %q\n", status, path); err != nil {
				return a.fail(err)
			}
		}
	}
	if selected == 0 && code == 0 {
		return a.fail(errors.New("no stores inside Git worktrees selected"))
	}
	return code
}

func gitRules(path string) ([]string, []string, error) {
	if strings.ContainsAny(path, "\x00\r\n") {
		return nil, nil, errors.New("Git rules cannot represent NUL or line breaks in a store path")
	}
	// Quote glob metacharacters, then quote whitespace for each file format.
	literal := strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`, "]", `\]`).Replace(path)
	parent, name := "", literal
	if i := strings.LastIndex(literal, "/"); i >= 0 {
		parent, name = literal[:i+1], literal[i+1:]
	}
	ignore := []string{"/" + parent + "." + name + ".guard", "/" + parent + "." + name + ".tmp", "/" + parent + "." + name + ".old", "/" + literal + "/**/.*.tmp", "/" + literal + "/**/.*.old"}
	for i := range ignore {
		ignore[i] = strings.NewReplacer(" ", `\ `, "\t", "\\\t").Replace(ignore[i])
	}
	attribute := func(pattern string) string {
		if strings.IndexFunc(pattern, func(r rune) bool { return r <= 32 || r >= 127 || r == '\\' || r == '"' }) >= 0 || strings.HasPrefix(pattern, "#") || strings.HasPrefix(pattern, "!") {
			// Git accepts C-style octal byte escapes, not Go's hex or
			// Unicode escapes. Preserve arbitrary native path bytes.
			var quoted strings.Builder
			quoted.WriteByte('"')
			for i := 0; i < len(pattern); i++ {
				c := pattern[i]
				switch {
				case c == '\\' || c == '"':
					quoted.WriteByte('\\')
					quoted.WriteByte(c)
				case c < 32 || c >= 127:
					fmt.Fprintf(&quoted, "\\%03o", c)
				default:
					quoted.WriteByte(c)
				}
			}
			quoted.WriteByte('"')
			pattern = quoted.String()
		}
		return pattern + " text eol=lf"
	}
	if !strings.Contains(literal, "/") || strings.HasPrefix(literal, "#") || strings.HasPrefix(literal, "!") {
		literal = "/" + literal
	}
	return ignore, []string{attribute(literal), attribute(literal + "/**")}, nil
}

// Append only missing exact rules. Keep existing content and file permissions.
func addRules(path string, rules []string) (changed bool, err error) {
	st, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	var data []byte
	mode := os.FileMode(0644)
	if st != nil {
		if !st.Mode().IsRegular() {
			return false, fmt.Errorf("%q is not a regular file", path)
		}
		data, err = os.ReadFile(path)
		if err != nil {
			return false, err
		}
		mode = st.Mode().Perm()
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		seen[strings.TrimSuffix(line, "\r")] = true
	}
	var added []string
	for _, rule := range rules {
		if !seen[rule] {
			added = append(added, rule)
			seen[rule] = true
		}
	}
	if len(added) == 0 {
		return false, nil
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	data = append(data, []byte(strings.Join(added, "\n")+"\n")...)
	f, err := os.CreateTemp(filepath.Dir(path), ".locks-git-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(f.Name())
	n, writeErr := f.Write(data)
	if writeErr == nil && n != len(data) {
		writeErr = io.ErrShortWrite
	}
	err = errors.Join(writeErr, f.Chmod(mode), f.Close())
	if err != nil {
		return false, err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return false, err
	}
	return true, nil
}
