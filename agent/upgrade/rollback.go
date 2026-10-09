package upgrade

import (
	"fmt"
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
	RollbackDetail string
	UpgradeID      string
	FromVersion    string
	ToVersion      string
}

// MaxProbationStarts is how many times a new binary may start without
// completing a handshake before it is rolled back as a crash loop. It covers
// a binary that dies before the probation timer can fire.
const MaxProbationStarts = 5

// exit is os.Exit, replaced in tests.
var exit = os.Exit

// agentBinaryName is the file name of the installed agent binary. RestoreOld
// on Windows leaves "<binary>.failed" behind, so startup cleanup must target
// kuma-agent.exe there, not the extensionless name.
func agentBinaryName(goos string) string {
	if goos == "windows" {
		return "kuma-agent.exe"
	}
	return "kuma-agent"
}

// startupBinaryPath is the binary CheckStartup acts on: the running
// executable when it lives in binaryDir, else the default name there.
func startupBinaryPath(binaryDir string) string {
	if exe, err := os.Executable(); err == nil && filepath.Dir(exe) == filepath.Clean(binaryDir) {
		return exe
	}
	return filepath.Join(binaryDir, agentBinaryName(runtime.GOOS))
}

func CheckStartup(binaryDir string, log *slog.Logger) StartupResult {
	return checkStartup(binaryDir, startupBinaryPath(binaryDir), log)
}

func checkStartup(binaryDir, binaryPath string, log *slog.Logger) StartupResult {
	markerPath := filepath.Join(binaryDir, PendingFile)
	p, err := ReadPending(markerPath)
	if err != nil {
		log.Warn("upgrade: failed to read pending marker", "err", err)
		os.Remove(markerPath)
		return StartupResult{State: StateNormal}
	}
	if p == nil {
		CleanupFailed(binaryPath)
		return StartupResult{State: StateNormal}
	}

	version := buildinfo.Version
	switch {
	case version == p.ToVersion:
		// Counted here, not from StartedAt: a device that sleeps or powers
		// off right after the swap must not roll back a good binary.
		p.Starts++
		if p.Starts > MaxProbationStarts {
			Rollback(binaryDir, binaryPath, markerPath, proto.UpgradeReasonCrashLoop,
				fmt.Sprintf("started %d times without completing a handshake", p.Starts), log)
			return StartupResult{State: StateNormal}
		}
		if err := WritePending(markerPath, p); err != nil {
			log.Warn("upgrade: failed to count probation start", "err", err)
		}
		log.Info("upgrade: probation started", "from", p.FromVersion, "to", p.ToVersion, "starts", p.Starts)
		return StartupResult{
			State: StateProbation, PendingMarker: markerPath,
			UpgradeID:   p.UpgradeID,
			FromVersion: p.FromVersion, ToVersion: p.ToVersion,
		}
	case version == p.FromVersion:
		log.Info("upgrade: running after rollback", "from", p.FromVersion, "to", p.ToVersion, "reason", p.RollbackReason)
		return StartupResult{
			State: StateRolledBack, PendingMarker: markerPath,
			RollbackReason: p.RollbackReason,
			RollbackDetail: p.RollbackDetail,
			UpgradeID:      p.UpgradeID,
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

// Rollback records reason and detail in the marker, puts the previous binary
// back and exits so the service manager starts it. The previous binary is
// restored even when the marker cannot be written; it then reports the
// rollback without a cause.
func Rollback(binaryDir, binaryPath, markerPath, reason, detail string, log *slog.Logger) {
	log.Error("upgrade: rolling back", "reason", reason, "detail", detail)
	restore(binaryPath, markerPath, reason, detail, log)
	exit(1)
}

// RollbackIfProbation rolls back a new binary that cannot start: if a marker
// exists and this binary is its to_version, the reason and detail are
// recorded and the previous binary is restored. It reports whether this
// binary was on probation. The caller is expected to exit; the service
// manager then starts the restored binary.
func RollbackIfProbation(binaryPath, reason, detail string, log *slog.Logger) bool {
	markerPath := filepath.Join(filepath.Dir(binaryPath), PendingFile)
	p, err := ReadPending(markerPath)
	if err != nil {
		log.Warn("upgrade: failed to read pending marker", "err", err)
		return false
	}
	if p == nil || p.ToVersion != buildinfo.Version {
		return false
	}
	log.Error("upgrade: rolling back, new binary failed to start", "reason", reason, "detail", detail)
	restore(binaryPath, markerPath, reason, detail, log)
	return true
}

// restore is the part of a rollback that does not exit.
func restore(binaryPath, markerPath, reason, detail string, log *slog.Logger) {
	if err := SetRollback(markerPath, reason, buildDetail("", detail)); err != nil {
		log.Error("upgrade: failed to record rollback cause in marker", "err", err)
	}
	if err := RestoreOld(binaryPath); err != nil {
		log.Error("upgrade: rollback failed", "err", err)
	}
}

const ProbationTimeout = 120 * time.Second
