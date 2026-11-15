package locks

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Record is one stored value.
type Record struct {
	ID    ID
	Value string
}
type leafLine struct {
	raw string
	id  ID
}

// Leaf keeps ignored lines and record positions for edits without a split.
type Leaf struct {
	Records []Record
	lines   []leafLine
}

func readBounded(r io.Reader, n int) ([]byte, error) {
	if r == nil {
		return nil, ErrFormat
	}
	b, e := io.ReadAll(io.LimitReader(r, int64(n)+1))
	if e != nil {
		return nil, e
	}
	if len(b) > n {
		return nil, ErrLimit
	}
	return b, nil
}

// DecodeLeaf validates the whole leaf, including duplicates and its routing prefix.
func DecodeLeaf(r io.Reader, pathPrefix string, limits Limits) (*Leaf, error) {
	l, e := limits.normalized()
	if e != nil {
		return nil, e
	}
	if len(pathPrefix) > 52 || strings.IndexFunc(pathPrefix, func(r rune) bool { return !strings.ContainsRune(alphabet, r) }) >= 0 {
		return nil, ErrFormat
	}
	b, e := readBounded(r, l.ScanBytes)
	if e != nil {
		return nil, e
	}
	if !utf8.Valid(b) || bytes.ContainsRune(b, 0) || bytes.HasPrefix(b, []byte{239, 187, 191}) {
		return nil, ErrFormat
	}
	leaf := &Leaf{}
	seen := map[ID]bool{}
	for len(b) > 0 {
		line, rest, ended := bytes.Cut(b, []byte{'\n'})
		b = rest
		if len(line) > l.LineBytes {
			return nil, ErrLimit
		}
		raw := string(line)
		if ended {
			raw += "\n"
		}
		s := strings.TrimRight(string(line), "\r")
		if marker(s) {
			return nil, fmt.Errorf("%w: conflict marker", ErrFormat)
		}
		if s == "" || !strings.ContainsRune(alphabet, rune(s[0])) {
			leaf.lines = append(leaf.lines, leafLine{raw: raw})
			continue
		}
		key, value, ok := strings.Cut(s, " ")
		if !ok {
			return nil, ErrFormat
		}
		id, e := ParseID(key)
		if e != nil {
			return nil, e
		}
		if seen[id] || !strings.HasPrefix(key, pathPrefix) {
			return nil, fmt.Errorf("%w: duplicate or misplaced record", ErrFormat)
		}
		if len(value) > l.ValueBytes {
			return nil, ErrLimit
		}
		seen[id] = true
		leaf.Records = append(leaf.Records, Record{id, value})
		leaf.lines = append(leaf.lines, leafLine{raw, id})
	}
	return leaf, nil
}
func marker(s string) bool {
	if len(s) < 7 || !strings.ContainsRune("<|=>", rune(s[0])) {
		return false
	}
	i := 1
	for i < len(s) && s[i] == s[0] {
		i++
	}
	return i >= 7 && (i == len(s) || s[i] == ' ')
}

// EncodeLeaf writes canonical records. It rejects duplicates before writing any bytes.
func EncodeLeaf(w io.Writer, records []Record) error {
	if w == nil {
		return ErrFormat
	}
	var b strings.Builder
	seen := map[ID]bool{}
	for _, r := range records {
		if _, e := ParseID(string(r.ID)); e != nil {
			return e
		}
		if seen[r.ID] {
			return ErrFormat
		}
		seen[r.ID] = true
		v, e := NormalizeValue(r.Value)
		if e != nil {
			return e
		}
		fmt.Fprintf(&b, "%s %s\n", r.ID, v)
	}
	n, e := io.WriteString(w, b.String())
	if e == nil && n != b.Len() {
		return io.ErrShortWrite
	}
	return e
}
func (l *Leaf) edit(w Write) (Result, []byte, []Record) {
	records := append([]Record(nil), l.Records...)
	idx := -1
	for i, r := range records {
		if r.ID == w.ID {
			idx = i
			break
		}
	}
	old := ""
	if idx >= 0 {
		old = records[idx].Value
	}
	result := decide(w.ID, old, idx >= 0, w)
	if result.Status == Same || result.Status == Conflict {
		return result, nil, records
	}
	if idx < 0 {
		records = append(records, Record{w.ID, w.Value})
	} else {
		records[idx].Value = w.Value
	}
	var b strings.Builder
	for _, line := range l.lines {
		if line.id == w.ID {
			fmt.Fprintf(&b, "%s %s\n", w.ID, w.Value)
		} else {
			b.WriteString(line.raw)
		}
	}
	if idx < 0 {
		s := b.String()
		if len(s) > 0 && s[len(s)-1] != '\n' {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s %s\n", w.ID, w.Value)
	}
	return result, []byte(b.String()), records
}

// shardName validates tracked names and recognized recovery siblings.
func shardName(name string, depth int) (base, side string, err error) {
	if depth < 0 || depth == 1 || depth >= 52 {
		return "", "", ErrFormat
	}
	base = name
	if strings.HasPrefix(name, ".") && (strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".old")) {
		if len(name) < 5 {
			return "", "", ErrFormat
		}
		base = name[1 : len(name)-4]
		side = name[len(name)-3:]
	}
	want := 1
	if depth == 0 {
		want = 2
	}
	if len(base) != want || strings.Trim(base, alphabet) != "" {
		return "", "", fmt.Errorf("%w: shard %q", ErrFormat, name)
	}
	return base, side, nil
}
