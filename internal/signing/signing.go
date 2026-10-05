package signing

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
)

func BuildMessage(version, os, arch, sha256Hex string) []byte {
	return []byte(fmt.Sprintf("homelab-agent\n%s\n%s\n%s\n%s\n", version, os, arch, sha256Hex))
}

func Sign(key ed25519.PrivateKey, message []byte) []byte {
	return ed25519.Sign(key, message)
}

func Verify(pub ed25519.PublicKey, message, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, message, sig)
}

func VerifyAny(keys []ed25519.PublicKey, message, sig []byte) bool {
	for _, k := range keys {
		if Verify(k, message, sig) {
			return true
		}
	}
	return false
}

func ParseHexPublicKey(s string) (ed25519.PublicKey, error) {
	if s == "" {
		return nil, errors.New("signing: empty key")
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("signing: decode hex: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("signing: key is %d bytes, want %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}
