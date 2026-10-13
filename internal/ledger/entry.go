// Package ledger is the append-only record everything in rindcut is read from
package ledger

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"time"
)

type Kind string

const (
	KindJob        Kind = "job"
	KindSession    Kind = "session"
	KindProbe      Kind = "probe" // a dry run, never counted as a verdict
	KindVerdict    Kind = "verdict"
	KindCorrection Kind = "correction"
)

// Entry is one line of the ledger
type Entry struct {
	ID      string          `json:"id"`
	Kind    Kind            `json:"kind"`
	At      time.Time       `json:"at"`
	Version string          `json:"version"`
	Data    json.RawMessage `json:"data"`
}

func (e Entry) Decode(v any) error { return json.Unmarshal(e.Data, v) }

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newID is 48 bits of milliseconds then 80 random bits in Crockford base32, the ULID layout
// Ids sort by time to the millisecond, while file order stays the true order
func newID(t time.Time) string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(t.UnixMilli())<<16)
	_, _ = rand.Read(b[6:])
	var out [26]byte
	// 128 bits read 5 at a time from the top, padded with two zero bits in front
	hi, lo := binary.BigEndian.Uint64(b[:8]), binary.BigEndian.Uint64(b[8:])
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}
