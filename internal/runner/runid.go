package runner

import (
	"crypto/rand"
	"fmt"
	"time"
)

// NewRunID generates a UUIDv7 run identifier.
// UUIDv7 encodes the current time so run IDs sort chronologically.
func NewRunID() string {
	ms := uint64(time.Now().UnixMilli())

	var b [16]byte
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)

	if _, err := rand.Read(b[6:]); err != nil {
		panic(fmt.Sprintf("read crypto/rand for run ID: %v", err))
	}

	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80

	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
