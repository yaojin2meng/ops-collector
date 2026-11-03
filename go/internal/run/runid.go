package run

import (
	"crypto/rand"
	"encoding/hex"
)

func newRunID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
