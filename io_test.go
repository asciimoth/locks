package locks

import (
	"io"
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
