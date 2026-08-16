// Package e2e provides the end-to-end encryption used between bunk peers.
//
// It uses nacl/box (X25519 + XSalsa20-Poly1305) with long-lived static
// keys exchanged during pairing. The hub relays only sealed frames, so it
// can never read traffic — even if the hub operator is malicious, they
// cannot forge messages because sealing binds the sender's key.
//
// Note: static ECDH has no forward secrecy. Ephemeral per-channel keys
// (and/or WireGuard-style rekeying) are a planned v2 improvement; the
// threat model here is "hub is blind and cannot forge", not "perfect
// forward secrecy".
package e2e

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"golang.org/x/crypto/nacl/box"
)

// KeyPair is a long-lived X25519 keypair for one machine.
type KeyPair struct {
	Public  *[32]byte `json:"-"`
	Private *[32]byte `json:"-"`
	PubB64  string    `json:"public"`
	PrivB64 string    `json:"private"`
}

// GenerateKeyPair creates a fresh keypair and fills in the b64 fields.
func GenerateKeyPair() (*KeyPair, error) {
	pub, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate keypair: %w", err)
	}
	return &KeyPair{
		Public:  pub,
		Private: priv,
		PubB64:  base64.StdEncoding.EncodeToString(pub[:]),
		PrivB64: base64.StdEncoding.EncodeToString(priv[:]),
	}, nil
}

// DecodePublicB64 parses a peer's public key.
func DecodePublicB64(s string) (*[32]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode pubkey: %w", err)
	}
	if len(raw) != 32 {
		return nil, errors.New("pubkey must be 32 bytes")
	}
	var k [32]byte
	copy(k[:], raw)
	return &k, nil
}

// LoadKey rebuilds a KeyPair (derived pointers included) from persisted
// base64 strings. Public/Private are json:"-" so they are nil after
// unmarshal; every load must go through here or sealing will panic.
func LoadKey(pubB64, privB64 string) (*KeyPair, error) {
	pub, err := DecodePublicB64(pubB64)
	if err != nil {
		return nil, err
	}
	privRaw, err := base64.StdEncoding.DecodeString(privB64)
	if err != nil {
		return nil, fmt.Errorf("decode privkey: %w", err)
	}
	if len(privRaw) != 32 {
		return nil, errors.New("privkey must be 32 bytes")
	}
	var priv [32]byte
	copy(priv[:], privRaw)
	return &KeyPair{Public: pub, Private: &priv, PubB64: pubB64, PrivB64: privB64}, nil
}

// Seal encrypts and authenticates payload for recipientPub. The returned
// slice is nonce || ciphertext.
func (kp *KeyPair) Seal(recipientPub *[32]byte, payload []byte) ([]byte, error) {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	return box.Seal(nonce[:], payload, &nonce, recipientPub, kp.Private), nil
}

// Open decrypts and authenticates a frame supposedly from senderPub.
// It fails (nil, err) if the frame was not sealed by senderPub's holder.
func (kp *KeyPair) Open(senderPub *[32]byte, sealed []byte) ([]byte, error) {
	if len(sealed) < 24+box.Overhead {
		return nil, errors.New("frame too short")
	}
	var nonce [24]byte
	copy(nonce[:], sealed[:24])
	out, ok := box.Open(nil, sealed[24:], &nonce, senderPub, kp.Private)
	if !ok {
		return nil, errors.New("decrypt/authenticate failed")
	}
	return out, nil
}
