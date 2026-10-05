package proto

// Upgrade messages are frozen at protocol v1. Fields may be added; existing
// fields never change meaning and are never removed.

// UpgradeRequest asks the agent to move to a target version.
type UpgradeRequest struct {
	Version   string `json:"version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	Signature string `json:"signature"`
	URL       string `json:"url"`
}

// Upgrade states.
const (
	UpgradeDownloading = "downloading"
	UpgradeVerifying   = "verifying"
	UpgradeSelftest    = "selftest"
	UpgradeSwapped     = "swapped"
	UpgradeRestarting  = "restarting"
	UpgradeVerified    = "verified"
	UpgradeRolledBack  = "rolled_back"
	UpgradeFailed      = "failed"
)

// Upgrade failure reasons.
const (
	UpgradeReasonDownloadFailed = "download_failed"
	UpgradeReasonVerifyFailed   = "verify_failed"
	UpgradeReasonSelftestFailed = "selftest_failed"
	UpgradeReasonSwapFailed     = "swap_failed"
	UpgradeReasonNoHandshake    = "no_handshake"
)

// UpgradeResult reports progress or outcome of an upgrade.
type UpgradeResult struct {
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
	State       string `json:"state"`
	Reason      string `json:"reason,omitempty"`
}
