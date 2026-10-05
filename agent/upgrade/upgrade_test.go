package upgrade

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jhyoong/KumaBoard/internal/signing"
	"github.com/jhyoong/KumaBoard/proto"
)

func testKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestVerifyArtifact(t *testing.T) {
	pub, priv := testKeys(t)
	data := []byte("fake-binary-content")
	h := sha256.Sum256(data)
	sha := hex.EncodeToString(h[:])
	msg := signing.BuildMessage("0.4.0", runtime.GOOS, runtime.GOARCH, sha)
	sig := base64.StdEncoding.EncodeToString(signing.Sign(priv, msg))

	u := &Upgrader{
		BinaryDir: t.TempDir(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		Keys:      []ed25519.PublicKey{pub},
	}

	tmp := filepath.Join(u.BinaryDir, "kuma-agent.new")
	os.WriteFile(tmp, data, 0o755)

	req := proto.UpgradeRequest{
		Version: "0.4.0", OS: runtime.GOOS, Arch: runtime.GOARCH,
		SHA256: sha, SizeBytes: int64(len(data)), Signature: sig,
	}
	if err := u.verifyArtifact(tmp, req); err != nil {
		t.Fatalf("valid artifact rejected: %v", err)
	}

	badReq := req
	badReq.SizeBytes = 999
	if err := u.verifyArtifact(tmp, badReq); err == nil {
		t.Fatal("wrong size accepted")
	}

	badReq = req
	badReq.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := u.verifyArtifact(tmp, badReq); err == nil {
		t.Fatal("wrong hash accepted")
	}

	badReq = req
	badReq.Signature = base64.StdEncoding.EncodeToString([]byte("bad-sig-that-is-not-64-bytes-but-will-fail-verify"))
	if err := u.verifyArtifact(tmp, badReq); err == nil {
		t.Fatal("wrong signature accepted")
	}

	badReq = req
	badReq.Version = "0.5.0"
	if err := u.verifyArtifact(tmp, badReq); err == nil {
		t.Fatal("version mismatch accepted")
	}
}

func TestDownload(t *testing.T) {
	data := []byte("binary-content-for-download-test")
	var gotAuth, gotDevice string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotDevice = r.Header.Get("X-Device-Name")
		w.Write(data)
	}))
	defer ts.Close()

	u := &Upgrader{
		BinaryDir:  t.TempDir(),
		DeviceName: "test-device",
		Token:      "secret-token",
		HTTPClient: ts.Client(),
	}

	path, err := u.download(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	got, _ := os.ReadFile(path)
	if string(got) != string(data) {
		t.Fatalf("downloaded content mismatch")
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotDevice != "test-device" {
		t.Fatalf("device = %q", gotDevice)
	}
}
