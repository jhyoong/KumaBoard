package proto

import (
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Message types.
const (
	TypeHello              = "hello"
	TypeHelloAck           = "hello_ack"
	TypePing               = "ping"
	TypePong               = "pong"
	TypeMetrics            = "metrics"
	TypeCommandRequest     = "command_request"
	TypeCommandResult      = "command_result"
	TypeCommandOutput      = "command_output"
	TypeCommandCancel      = "command_cancel"
	TypeCommandsUpdate     = "commands_update"
	TypeTerminalOpen       = "terminal_open"
	TypeTerminalOpenResult = "terminal_open_result"
	TypeWolRequest         = "wol_request"
	TypeUpgradeRequest     = "upgrade_request"
	TypeUpgradeResult      = "upgrade_result"
	TypeError              = "error"
)

// Error codes carried by TypeError.
const (
	ErrAuthFailed                 = "auth_failed"
	ErrUnknownDevice              = "unknown_device"
	ErrProtocolVersionUnsupported = "protocol_version_unsupported"
	ErrDuplicateSession           = "duplicate_session"
	ErrProtocol                   = "protocol_error"
)

// Command run statuses. Stored in command_runs.status and sent in CommandResult.
const (
	RunRunning                = "running"
	RunOK                     = "ok"
	RunFailed                 = "failed"
	RunTimeout                = "timeout"
	RunUnknownCommand         = "unknown_command"
	RunBusy                   = "busy"
	RunDispatched             = "dispatched"
	RunDisconnectedAsExpected = "disconnected_as_expected"
	RunNoDisconnect           = "no_disconnect"
	RunLost                   = "lost"
	// RunCancelled: stopped by command_cancel. RunRefused: the agent's file
	// permission check failed immediately before exec. Both are v2.
	RunCancelled = "cancelled"
	RunRefused   = "refused"
)

// IsTerminalRunStatus reports whether a run status will not change again.
func IsTerminalRunStatus(s string) bool {
	return s != RunRunning && s != RunDispatched
}

// Capabilities an agent may declare.
const (
	CapMetrics        = "metrics"
	CapCustomCommands = "custom-commands"
	CapWolTarget      = "wol-target"
	CapWolSender      = "wol-sender"
	CapTerminal       = "terminal"
	// CapGPU adds per-adapter GPU samples to metrics. Effective only together
	// with CapMetrics, because the samples ride in the metrics message.
	CapGPU = "gpu"
)

// CommandDef is a command the agent declares at handshake. Untrusted input.
type CommandDef struct {
	Name             string `json:"name"`
	Description      string `json:"description"`
	TimeoutS         int    `json:"timeout_s"`
	ExpectDisconnect bool   `json:"expect_disconnect"`
	// Confirm asks the dashboard to confirm before starting. It guards
	// against misclicks only and is not a security control.
	Confirm bool `json:"confirm"`
}

// CommandProblem is a command present in the agent's config but not declared
// because the agent's own account could modify what it would execute.
// Untrusted input.
type CommandProblem struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Sanitize limits for agent-supplied command declarations and output.
const (
	MaxCommands       = 64
	MaxCommandTextLen = 256
	// MaxCommandOutput is how much of each output stream is retained: the
	// last 64 KiB, at the agent, the server and the dashboard.
	MaxCommandOutput = 64 << 10
)

// nameRe matches the device and command name rule enforced by the agent
// config and the dashboard API.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// SanitizeCommands bounds an untrusted command list: entries with a bad or
// repeated name are dropped, the list is capped at MaxCommands, descriptions
// are truncated and a negative timeout becomes 0.
func SanitizeCommands(in []CommandDef) []CommandDef {
	out := make([]CommandDef, 0, min(len(in), MaxCommands))
	seen := map[string]bool{}
	for _, c := range in {
		if len(out) == MaxCommands {
			break
		}
		if !nameRe.MatchString(c.Name) || seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		c.Description = truncateText(c.Description, MaxCommandTextLen)
		c.TimeoutS = max(c.TimeoutS, 0)
		out = append(out, c)
	}
	return out
}

// SanitizeProblems bounds an untrusted problem list the same way.
func SanitizeProblems(in []CommandProblem) []CommandProblem {
	out := make([]CommandProblem, 0, min(len(in), MaxCommands))
	seen := map[string]bool{}
	for _, p := range in {
		if len(out) == MaxCommands {
			break
		}
		if !nameRe.MatchString(p.Name) || seen[p.Name] {
			continue
		}
		seen[p.Name] = true
		p.Reason = truncateText(p.Reason, MaxCommandTextLen)
		out = append(out, p)
	}
	return out
}

// SanitizeConfigError bounds an untrusted config_error string.
func SanitizeConfigError(s string) string { return truncateText(s, MaxCommandTextLen) }

// Hello is the agent's first message.
type Hello struct {
	ProtocolVersion int          `json:"protocol_version"`
	AgentVersion    string       `json:"agent_version"`
	DeviceName      string       `json:"device_name"`
	Token           string       `json:"token"`
	OS              string       `json:"os"`
	Arch            string       `json:"arch"`
	Capabilities    []string     `json:"capabilities"`
	Commands        []CommandDef `json:"commands"`
	// Problems lists commands withheld from Commands. Absent from v1 agents.
	Problems []CommandProblem `json:"problems,omitempty"`
}

// Redacted returns a copy safe for logging and audit.
func (h Hello) Redacted() Hello {
	h.Token = ""
	return h
}

// HelloAck is the server's acceptance.
type HelloAck struct {
	SessionID          string    `json:"session_id"`
	ServerTime         time.Time `json:"server_time"`
	MetricsIntervalS   int       `json:"metrics_interval_s"`
	HeartbeatIntervalS int       `json:"heartbeat_interval_s"`
}

// Metrics is one sample from an agent.
type Metrics struct {
	CPUPercent      float64 `json:"cpu_percent"`
	MemTotalBytes   uint64  `json:"mem_total_bytes"`
	MemUsedBytes    uint64  `json:"mem_used_bytes"`
	MemUsedPercent  float64 `json:"mem_used_percent"`
	DiskTotalBytes  uint64  `json:"disk_total_bytes"`
	DiskUsedBytes   uint64  `json:"disk_used_bytes"`
	DiskUsedPercent float64 `json:"disk_used_percent"`
	UptimeS         uint64  `json:"uptime_s"`
	Load1           float64 `json:"load1"`
	Load5           float64 `json:"load5"`
	Load15          float64 `json:"load15"`
	GPUs            []GPU   `json:"gpus,omitempty"`
	// TempC is the host CPU/SoC temperature in °C; absent when the platform
	// has no usable sensor (never sent as 0).
	TempC *float64 `json:"temp_c,omitempty"`
	// TempSensor names the sensor TempC came from, e.g. "k10temp/Tctl".
	TempSensor string `json:"temp_sensor,omitempty"`
}

// GPU is one adapter's sample. Pointer fields are absent when the platform
// cannot report them (e.g. Apple unified memory has no VRAM total).
type GPU struct {
	Index         int      `json:"index"`
	Name          string   `json:"name"`
	Vendor        string   `json:"vendor"` // "apple","amd","intel","nvidia","unknown"
	UtilPercent   *float64 `json:"util_percent,omitempty"`
	MemUsedBytes  *uint64  `json:"mem_used_bytes,omitempty"`
	MemTotalBytes *uint64  `json:"mem_total_bytes,omitempty"`
	TempC         *float64 `json:"temp_c,omitempty"`
	PowerW        *float64 `json:"power_w,omitempty"`
	Suspended     bool     `json:"suspended,omitempty"`
}

// Sanitize limits for agent-supplied GPU samples.
const (
	MaxGPUs       = 8
	MaxGPUTextLen = 64
	// MaxTempSensorLen bounds Metrics.TempSensor, same as MaxGPUTextLen.
	MaxTempSensorLen = 64
)

// Sanitize bounds an untrusted sample in place: it caps the GPU list, truncates
// GPU text, clamps GPU utilisation to 0-100, and removes NaN/Inf, which would
// otherwise make re-encoding for the dashboard fail. A host temperature outside
// -50..200 °C is dropped, and the sensor label goes with it.
func (m *Metrics) Sanitize() {
	for _, f := range []*float64{&m.CPUPercent, &m.MemUsedPercent, &m.DiskUsedPercent, &m.Load1, &m.Load5, &m.Load15} {
		if !finite(*f) {
			*f = 0
		}
	}
	if len(m.GPUs) > MaxGPUs {
		m.GPUs = m.GPUs[:MaxGPUs]
	}
	for i := range m.GPUs {
		g := &m.GPUs[i]
		g.Name = truncateText(g.Name, MaxGPUTextLen)
		g.Vendor = truncateText(g.Vendor, MaxGPUTextLen)
		if g.UtilPercent = finitePtr(g.UtilPercent); g.UtilPercent != nil {
			v := min(max(*g.UtilPercent, 0), 100)
			g.UtilPercent = &v
		}
		g.TempC = finitePtr(g.TempC)
		g.PowerW = finitePtr(g.PowerW)
	}
	m.TempC = finitePtr(m.TempC)
	if m.TempC != nil && (*m.TempC < -50 || *m.TempC > 200) {
		m.TempC = nil
	}
	if m.TempC == nil {
		m.TempSensor = ""
	}
	m.TempSensor = truncateText(m.TempSensor, MaxTempSensorLen)
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func finitePtr(f *float64) *float64 {
	if f == nil || !finite(*f) {
		return nil
	}
	return f
}

// truncateText cuts s to at most n bytes without splitting a rune, dropping
// invalid UTF-8 first.
func truncateText(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// TailText keeps at most the last n bytes of s without splitting a rune,
// dropping invalid UTF-8 first. It is the front-trimming counterpart of
// truncateText.
func TailText(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

// CommandRequest names a command. Never a shell string, and never anything
// else: commands take no parameters, so this must stay a single field.
type CommandRequest struct {
	Name string `json:"name"`
}

// CommandResult reports the outcome of a run.
type CommandResult struct {
	Status     string `json:"status"`
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"duration_ms"`
}

// CommandOutput is a streamed chunk; the envelope ID is the run ID. Defined
// in v1 but only sent by v2 agents. Skipped means output before this chunk was
// dropped because more than MaxCommandOutput arrived within one flush.
type CommandOutput struct {
	Seq     int    `json:"seq"`
	Stream  string `json:"stream"` // "stdout" or "stderr"
	Data    string `json:"data"`
	Skipped bool   `json:"skipped,omitempty"`
}

// Output stream names carried by CommandOutput.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
)

// CommandsUpdate replaces the device's declared commands after the agent
// re-read its own config file. Sent by v2 agents only, never requested by the
// server. ConfigError is set when the file could not be parsed and the
// previous set was kept. command_cancel, the other v2 message, has no payload:
// its envelope ID is the run ID.
type CommandsUpdate struct {
	Commands    []CommandDef     `json:"commands"`
	Problems    []CommandProblem `json:"problems"`
	ConfigError string           `json:"config_error"`
}

// Error is a coded error, usually followed by a close.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WolRequest asks a wol-sender agent to emit a magic packet. Dormant in v1.
type WolRequest struct {
	MAC       string `json:"mac"`
	Broadcast string `json:"broadcast"`
}
