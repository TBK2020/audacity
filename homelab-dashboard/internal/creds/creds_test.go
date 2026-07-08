package creds

import (
	"bytes"
	"testing"
)

func TestSealOpenRoundtrip(t *testing.T) {
	key := DeriveKey("test-passphrase")
	nonce, ct, err := Seal(key, "pve-root", []byte("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("s3cret")) {
		t.Fatal("ciphertext contains plaintext")
	}
	got, err := Open(key, "pve-root", nonce, ct)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "s3cret" {
		t.Fatalf("roundtrip = %q", got)
	}
}

func TestOpenRejectsWrongKey(t *testing.T) {
	nonce, ct, err := Seal(DeriveKey("right"), "id", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(DeriveKey("wrong"), "id", nonce, ct); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestOpenRejectsSwappedCredentialID(t *testing.T) {
	key := DeriveKey("k")
	nonce, ct, err := Seal(key, "cred-a", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(key, "cred-b", nonce, ct); err == nil {
		t.Fatal("ciphertext bound to cred-a was accepted for cred-b")
	}
}

func TestDeriveKeyEmpty(t *testing.T) {
	if DeriveKey("") != nil {
		t.Fatal("empty passphrase must yield nil key (gateway disabled)")
	}
}
