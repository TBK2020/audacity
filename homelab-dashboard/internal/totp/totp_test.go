package totp

import (
	"testing"
	"time"
)

// RFC 6238 Appendix B test vectors (SHA-1, 8 digits truncated to our 6).
// Secret "12345678901234567890" = base32 GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ.
const rfcSecret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

func TestRFCVectors(t *testing.T) {
	vectors := map[int64]string{
		59:          "287082", // RFC value 94287082
		1111111109:  "081804",
		1234567890:  "005924",
		2000000000:  "279037",
		20000000000: "353130",
	}
	for unix, want := range vectors {
		got, err := code(rfcSecret, time.Unix(unix, 0))
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("T=%d: code = %s, want %s", unix, got, want)
		}
	}
}

func TestVerifyDriftWindow(t *testing.T) {
	now := time.Unix(1234567890, 0)
	cur, _ := code(rfcSecret, now)
	prev, _ := code(rfcSecret, now.Add(-30*time.Second))
	next, _ := code(rfcSecret, now.Add(30*time.Second))
	far, _ := code(rfcSecret, now.Add(90*time.Second))

	for _, ok := range []string{cur, prev, next} {
		if !Verify(rfcSecret, ok, now) {
			t.Errorf("code %s within ±1 step rejected", ok)
		}
	}
	if Verify(rfcSecret, far, now) {
		t.Error("code 3 steps away accepted")
	}
	if Verify(rfcSecret, "000000", now) && cur != "000000" && prev != "000000" && next != "000000" {
		t.Error("wrong code accepted")
	}
	if Verify(rfcSecret, "12345", now) {
		t.Error("5-digit input accepted")
	}
}

func TestGenerateSecretRoundtrip(t *testing.T) {
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	c, err := code(secret, now)
	if err != nil {
		t.Fatal(err)
	}
	if !Verify(secret, c, now) {
		t.Error("self-generated code rejected")
	}
}
