package decide

import (
	"crypto/rand"
	"fmt"
)

// newUUIDv4 returns a random UUID v4 in canonical 8-4-4-4-12 hex form.
// It panics on entropy exhaustion, which is fatal system-wide.
func newUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Errorf("decide: rand.Read: %w", err))
	}
	// RFC 4122 §4.4: version 4, variant 10.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}