package collectors

import (
	"context"
	"testing"
)

func TestCollectReturnsRealValues(t *testing.T) {
	m, err := New(Options{}).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.MemTotalBytes == 0 || m.DiskTotalBytes == 0 || m.UptimeS == 0 {
		t.Fatalf("zero values in metrics: %+v", m)
	}
	if m.MemUsedPercent < 0 || m.MemUsedPercent > 100 || m.DiskUsedPercent < 0 || m.DiskUsedPercent > 100 {
		t.Fatalf("percent out of range: %+v", m)
	}
}
