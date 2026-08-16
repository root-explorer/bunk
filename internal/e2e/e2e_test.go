package e2e

import (
	"bytes"
	"testing"
)

func TestSealOpenRoundtrip(t *testing.T) {
	alice, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	bob, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("hello from alice to bob over the blind relay")
	sealed, err := alice.Seal(bob.Public, msg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := bob.Open(alice.Public, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("roundtrip mismatch: %q", got)
	}
}

func TestWrongSenderFails(t *testing.T) {
	alice, _ := GenerateKeyPair()
	bob, _ := GenerateKeyPair()
	carol, _ := GenerateKeyPair()

	sealed, err := alice.Seal(bob.Public, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	// Bob tries to open a frame from carol: must fail (auth binds sender).
	if _, err := bob.Open(carol.Public, sealed); err == nil {
		t.Fatal("expected failure opening with wrong sender key")
	}
	// Carol (not alice) must not be able to read it either.
	if _, err := carol.Open(alice.Public, sealed); err == nil {
		t.Fatal("expected failure opening with wrong recipient key")
	}
}

func TestTamperedFrameFails(t *testing.T) {
	alice, _ := GenerateKeyPair()
	bob, _ := GenerateKeyPair()
	sealed, err := alice.Seal(bob.Public, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	sealed[len(sealed)-1] ^= 0xff
	if _, err := bob.Open(alice.Public, sealed); err == nil {
		t.Fatal("expected failure on tampered frame")
	}
}

func TestPublicKeyEncoding(t *testing.T) {
	kp, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	dec, err := DecodePublicB64(kp.PubB64)
	if err != nil {
		t.Fatal(err)
	}
	if *dec != *kp.Public {
		t.Fatal("b64 roundtrip mismatch")
	}
}

// TestLoadKeyRestart simulates a daemon restart: only the base64 strings
// survive serialization, so LoadKey must rebuild usable pointers.
func TestLoadKeyRestart(t *testing.T) {
	alice, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	bob, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	// "restart": bob's key is reloaded from its persisted b64 strings.
	reloaded, err := LoadKey(bob.PubB64, bob.PrivB64)
	if err != nil {
		t.Fatalf("LoadKey: %v", err)
	}
	if reloaded.Public == nil || reloaded.Private == nil {
		t.Fatal("LoadKey left derived pointers nil")
	}
	sealed, err := alice.Seal(reloaded.Public, []byte("after restart"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	msg, err := reloaded.Open(alice.Public, sealed)
	if err != nil || string(msg) != "after restart" {
		t.Fatalf("Open after restart failed: %v %q", err, msg)
	}
}
