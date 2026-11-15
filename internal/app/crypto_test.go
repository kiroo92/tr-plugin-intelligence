package app

import (
	"strings"
	"testing"
)

// TestCipherRoundTrip checks that a sealed credential opens with the same
// record identity and that the ciphertext never contains the plaintext.
func TestCipherRoundTrip(t *testing.T) {
	box, err := newCipherBox(strings.Repeat("m", masterKeyMinLength))
	if err != nil {
		t.Fatalf("newCipherBox: %v", err)
	}
	keyID, err := newKeyID()
	if err != nil {
		t.Fatalf("newKeyID: %v", err)
	}
	secret := "sk-live-abcdefghijklmnop"
	sealed, err := box.seal(secret, keyID)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if strings.Contains(sealed, secret) {
		t.Fatal("ciphertext contains the plaintext secret")
	}
	opened, err := box.open(sealed, keyID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if opened != secret {
		t.Fatalf("open returned %q, want the original secret", opened)
	}
}

// TestCipherBindsRecordIdentity checks that a payload moved to another record
// fails authentication instead of returning the wrong credential.
func TestCipherBindsRecordIdentity(t *testing.T) {
	box, err := newCipherBox(strings.Repeat("m", masterKeyMinLength))
	if err != nil {
		t.Fatalf("newCipherBox: %v", err)
	}
	sealed, err := box.seal("sk-one", "record-a")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := box.open(sealed, "record-b"); err == nil {
		t.Fatal("open accepted a payload sealed for another record")
	}
}

// TestCipherRejectsShortMasterKey checks that a missing or short secret refuses
// work rather than falling back to plaintext.
func TestCipherRejectsShortMasterKey(t *testing.T) {
	for _, value := range []string{"", "short", strings.Repeat("m", masterKeyMinLength-1)} {
		if _, err := newCipherBox(value); err == nil {
			t.Fatalf("newCipherBox accepted %d characters", len(value))
		}
	}
}

// TestCipherRejectsCorruptPayload checks that malformed input is reported
// instead of panicking.
func TestCipherRejectsCorruptPayload(t *testing.T) {
	box, err := newCipherBox(strings.Repeat("m", masterKeyMinLength))
	if err != nil {
		t.Fatalf("newCipherBox: %v", err)
	}
	for _, payload := range []string{"", "not-base64!!", "AAAA"} {
		if _, err := box.open(payload, "record"); err == nil {
			t.Fatalf("open accepted corrupt payload %q", payload)
		}
	}
}
