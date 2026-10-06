package proto

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHelloRedacted(t *testing.T) {
	h := Hello{DeviceName: "d", Token: "secret"}
	r := h.Redacted()
	if r.Token != "" || h.Token != "secret" || r.DeviceName != "d" {
		t.Fatalf("redaction wrong: %+v %+v", h, r)
	}
}

func TestHelloRoundTrip(t *testing.T) {
	in := Hello{
		ProtocolVersion: 1, AgentVersion: "0.1.0", DeviceName: "macos-desktop",
		Token: "t", OS: "darwin", Arch: "arm64",
		Capabilities: []string{"metrics"},
		Commands:     []CommandDef{{Name: "sleep", TimeoutS: 15, ExpectDisconnect: true}},
	}
	env, err := New(TypeHello, in)
	if err != nil {
		t.Fatal(err)
	}
	var out Hello
	if err := env.Unmarshal(&out); err != nil {
		t.Fatal(err)
	}
	if out.Commands[0].ExpectDisconnect != true || out.Capabilities[0] != "metrics" {
		t.Fatalf("round trip lost data: %+v", out)
	}
}

func TestRunStatusIsTerminal(t *testing.T) {
	if IsTerminalRunStatus(RunRunning) || IsTerminalRunStatus(RunDispatched) {
		t.Fatal("running and dispatched are not terminal")
	}
	if !IsTerminalRunStatus(RunOK) || !IsTerminalRunStatus(RunLost) {
		t.Fatal("ok and lost are terminal")
	}
}

func f64(v float64) *float64 { return &v }

func TestMetricsWithoutGPUsDecodes(t *testing.T) {
	// A v1 agent from before GPU support.
	old := []byte(`{"cpu_percent":12.5,"mem_total_bytes":100,"mem_used_bytes":50,"mem_used_percent":50,` +
		`"disk_total_bytes":10,"disk_used_bytes":5,"disk_used_percent":50,"uptime_s":60,"load1":1,"load5":1,"load15":1}`)
	var m Metrics
	if err := json.Unmarshal(old, &m); err != nil {
		t.Fatal(err)
	}
	if m.GPUs != nil || m.CPUPercent != 12.5 {
		t.Fatalf("got %+v", m)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "gpus") {
		t.Fatalf("absent gpus must stay absent on re-encode: %s", out)
	}
}

func TestMetricsWithGPUsDecodesIntoOldStruct(t *testing.T) {
	// The pre-GPU Metrics shape, as an old server decodes it.
	type oldMetrics struct {
		CPUPercent float64 `json:"cpu_percent"`
		UptimeS    uint64  `json:"uptime_s"`
	}
	vram := uint64(8 << 30)
	env, err := New(TypeMetrics, Metrics{CPUPercent: 3, UptimeS: 9, GPUs: []GPU{{Name: "RX 6600", Vendor: "amd", MemTotalBytes: &vram}}})
	if err != nil {
		t.Fatal(err)
	}
	var o oldMetrics
	if err := env.Unmarshal(&o); err != nil {
		t.Fatal(err)
	}
	if o.CPUPercent != 3 || o.UptimeS != 9 {
		t.Fatalf("got %+v", o)
	}
}

func TestGPUAbsentFieldsOmitted(t *testing.T) {
	b, err := json.Marshal(GPU{Index: 1, Name: "Apple M2 Max", Vendor: "apple", UtilPercent: f64(0)})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"index":1,"name":"Apple M2 Max","vendor":"apple","util_percent":0}`
	if string(b) != want {
		t.Fatalf("got %s want %s", b, want)
	}
}

func TestMetricsSanitize(t *testing.T) {
	m := Metrics{CPUPercent: math.NaN(), Load1: math.Inf(1)}
	for i := range 12 {
		m.GPUs = append(m.GPUs, GPU{Index: i, Name: strings.Repeat("é", 40), Vendor: "amd"})
	}
	m.GPUs[0].UtilPercent = f64(250)
	m.GPUs[1].UtilPercent = f64(-3)
	m.GPUs[2].UtilPercent = f64(math.NaN())
	m.GPUs[3].TempC = f64(math.Inf(-1))
	m.GPUs[4].PowerW = f64(math.NaN())
	m.GPUs[5].TempC = f64(61)
	m.Sanitize()

	if m.CPUPercent != 0 || m.Load1 != 0 {
		t.Fatalf("NaN/Inf not zeroed: %+v", m)
	}
	if len(m.GPUs) != MaxGPUs {
		t.Fatalf("gpus not capped: %d", len(m.GPUs))
	}
	if n := len(m.GPUs[0].Name); n > MaxGPUTextLen || !utf8.ValidString(m.GPUs[0].Name) {
		t.Fatalf("name not truncated cleanly: %d %q", n, m.GPUs[0].Name)
	}
	if *m.GPUs[0].UtilPercent != 100 || *m.GPUs[1].UtilPercent != 0 {
		t.Fatalf("util not clamped: %v %v", *m.GPUs[0].UtilPercent, *m.GPUs[1].UtilPercent)
	}
	if m.GPUs[2].UtilPercent != nil || m.GPUs[3].TempC != nil || m.GPUs[4].PowerW != nil {
		t.Fatal("non-finite values not dropped")
	}
	if m.GPUs[5].TempC == nil || *m.GPUs[5].TempC != 61 {
		t.Fatal("finite value lost")
	}
	if _, err := json.Marshal(m); err != nil {
		t.Fatalf("sanitized metrics must encode: %v", err)
	}
}

func TestMetricsWithoutTempDecodes(t *testing.T) {
	// A v1 agent from before temperature support.
	old := []byte(`{"cpu_percent":12.5,"mem_total_bytes":100,"mem_used_bytes":50,"mem_used_percent":50,` +
		`"disk_total_bytes":10,"disk_used_bytes":5,"disk_used_percent":50,"uptime_s":60,"load1":1,"load5":1,"load15":1}`)
	var m Metrics
	if err := json.Unmarshal(old, &m); err != nil {
		t.Fatal(err)
	}
	if m.TempC != nil || m.TempSensor != "" {
		t.Fatalf("got %+v", m)
	}
	m.Sanitize()
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "temp_c") || strings.Contains(string(out), "temp_sensor") {
		t.Fatalf("absent temp must stay absent on re-encode: %s", out)
	}
}

func TestMetricsWithTempDecodesIntoOldStruct(t *testing.T) {
	// The pre-temperature Metrics shape, as an old server decodes it.
	type oldMetrics struct {
		CPUPercent float64 `json:"cpu_percent"`
		UptimeS    uint64  `json:"uptime_s"`
		GPUs       []GPU   `json:"gpus,omitempty"`
	}
	env, err := New(TypeMetrics, Metrics{CPUPercent: 3, UptimeS: 9, TempC: f64(54), TempSensor: "k10temp/Tctl"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env.Payload), `"temp_c":54`) || !strings.Contains(string(env.Payload), `"temp_sensor":"k10temp/Tctl"`) {
		t.Fatalf("temp not encoded: %s", env.Payload)
	}
	var o oldMetrics
	if err := env.Unmarshal(&o); err != nil {
		t.Fatal(err)
	}
	if o.CPUPercent != 3 || o.UptimeS != 9 {
		t.Fatalf("got %+v", o)
	}
}

func TestMetricsSanitizeTemp(t *testing.T) {
	cases := []struct {
		name       string
		in         *float64
		sensor     string
		wantNil    bool
		wantSensor string
	}{
		{"nil", nil, "k10temp/Tctl", true, ""},
		{"nan", f64(math.NaN()), "x", true, ""},
		{"inf", f64(math.Inf(1)), "x", true, ""},
		{"too hot", f64(200.5), "x", true, ""},
		{"too cold", f64(-50.5), "x", true, ""},
		{"low edge", f64(-50), "x", false, "x"},
		{"high edge", f64(200), "x", false, "x"},
		{"zero kept", f64(0), "x", false, "x"},
		{"normal", f64(54), "k10temp/Tctl", false, "k10temp/Tctl"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := Metrics{TempC: c.in, TempSensor: c.sensor}
			m.Sanitize()
			if (m.TempC == nil) != c.wantNil {
				t.Fatalf("TempC = %v, want nil=%v", m.TempC, c.wantNil)
			}
			if m.TempSensor != c.wantSensor {
				t.Fatalf("TempSensor = %q, want %q", m.TempSensor, c.wantSensor)
			}
			if _, err := json.Marshal(m); err != nil {
				t.Fatal(err)
			}
		})
	}

	m := Metrics{TempC: f64(40), TempSensor: strings.Repeat("é", 40)}
	m.Sanitize()
	if n := len(m.TempSensor); n > MaxTempSensorLen || n == 0 || !utf8.ValidString(m.TempSensor) {
		t.Fatalf("sensor not truncated cleanly: %d %q", n, m.TempSensor)
	}
}

// R2: the dashboard never supplies arguments. A command_request is a name and
// nothing else, and command_cancel has no payload at all.
func TestCommandRequestShapeIsNameOnly(t *testing.T) {
	env, err := New(TypeCommandRequest, CommandRequest{Name: "backup"})
	if err != nil {
		t.Fatal(err)
	}
	if string(env.Payload) != `{"name":"backup"}` {
		t.Fatalf("command_request payload = %s", env.Payload)
	}
	// Extra fields from a hostile peer are not read.
	in := &Envelope{Type: TypeCommandRequest, Payload: json.RawMessage(`{"name":"backup","args":["-rf","/"],"run":["/bin/sh"]}`)}
	var req CommandRequest
	if err := in.Unmarshal(&req); err != nil {
		t.Fatal(err)
	}
	if req != (CommandRequest{Name: "backup"}) {
		t.Fatalf("got %+v", req)
	}
	cancel, err := New(TypeCommandCancel, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Encode(cancel)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "payload") {
		t.Fatalf("command_cancel must carry no payload: %s", b)
	}
}

func TestVersionWindow(t *testing.T) {
	if Version != 2 || MinSupported != 1 {
		t.Fatalf("version %d min %d", Version, MinSupported)
	}
	if !Supported(1) || !Supported(2) || Supported(0) || Supported(3) {
		t.Fatal("window must be exactly 1..2")
	}
}

func TestNewRunStatusesAreTerminal(t *testing.T) {
	if !IsTerminalRunStatus(RunCancelled) || !IsTerminalRunStatus(RunRefused) {
		t.Fatal("cancelled and refused are terminal")
	}
}

func TestHelloFromV1AgentDecodes(t *testing.T) {
	old := []byte(`{"protocol_version":1,"device_name":"d","commands":[{"name":"sleep","description":"","timeout_s":15,"expect_disconnect":true}]}`)
	var h Hello
	if err := json.Unmarshal(old, &h); err != nil {
		t.Fatal(err)
	}
	if h.Problems != nil || h.Commands[0].Confirm || !h.Commands[0].ExpectDisconnect {
		t.Fatalf("got %+v", h)
	}
}

func TestCommandsUpdateRoundTrip(t *testing.T) {
	in := CommandsUpdate{
		Commands:    []CommandDef{{Name: "backup", TimeoutS: 60, Confirm: true}},
		Problems:    []CommandProblem{{Name: "wipe", Reason: "/opt/x is writable by the agent account"}},
		ConfigError: "yaml: line 3",
	}
	env, err := New(TypeCommandsUpdate, in)
	if err != nil {
		t.Fatal(err)
	}
	var out CommandsUpdate
	if err := env.Unmarshal(&out); err != nil {
		t.Fatal(err)
	}
	if !out.Commands[0].Confirm || out.Problems[0] != in.Problems[0] || out.ConfigError != in.ConfigError {
		t.Fatalf("round trip lost data: %+v", out)
	}
}

func TestCommandOutputSkippedOmittedWhenFalse(t *testing.T) {
	b, _ := json.Marshal(CommandOutput{Seq: 1, Stream: StreamStdout, Data: "x"})
	if string(b) != `{"seq":1,"stream":"stdout","data":"x"}` {
		t.Fatalf("got %s", b)
	}
	b, _ = json.Marshal(CommandOutput{Seq: 2, Stream: StreamStderr, Skipped: true})
	if !strings.Contains(string(b), `"skipped":true`) {
		t.Fatalf("got %s", b)
	}
}

func TestSanitizeCommands(t *testing.T) {
	var in []CommandDef
	in = append(in,
		CommandDef{Name: "Bad Name", TimeoutS: 5},
		CommandDef{Name: "", TimeoutS: 5},
		CommandDef{Name: "dup", Description: strings.Repeat("é", 300), TimeoutS: -4},
		CommandDef{Name: "dup", Description: "second"},
	)
	for i := range 100 {
		in = append(in, CommandDef{Name: "c" + strings.Repeat("x", i%5) + "-" + string(rune('a'+i%26)) + string(rune('a'+i/26)), TimeoutS: 1})
	}
	out := SanitizeCommands(in)
	if len(out) != MaxCommands {
		t.Fatalf("not capped: %d", len(out))
	}
	if out[0].Name != "dup" || out[0].TimeoutS != 0 {
		t.Fatalf("first kept entry: %+v", out[0])
	}
	if n := len(out[0].Description); n > MaxCommandTextLen || n == 0 || !utf8.ValidString(out[0].Description) {
		t.Fatalf("description not truncated cleanly: %d", n)
	}
	seen := map[string]bool{}
	for _, c := range out {
		if seen[c.Name] {
			t.Fatalf("duplicate %q survived", c.Name)
		}
		seen[c.Name] = true
	}
	if got := SanitizeCommands(nil); got == nil || len(got) != 0 {
		t.Fatalf("nil input must give an empty list: %#v", got)
	}
}

func TestSanitizeProblemsAndConfigError(t *testing.T) {
	in := []CommandProblem{
		{Name: "../x", Reason: "r"},
		{Name: "ok", Reason: strings.Repeat("é", 300)},
		{Name: "ok", Reason: "again"},
	}
	for i := range 80 {
		in = append(in, CommandProblem{Name: "p" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Reason: "r"})
	}
	out := SanitizeProblems(in)
	if len(out) != MaxCommands || out[0].Name != "ok" {
		t.Fatalf("got %d entries, first %+v", len(out), out[0])
	}
	if n := len(out[0].Reason); n > MaxCommandTextLen || !utf8.ValidString(out[0].Reason) {
		t.Fatalf("reason not truncated cleanly: %d", n)
	}
	if e := SanitizeConfigError(strings.Repeat("é", 300) + "\xff"); len(e) > MaxCommandTextLen || !utf8.ValidString(e) {
		t.Fatalf("config error not bounded: %d", len(e))
	}
}

func TestTailText(t *testing.T) {
	if got := TailText("hello", 10); got != "hello" {
		t.Fatalf("short input changed: %q", got)
	}
	if got := TailText("0123456789", 4); got != "6789" {
		t.Fatalf("got %q", got)
	}
	// Cutting inside "é" (2 bytes) must drop the whole rune, not split it.
	if got := TailText("aéb", 2); got != "b" {
		t.Fatalf("got %q", got)
	}
	if got := TailText("a\xffb", 8); got != "ab" {
		t.Fatalf("invalid UTF-8 not dropped: %q", got)
	}
	big := strings.Repeat("é", MaxCommandOutput)
	if got := TailText(big, MaxCommandOutput); len(got) > MaxCommandOutput || !utf8.ValidString(got) {
		t.Fatalf("tail of %d bytes is %d", len(big), len(got))
	}
}
