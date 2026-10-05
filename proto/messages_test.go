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
