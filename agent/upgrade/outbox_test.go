package upgrade

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jhyoong/KumaBoard/proto"
)

func TestOutboxWriteFlushDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, OutboxFile)
	res := proto.UpgradeResult{
		FromVersion: "0.3.0", ToVersion: "0.4.0", State: proto.UpgradeFailed,
		Reason: proto.UpgradeReasonDownloadFailed, UpgradeID: "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		Detail: "GET /x: status 404",
	}

	// Nothing stored: nothing sent.
	sent, err := FlushOutbox(dir, func(proto.UpgradeResult) error {
		t.Fatal("send called with an empty outbox")
		return nil
	})
	if sent || err != nil {
		t.Fatalf("empty flush = %v, %v", sent, err)
	}

	if err := WriteOutbox(dir, res); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadOutbox(dir); err != nil || got == nil || *got != res {
		t.Fatalf("ReadOutbox = %+v, %v", got, err)
	}

	// A failed send keeps the file for the next connect.
	down := errors.New("socket down")
	if sent, err := FlushOutbox(dir, func(proto.UpgradeResult) error { return down }); sent || !errors.Is(err, down) {
		t.Fatalf("flush with a dead socket = %v, %v", sent, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("outbox dropped after a failed send: %v", err)
	}

	var got []proto.UpgradeResult
	keep := func(r proto.UpgradeResult) error { got = append(got, r); return nil }
	if sent, err := FlushOutbox(dir, keep); !sent || err != nil {
		t.Fatalf("flush = %v, %v", sent, err)
	}
	if len(got) != 1 || got[0] != res {
		t.Fatalf("sent %+v, want %+v", got, res)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("outbox not deleted after delivery: %v", err)
	}

	// Delivered once only.
	if sent, _ := FlushOutbox(dir, keep); sent || len(got) != 1 {
		t.Fatalf("result delivered twice")
	}
}

func TestOutboxUnreadableFileIsDropped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, OutboxFile)
	os.WriteFile(path, []byte("{not json"), 0o600)
	sent, err := FlushOutbox(dir, func(proto.UpgradeResult) error {
		t.Fatal("send called for an undecodable file")
		return nil
	})
	if sent || err == nil {
		t.Fatalf("flush = %v, %v, want an error", sent, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("undecodable outbox kept: %v", err)
	}
}

func TestOutboxToleratesUnknownFields(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, OutboxFile),
		[]byte(`{"from_version":"a","to_version":"b","state":"failed","reason":"busy","later_field":1}`), 0o600)
	got, err := ReadOutbox(dir)
	if err != nil || got == nil || got.State != proto.UpgradeFailed || got.Reason != proto.UpgradeReasonBusy {
		t.Fatalf("ReadOutbox = %+v, %v", got, err)
	}
}
