package signing

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func TestSignAndVerifyRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	msg := BuildMessage("0.4.0", "linux", "amd64", "abcdef1234567890")
	sig := Sign(priv, msg)
	if !Verify(pub, msg, sig) {
		t.Fatal("valid signature rejected")
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	msg := BuildMessage("0.4.0", "linux", "amd64", "abcdef")
	sig := Sign(priv, msg)
	if Verify(pub2, msg, sig) {
		t.Fatal("wrong key accepted")
	}
}

func TestVerifyRejectsTamperedMessage(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	msg := BuildMessage("0.4.0", "linux", "amd64", "abcdef")
	sig := Sign(priv, msg)
	tampered := BuildMessage("0.5.0", "linux", "amd64", "abcdef")
	if Verify(pub, tampered, sig) {
		t.Fatal("tampered message accepted")
	}
}

func TestVerifyEitherKey(t *testing.T) {
	pub1, priv1, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	msg := BuildMessage("0.4.0", "linux", "amd64", "abc")
	sig := Sign(priv1, msg)
	if !VerifyAny([]ed25519.PublicKey{pub1, pub2}, msg, sig) {
		t.Fatal("key1 rejected when in slot")
	}
	if !VerifyAny([]ed25519.PublicKey{pub2, pub1}, msg, sig) {
		t.Fatal("key1 rejected when second in slot")
	}
	pub3, _, _ := ed25519.GenerateKey(rand.Reader)
	if VerifyAny([]ed25519.PublicKey{pub2, pub3}, msg, sig) {
		t.Fatal("neither key should match")
	}
}

func TestParseHexKey(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	hexStr := hex.EncodeToString(pub)
	got, err := ParseHexPublicKey(hexStr)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(pub) {
		t.Fatal("parsed key mismatch")
	}
	if _, err := ParseHexPublicKey("short"); err == nil {
		t.Fatal("expected error for short key")
	}
	if _, err := ParseHexPublicKey(""); err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestBuildMessage(t *testing.T) {
	got := BuildMessage("0.4.0", "linux", "amd64", "abc123")
	want := "homelab-agent\n0.4.0\nlinux\namd64\nabc123\n"
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSignatureBase64RoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	msg := BuildMessage("1.0.0", "darwin", "arm64", "deadbeef")
	sig := Sign(priv, msg)
	b64 := base64.StdEncoding.EncodeToString(sig)
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	if !Verify(pub, msg, decoded) {
		t.Fatal("base64 round-trip broke signature")
	}
}
