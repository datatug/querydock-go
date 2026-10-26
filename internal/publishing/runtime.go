package publishing

import (
	"encoding/base64"
	"io"
	"time"
)

// SecureIDs reads 128 fresh random bits; use crypto/rand.Reader in composition.
// No route-reserved word can have this fixed 22-character encoding.
type SecureIDs struct{ Reader io.Reader }

func (s SecureIDs) NewID() (string, error) {
	var bits [16]byte
	if _, err := io.ReadFull(s.Reader, bits[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bits[:]), nil
}

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }
