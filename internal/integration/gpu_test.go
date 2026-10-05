package integration

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/registry"
)

// registryTee feeds hub events to the recorder and metrics to the dashboard
// registry, which backs GET /api/devices/{name}/metrics and the SSE stream.
type registryTee struct {
	*recorder
	reg *registry.Registry
}

func (t registryTee) MetricsReceived(name string, m proto.Metrics) {
	t.recorder.MetricsReceived(name, m)
	t.reg.MetricsReceived(name, m)
}

// feedRegistry restarts the server with the registry wired to the hub, as in
// production.
func (h *harness) feedRegistry() {
	h.t.Helper()
	h.stopServer()
	h.hubOpt.Events = registryTee{recorder: h.events, reg: h.registry}
	h.startServer()
}

// sseMetrics streams the data lines of "metrics" events from /api/events.
func (h *harness) sseMetrics(ctx context.Context, cookie *http.Cookie) <-chan string {
	h.t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.srv.URL+"/api/events", nil)
	if err != nil {
		h.t.Fatal(err)
	}
	req.AddCookie(cookie)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		h.t.Fatalf("GET /api/events: %d", resp.StatusCode)
	}
	ch := make(chan string, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), proto.MaxMessageSize)
		event := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && event == "metrics":
				select {
				case ch <- strings.TrimPrefix(line, "data: "):
				default:
				}
			case line == "":
				event = ""
			}
		}
	}()
	return ch
}

// nextSSEMetrics waits for a metrics event matching want.
func nextSSEMetrics(t *testing.T, ch <-chan string, want func(string) bool) string {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case data, ok := <-ch:
			if !ok {
				t.Fatal("SSE stream ended")
			}
			if want(data) {
				return data
			}
		case <-timeout:
			t.Fatal("no matching SSE metrics event")
		}
	}
}

// apiMetrics returns the raw body of GET /api/devices/{name}/metrics.
func (h *harness) apiMetrics(cookie *http.Cookie, name string) []byte {
	h.t.Helper()
	return h.apiGet(cookie, "/api/devices/"+name+"/metrics")
}

// apiGet returns the raw body of an authenticated GET, failing on non-200.
func (h *harness) apiGet(cookie *http.Cookie, path string) []byte {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+path, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	req.AddCookie(cookie)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("GET %s: %d %s", path, resp.StatusCode, body)
	}
	return body
}

// oldAgentMetrics is a metrics payload exactly as a pre-GPU v1 agent sends it.
const oldAgentMetrics = `{"cpu_percent":12.5,"mem_total_bytes":8589934592,"mem_used_bytes":4294967296,` +
	`"mem_used_percent":50,"disk_total_bytes":107374182400,"disk_used_bytes":53687091200,` +
	`"disk_used_percent":50,"uptime_s":3600,"load1":0.5,"load5":0.4,"load15":0.3}`

// TestOldAgentMetricsOnNewServer speaks the v1 wire protocol by hand, as an
// agent built before GPU and temperature support, and checks the new server
// accepts its metrics and re-publishes and stores them with no gpus or
// temp_c/temp_sensor keys at all.
func TestOldAgentMetricsOnNewServer(t *testing.T) {
	h := newHarness(t)
	h.feedRegistry()
	token := h.registerDevice("old")
	cookie := h.loginCookie()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events := h.sseMetrics(ctx, cookie)

	conn, _, err := websocket.Dial(ctx, "wss://"+h.addr+"/ws", &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs: h.agentConfig("old", token).CAPool, MinVersion: tls.VersionTLS12,
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	write := func(env *proto.Envelope) {
		b, err := proto.Encode(env)
		if err != nil {
			t.Error(err)
			return
		}
		conn.Write(ctx, websocket.MessageText, b)
	}
	hello, _ := proto.New(proto.TypeHello, proto.Hello{
		ProtocolVersion: 1, AgentVersion: "0.1.0", DeviceName: "old", Token: token,
		OS: "linux", Arch: "amd64", Capabilities: []string{proto.CapMetrics}, Commands: []proto.CommandDef{},
	})
	write(hello)
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ack, err := proto.Decode(data); err != nil || ack.Type != proto.TypeHelloAck {
		t.Fatalf("expected hello_ack, got %s", data)
	}
	// Answer heartbeats so the session stays up.
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if env, err := proto.Decode(data); err == nil && env.Type == proto.TypePing {
				pong, _ := proto.Reply(env, proto.TypePong, nil)
				write(pong)
			}
		}
	}()
	// Keep sending until the SSE subscriber sees one; the stream may attach
	// after the first sample.
	go func() {
		for ctx.Err() == nil {
			env, _ := proto.New(proto.TypeMetrics, nil)
			env.Payload = json.RawMessage(oldAgentMetrics)
			write(env)
			time.Sleep(100 * time.Millisecond)
		}
	}()

	data2 := nextSSEMetrics(t, events, func(d string) bool { return strings.Contains(d, `"device":"old"`) })
	if strings.Contains(data2, "gpus") || strings.Contains(data2, "temp_") {
		t.Fatalf("SSE metrics from an old agent must not carry gpus or temp: %s", data2)
	}
	var ev registry.MetricsEvent
	if err := json.Unmarshal([]byte(data2), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Metrics.CPUPercent != 12.5 || ev.Metrics.UptimeS != 3600 || ev.Metrics.GPUs != nil || ev.Metrics.TempC != nil {
		t.Fatalf("SSE metrics = %+v", ev.Metrics)
	}

	body := h.apiMetrics(cookie, "old")
	if strings.Contains(string(body), "gpus") || strings.Contains(string(body), "temp_") {
		t.Fatalf("API metrics from an old agent must not carry gpus or temp: %s", body)
	}
	var hist []proto.Metrics
	if err := json.Unmarshal(body, &hist); err != nil {
		t.Fatal(err)
	}
	if len(hist) == 0 || hist[len(hist)-1].CPUPercent != 12.5 {
		t.Fatalf("API metrics = %s", body)
	}
	if list := h.apiGet(cookie, "/api/devices"); strings.Contains(string(list), "temp_") {
		t.Fatalf("device list for an old agent must not carry temp: %s", list)
	}
	if hb := h.apiGet(cookie, "/api/devices/old/metrics/history?window=1h"); !strings.Contains(string(hb), `"cpu":12.5`) ||
		strings.Contains(string(hb), "temp_c") {
		t.Fatalf("history for an old agent = %s", hb)
	}
}

// TestAgentWithoutGPUCapabilitySendsNoGPUs checks the gpu capability gate: a
// new agent with only "metrics" sends the pre-GPU payload shape, even on a
// host that has a GPU.
func TestAgentWithoutGPUCapabilitySendsNoGPUs(t *testing.T) {
	h := newHarness(t)
	h.feedRegistry()
	token := h.registerDevice("plain")
	cookie := h.loginCookie()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events := h.sseMetrics(ctx, cookie)
	h.startAgent(h.agentConfig("plain", token), nil)
	data := nextSSEMetrics(t, events, func(d string) bool { return strings.Contains(d, `"device":"plain"`) })
	if strings.Contains(data, "gpus") {
		t.Fatalf("gpus sent without the gpu capability: %s", data)
	}
}
