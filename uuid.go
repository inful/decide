package decide

import (
	"crypto/rand"
	"fmt"
)

// newUUIDv4 returns a random UUID v4 in canonical 8-4-4-4-12 hex form.
//
// crypto/rand.Read never returns an error: since Go 1.24 it panics itself
// if the system entropy source fails, which is fatal system-wide.
func newUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	// RFC 4122 §4.4: version 4, variant 10.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
