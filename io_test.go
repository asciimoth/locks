package locks

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestNilParserInputs(t *testing.T) {
	_, err := DecodeLeaf(nil, "", Limits{})
	requireErr(t, err, ErrFormat)
	_, err = DecodeRequest(nil, Limits{})
	requireErr(t, err, ErrFormat)
	_, err = DecodeResults(nil, Request{Items: []Write{{ID: abc}}}, Limits{})
	requireErr(t, err, ErrFormat)
	requireErr(t, DecodeError(nil, 400, Limits{}), ErrFormat)
	requireErr(t, EncodeLeaf(nil, []Record{{abc, "v"}}), ErrFormat)
}

func TestEncodeLeafShortWrite(t *testing.T) {
	requireErr(t, EncodeLeaf(shortWriter{}, []Record{{abc, "v"}}), io.ErrShortWrite)
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestMalformedReaders(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reader io.Reader
		want   error
	}{
		{"nil pointer", (*strings.Reader)(nil), ErrFormat},
		{"nil function", readerFunc(nil), ErrFormat},
		{"negative count", readerFunc(func([]byte) (int, error) { return -1, nil }), ErrFormat},
		{"large count", readerFunc(func(p []byte) (int, error) { return len(p) + 1, io.ErrClosedPipe }), ErrFormat},
		{"no progress", readerFunc(func([]byte) (int, error) { return 0, nil }), io.ErrNoProgress},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeLeaf(tc.reader, "", Limits{})
			requireErr(t, err, tc.want)
			_, err = DecodeRequest(tc.reader, Limits{})
			requireErr(t, err, tc.want)
			_, err = DecodeResults(tc.reader, Request{Items: []Write{{ID: abc}}}, Limits{})
			requireErr(t, err, tc.want)
			requireErr(t, DecodeError(tc.reader, 400, Limits{}), tc.want)
		})
	}
	_, err := DecodeLeaf(readerFunc(func(p []byte) (int, error) { return len(p) + 1, io.ErrClosedPipe }), "", Limits{})
	requireErr(t, err, io.ErrClosedPipe)
	var writer *bytes.Buffer
	requireErr(t, EncodeLeaf(writer, []Record{{abc, "value"}}), ErrFormat)
}

func TestReaderTemporaryNoProgress(t *testing.T) {
	data := strings.NewReader(string(abc) + " value\n")
	calls := 0
	r := readerFunc(func(p []byte) (int, error) {
		calls++
		if calls%100 != 0 {
			return 0, nil
		}
		return data.Read(p[:1])
	})
	leaf, err := DecodeLeaf(r, "", Limits{})
	if err != nil || len(leaf.Records) != 1 || leaf.Records[0].Value != "value" {
		t.Fatalf("temporary empty reads: %v %v", leaf, err)
	}
}
