// Package creds provides the AES-256-GCM sealing used by the credential store.
// The master key never touches the database: it is derived at startup from
// LABDECK_MASTER_KEY, so a stolen labdeck.db alone yields no secrets.
package creds

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

// DeriveKey turns the operator's passphrase into a fixed 32-byte AES key.
func DeriveKey(passphrase string) []byte {
	if passphrase == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(passphrase))
	return sum[:]
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("master key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts plaintext, binding it to the credential id so a ciphertext
// cannot be silently swapped onto another credential row.
func Seal(key []byte, credID string, plaintext []byte) (nonce, ciphertext []byte, err error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	return nonce, gcm.Seal(nil, nonce, plaintext, []byte(credID)), nil
}

func Open(key []byte, credID string, nonce, ciphertext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, []byte(credID))
	if err != nil {
		return nil, fmt.Errorf("decrypt credential %s: wrong master key or corrupted data", credID)
	}
	return plaintext, nil
}
