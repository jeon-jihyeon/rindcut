package ledger

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Ids are private, so their layout is checked from inside the package
func TestNewID(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	a, b := newID(t0), newID(t0.Add(time.Millisecond))
	assert.Len(t, a, 26)
	assert.NotEqual(t, a, newID(t0), "random bits make ids unique within a millisecond")
	assert.Less(t, a, b, "ids sort by time")
	assert.Empty(t, strings.Trim(a, crockford), "Crockford base32 only")
}
