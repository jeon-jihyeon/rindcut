package check

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
)

func TestScriptDigest(t *testing.T) {
	t.Parallel()
	// Regular file read errors require OS faults beyond the public file API
	failure := errors.New("read failed")
	text := strings.Repeat("x", 1024)
	digest := sha256.Sum256([]byte(text))
	empty := sha256.Sum256(nil)
	type want struct {
		digest string
		ok     bool
	}
	tcs := []struct {
		name   string
		reader io.Reader
		want   want
	}{
		{name: "complete text", reader: strings.NewReader(text),
			want: want{digest: hex.EncodeToString(digest[:]), ok: true}},
		{name: "empty text", reader: strings.NewReader(""),
			want: want{digest: hex.EncodeToString(empty[:]), ok: true}},
		{name: "compiled program", reader: strings.NewReader("binary\x00program")},
		{name: "first read fails", reader: iotest.ErrReader(failure)},
		{name: "partial first read fails",
			reader: io.MultiReader(strings.NewReader("short"), iotest.ErrReader(failure))},
		{name: "later read fails",
			reader: io.MultiReader(strings.NewReader(text[:512]), iotest.ErrReader(failure))},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			digest, ok := scriptDigest(tc.reader)
			assert.Equal(t, tc.want, want{digest: digest, ok: ok})
		})
	}
}
