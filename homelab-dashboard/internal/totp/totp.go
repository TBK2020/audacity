// Package totp implements RFC 6238 time-based one-time passwords (SHA-1,
// 6 digits, 30s step) — the flavor every authenticator app speaks.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

const (
	digits = 6
	step   = 30 * time.Second
)

// GenerateSecret returns a new random base32 secret (20 bytes, RFC 4226 size).
func GenerateSecret() (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf), nil
}

// URL renders the otpauth:// provisioning URI for authenticator apps.
func URL(secret, account string) string {
	return fmt.Sprintf("otpauth://totp/labdeck:%s?secret=%s&issuer=labdeck", account, secret)
}

func code(secret string, t time.Time) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).
		DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil {
		return "", fmt.Errorf("invalid base32 secret: %w", err)
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(t.Unix())/uint64(step.Seconds()))
	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%1_000_000), nil
}

// Verify checks a 6-digit code, allowing ±1 time step of clock drift.
func Verify(secret, input string, now time.Time) bool {
	input = strings.TrimSpace(input)
	if len(input) != digits {
		return false
	}
	ok := false
	for _, drift := range []time.Duration{0, -step, step} {
		expected, err := code(secret, now.Add(drift))
		if err != nil {
			return false
		}
		// Constant-time compare; no early exit so timing doesn't leak the slot.
		if subtle.ConstantTimeCompare([]byte(expected), []byte(input)) == 1 {
			ok = true
		}
	}
	return ok
}
