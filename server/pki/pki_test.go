package pki

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func loadCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		t.Fatalf("no PEM in %s", path)
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEnsureCreatesVerifiableChain(t *testing.T) {
	dir := t.TempDir()
	if err := Ensure(dir, []string{"127.0.0.1", "kuma.local"}); err != nil {
		t.Fatal(err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, CAFile))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("ca.pem did not parse")
	}
	srv := loadCert(t, filepath.Join(dir, ServerCertFile))
	if _, err := srv.Verify(x509.VerifyOptions{Roots: pool, DNSName: "kuma.local"}); err != nil {
		t.Fatalf("server cert does not verify against CA: %v", err)
	}
	if len(srv.IPAddresses) != 1 || !srv.IPAddresses[0].Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("IP SAN missing: %v", srv.IPAddresses)
	}
	for _, f := range []string{caKeyFile, serverKeyFile} {
		info, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 0600", f, info.Mode().Perm())
		}
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := Ensure(dir, []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, CAFile))
	if err := Ensure(dir, []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, CAFile))
	if string(before) != string(after) {
		t.Fatal("second Ensure regenerated the CA")
	}
}

func TestReissueKeepsCAAndChangesSANs(t *testing.T) {
	dir := t.TempDir()
	if err := Ensure(dir, []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	caBefore, _ := os.ReadFile(filepath.Join(dir, CAFile))
	if err := Reissue(dir, []string{"10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	caAfter, _ := os.ReadFile(filepath.Join(dir, CAFile))
	if string(caBefore) != string(caAfter) {
		t.Fatal("Reissue changed the CA")
	}
	srv := loadCert(t, filepath.Join(dir, ServerCertFile))
	if len(srv.IPAddresses) != 1 || !srv.IPAddresses[0].Equal(net.ParseIP("10.0.0.1")) {
		t.Fatalf("new SAN not applied: %v", srv.IPAddresses)
	}
}

func TestServerCertExpiry(t *testing.T) {
	dir := t.TempDir()
	if err := Ensure(dir, []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	exp, err := ServerCertExpiry(dir)
	if err != nil {
		t.Fatal(err)
	}
	left := time.Until(exp)
	if left < 729*24*time.Hour || left > 731*24*time.Hour {
		t.Fatalf("expiry %v from now, want about 2 years", left)
	}
}

func TestTLSConfigLoads(t *testing.T) {
	dir := t.TempDir()
	if err := Ensure(dir, []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := TLSConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatal("no certificate loaded")
	}
}
