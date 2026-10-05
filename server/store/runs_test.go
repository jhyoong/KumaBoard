package store

import (
	"context"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
)

func TestRunLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _, _ := s.CreateDevice(ctx, "deb", "", false, Schedule{})
	r := &Run{ID: "run1", DeviceID: d.ID, Command: "uptime", RequestedBy: "admin", RequestedAt: time.Now(), Status: proto.RunRunning}
	if err := s.InsertRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	code := 0
	if err := s.FinishRun(ctx, "run1", proto.RunOK, &code, "out", "", false); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRun(ctx, "run1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != proto.RunOK || got.ExitCode == nil || *got.ExitCode != 0 || got.StdoutTail != "out" || got.FinishedAt == nil || got.DeviceName != "deb" {
		t.Fatalf("finish not recorded: %+v", got)
	}
}

func TestMarkLost(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d1, _, _ := s.CreateDevice(ctx, "a", "", false, Schedule{})
	d2, _, _ := s.CreateDevice(ctx, "b", "", false, Schedule{})
	now := time.Now()
	s.InsertRun(ctx, &Run{ID: "r1", DeviceID: d1.ID, Command: "x", RequestedBy: "u", RequestedAt: now, Status: proto.RunRunning})
	s.InsertRun(ctx, &Run{ID: "r2", DeviceID: d1.ID, Command: "x", RequestedBy: "u", RequestedAt: now, Status: proto.RunOK})
	s.InsertRun(ctx, &Run{ID: "r3", DeviceID: d2.ID, Command: "x", RequestedBy: "u", RequestedAt: now, Status: proto.RunDispatched})

	n, err := s.MarkDeviceRunsLost(ctx, d1.ID)
	if err != nil || n != 1 {
		t.Fatalf("MarkDeviceRunsLost n=%d err=%v", n, err)
	}
	r1, _ := s.GetRun(ctx, "r1")
	r3, _ := s.GetRun(ctx, "r3")
	if r1.Status != proto.RunLost || r3.Status != proto.RunDispatched {
		t.Fatalf("wrong rows marked: r1=%s r3=%s", r1.Status, r3.Status)
	}
	n, err = s.MarkAllInFlightLost(ctx)
	if err != nil || n != 1 {
		t.Fatalf("MarkAllInFlightLost n=%d err=%v", n, err)
	}
	r3, _ = s.GetRun(ctx, "r3")
	if r3.Status != proto.RunLost {
		t.Fatal("dispatched run not marked lost at startup")
	}
}

func TestListRunsNewestFirst(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _, _ := s.CreateDevice(ctx, "a", "", false, Schedule{})
	base := time.Now()
	for i := 0; i < 3; i++ {
		s.InsertRun(ctx, &Run{ID: string(rune('a' + i)), DeviceID: d.ID, Command: "x", RequestedBy: "u", RequestedAt: base.Add(time.Duration(i) * time.Second), Status: proto.RunOK})
	}
	runs, err := s.ListRuns(ctx, d.ID, 2)
	if err != nil || len(runs) != 2 || runs[0].ID != "c" {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
}
