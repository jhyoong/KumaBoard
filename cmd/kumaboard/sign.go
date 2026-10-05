package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jhyoong/KumaBoard/internal/signing"
)

type manifest struct {
	Version   string     `json:"version"`
	Artifacts []artifact `json:"artifacts"`
}

type artifact struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	Signature string `json:"signature"`
}

func runSign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath, "path to config.yaml")
	version := fs.String("version", "", "release version to sign")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *version == "" {
		return fmt.Errorf("usage: kumaboard sign --version x.y.z [-config PATH]")
	}
	cfg, err := loadConfigFlag([]string{"-config", *cfgPath})
	if err != nil {
		return err
	}
	keyPath := filepath.Join(cfg.DataDir, "pki", "release_ed25519")
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("read signing key %s: %w (must run as root)", keyPath, err)
	}
	if len(keyBytes) != ed25519.PrivateKeySize {
		return fmt.Errorf("signing key is %d bytes, want %d", len(keyBytes), ed25519.PrivateKeySize)
	}
	priv := ed25519.PrivateKey(keyBytes)

	relDir := filepath.Join(cfg.DataDir, "releases", *version)
	mfPath := filepath.Join(relDir, "manifest.json")
	mfBytes, err := os.ReadFile(mfPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var mf manifest
	if err := json.Unmarshal(mfBytes, &mf); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if mf.Version != *version {
		return fmt.Errorf("manifest version %q does not match --version %q", mf.Version, *version)
	}
	for i := range mf.Artifacts {
		a := &mf.Artifacts[i]
		binName := "kuma-agent"
		if a.OS == "windows" {
			binName = "kuma-agent.exe"
		}
		binPath := filepath.Join(relDir, a.OS+"_"+a.Arch, binName)
		hash, err := fileSHA256(binPath)
		if err != nil {
			return fmt.Errorf("hash %s: %w", binPath, err)
		}
		if hash != a.SHA256 {
			return fmt.Errorf("sha256 mismatch for %s_%s: manifest %s, computed %s", a.OS, a.Arch, a.SHA256, hash)
		}
		msg := signing.BuildMessage(*version, a.OS, a.Arch, hash)
		sig := signing.Sign(priv, msg)
		a.Signature = base64.StdEncoding.EncodeToString(sig)
		fmt.Printf("signed %s_%s\n", a.OS, a.Arch)
	}
	out, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(mfPath, out, 0o644); err != nil {
		return err
	}
	fmt.Printf("manifest updated: %s\n", mfPath)
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
