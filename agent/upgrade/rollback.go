package upgrade

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/jhyoong/KumaBoard/internal/buildinfo"
	"github.com/jhyoong/KumaBoard/proto"
)

type StartupState int

const (
	StateNormal StartupState = iota
	StateProbation
	StateRolledBack
)

type StartupResult struct {
	State          StartupState
	PendingMarker  string
	RollbackReason string
	FromVersion    string
	ToVersion      string
}

// agentBinaryName is the file name of the installed agent binary. RestoreOld
// on Windows leaves "<binary>.failed" behind, so startup cleanup must target
// kuma-agent.exe there, not the extensionless name.
func agentBinaryName(goos string) string {
	if goos == "windows" {
		return "kuma-agent.exe"
	}
	return "kuma-agent"
}

func CheckStartup(binaryDir string, log *slog.Logger) StartupResult {
	markerPath := filepath.Join(binaryDir, PendingFile)
	p, err := ReadPending(markerPath)
	if err != nil {
		log.Warn("upgrade: failed to read pending marker", "err", err)
		os.Remove(markerPath)
		return StartupResult{State: StateNormal}
	}
	if p == nil {
		CleanupFailed(filepath.Join(binaryDir, agentBinaryName(runtime.GOOS)))
		return StartupResult{State: StateNormal}
	}

	version := buildinfo.Version
	switch {
	case version == p.ToVersion:
		log.Info("upgrade: probation started", "from", p.FromVersion, "to", p.ToVersion)
		return StartupResult{
			State: StateProbation, PendingMarker: markerPath,
			FromVersion: p.FromVersion, ToVersion: p.ToVersion,
		}
	case version == p.FromVersion:
		log.Info("upgrade: running after rollback", "from", p.FromVersion, "to", p.ToVersion, "reason", p.RollbackReason)
		return StartupResult{
			State: StateRolledBack, PendingMarker: markerPath,
			RollbackReason: p.RollbackReason,
			FromVersion:    p.FromVersion, ToVersion: p.ToVersion,
		}
	default:
		log.Warn("upgrade: stale marker, deleting", "marker_to", p.ToVersion, "own_version", version)
		os.Remove(markerPath)
		return StartupResult{State: StateNormal}
	}
}

func ConfirmUpgrade(binaryDir, binaryPath string, log *slog.Logger) {
	os.Remove(filepath.Join(binaryDir, PendingFile))
	os.Remove(binaryPath + ".old")
	log.Info("upgrade: confirmed, marker and .old removed")
}

func Rollback(binaryDir, binaryPath, markerPath string, log *slog.Logger) {
	SetRollbackReason(markerPath, proto.UpgradeReasonNoHandshake)
	if err := RestoreOld(binaryPath); err != nil {
		log.Error("upgrade: rollback failed", "err", err)
	}
	log.Error("upgrade: rolling back, no handshake within timeout")
	os.Exit(1)
}

const ProbationTimeout = 120 * time.Second
