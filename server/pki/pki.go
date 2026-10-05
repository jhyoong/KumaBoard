// Package pki generates and loads the self-signed CA and the server certificate.
// The CA is the only trust root agents use.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	CAFile         = "ca.pem"
	ServerCertFile = "server.pem"
	caKeyFile      = "ca.key"
	serverKeyFile  = "server.key"

	caValidity     = 10 * 365 * 24 * time.Hour
	serverValidity = 2 * 365 * 24 * time.Hour
)

// Ensure creates the CA if missing, then the server certificate if missing.
// sans are IPs or hostnames for the server certificate.
func Ensure(dir string, sans []string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if !exists(filepath.Join(dir, caKeyFile)) {
		if err := generateCA(dir); err != nil {
			return fmt.Errorf("pki: generate CA: %w", err)
		}
	}
	if !exists(filepath.Join(dir, serverKeyFile)) {
		return Reissue(dir, sans)
	}
	return nil
}

// Reissue writes a new server certificate signed by the existing CA.
func Reissue(dir string, sans []string) error {
	caCert, caKey, err := loadCA(dir)
	if err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "KumaBoard"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(serverValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, s := range sans {
		if ip := net.ParseIP(s); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, s)
		}
	}
	if len(tmpl.IPAddresses)+len(tmpl.DNSNames) == 0 {
		return errors.New("pki: at least one SAN is required")
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	return writePair(dir, ServerCertFile, serverKeyFile, der, key)
}

// TLSConfig loads the server certificate for use by an HTTPS listener.
func TLSConfig(dir string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, ServerCertFile), filepath.Join(dir, serverKeyFile))
	if err != nil {
		return nil, fmt.Errorf("pki: load server cert: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ServerCertExpiry returns the NotAfter of the current server certificate.
func ServerCertExpiry(dir string) (time.Time, error) {
	c, err := readCert(filepath.Join(dir, ServerCertFile))
	if err != nil {
		return time.Time{}, err
	}
	return c.NotAfter, nil
}

func generateCA(dir string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "KumaBoard CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	return writePair(dir, CAFile, caKeyFile, der, key)
}

func loadCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cert, err := readCert(filepath.Join(dir, CAFile))
	if err != nil {
		return nil, nil, err
	}
	kb, err := os.ReadFile(filepath.Join(dir, caKeyFile))
	if err != nil {
		return nil, nil, err
	}
	block, _ := pem.Decode(kb)
	if block == nil {
		return nil, nil, errors.New("pki: ca.key is not PEM")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func readCert(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("pki: %s is not PEM", path)
	}
	return x509.ParseCertificate(block.Bytes)
}

func writePair(dir, certName, keyName string, der []byte, key *ecdsa.PrivateKey) error {
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	if err := os.WriteFile(filepath.Join(dir, keyName), keyPEM, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, certName), certPEM, 0o644)
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		panic(err)
	}
	return n
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
