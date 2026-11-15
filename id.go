package locks

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/blake2b"
)

const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// ID is a canonical digest and algorithm name. Use ParseID to validate external IDs.
type ID string

// ParseID rejects aliases, padding, invalid pad bits and known algorithm length errors.
func ParseID(s string) (ID, error) {
	digest, alg, ok := strings.Cut(s, ":")
	if !ok || !validName(alg) || len(digest) < 52 || len(digest) > 103 {
		return "", fmt.Errorf("%w: identifier", ErrFormat)
	}
	b, e := b32.DecodeString(digest)
	if e != nil || len(b) < 32 || len(b) > 64 || b32.EncodeToString(b) != digest {
		return "", fmt.Errorf("%w: digest", ErrFormat)
	}
	if (alg == "blake2" || alg == "sha256") && len(b) != 32 {
		return "", fmt.Errorf("%w: digest length", ErrFormat)
	}
	return ID(s), nil
}

// Hash hashes the exact raw bytes. An empty algorithm selects blake2.
func Hash(raw []byte, algorithm string) (ID, error) {
	var sum [32]byte
	switch algorithm {
	case "", "blake2":
		algorithm = "blake2"
		sum = blake2b.Sum256(raw)
	case "sha256":
		sum = sha256.Sum256(raw)
	default:
		return "", fmt.Errorf("%w: algorithm %q", ErrUnsupported, algorithm)
	}
	return ID(b32.EncodeToString(sum[:]) + ":" + algorithm), nil
}

// ParseKey resolves an HTTP key token. Raw keys are never interpreted as paths or URLs.
func ParseKey(token string, limits Limits) (ID, error) {
	l, e := limits.normalized()
	if e != nil {
		return "", e
	}
	if strings.HasPrefix(token, "id.") {
		return ParseID(token[3:])
	}
	if !strings.HasPrefix(token, "raw.") {
		return "", fmt.Errorf("%w: key token", ErrFormat)
	}
	alg, enc, ok := strings.Cut(token[4:], ".")
	if !ok || !validName(alg) {
		return "", ErrFormat
	}
	if len(enc) > base64.RawURLEncoding.EncodedLen(l.RawKeyBytes) {
		return "", ErrLimit
	}
	b, e := base64.RawURLEncoding.Strict().DecodeString(enc)
	if e != nil || base64.RawURLEncoding.EncodeToString(b) != enc {
		return "", ErrFormat
	}
	return Hash(b, alg)
}
func prefix(id ID) string { return string(id)[:52] }
func component(p string, depth int) string {
	if depth == 0 {
		return p[:2]
	}
	return p[depth : depth+1]
}
