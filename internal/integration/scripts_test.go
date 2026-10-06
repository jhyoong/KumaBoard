package integration

import (
	"bufio"
	"context"
	"crypto/tls"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/hub"
	"github.com/jhyoong/KumaBoard/server/registry"
)

// outputFor returns the run_output chunks recorded for a run, in order.
func (r *recorder) outputFor(runID string) []proto.CommandOutput {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []proto.CommandOutput
	for _, o := range r.output {
		if o.runID == runID {
			out = append(out, o.o)
		}
	}
	return out
}

func joinStream(chunks []proto.CommandOutput, stream string) string {
	var b strings.Builder
	for _, c := range chunks {
		if c.Stream == stream {
			b.WriteString(c.Data)
		}
	}
	return b.String()
}

// rawAgent is a hand-driven control session: no real agent, so a test can
// send whatever a hostile or old one might.
type rawAgent struct {
	t    *testing.T
	ctx  context.Context
	conn *websocket.Conn

	mu   sync.Mutex
	seen []*proto.Envelope // everything the server sent except pings
}

// startRawAgent connects, completes the handshake with the given hello and
// then answers pings.
func (h *harness) startRawAgent(hello proto.Hello) *rawAgent {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, "wss://"+h.addr+"/ws", &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs: h.agentConfig(hello.DeviceName, hello.Token).CAPool, MinVersion: tls.VersionTLS12,
		}}},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { conn.CloseNow() })
	conn.SetReadLimit(proto.MaxMessageSize)
	ra := &rawAgent{t: h.t, ctx: ctx, conn: conn}
	env, _ := proto.New(proto.TypeHello, hello)
	ra.send(env)
	_, data, err := conn.Read(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	if ack, err := proto.Decode(data); err != nil || ack.Type != proto.TypeHelloAck {
		h.t.Fatalf("expected hello_ack, got %s", data)
	}
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			env, err := proto.Decode(data)
			if err != nil {
				continue
			}
			if env.Type == proto.TypePing {
				pong, _ := proto.Reply(env, proto.TypePong, nil)
				ra.send(pong)
				continue
			}
			ra.mu.Lock()
			ra.seen = append(ra.seen, env)
			ra.mu.Unlock()
		}
	}()
	return ra
}

func (ra *rawAgent) send(env *proto.Envelope) {
	ra.t.Helper()
	b, err := proto.Encode(env)
	if err != nil {
		ra.t.Error(err)
		return
	}
	ra.conn.Write(ra.ctx, websocket.MessageText, b)
}

// reply sends a message whose envelope ID is the run ID.
func (ra *rawAgent) reply(runID, typ string, payload any) {
	ra.t.Helper()
	env, err := proto.New(typ, payload)
	if err != nil {
		ra.t.Fatal(err)
	}
	env.ID = runID
	ra.send(env)
}

func (ra *rawAgent) received(typ string) []*proto.Envelope {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	var out []*proto.Envelope
	for _, e := range ra.seen {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// scriptsTee feeds hub events to the recorder and, for the events the
// dashboard shows, to the registry as in production.
type scriptsTee struct {
	*recorder
	reg *registry.Registry
}

func (t scriptsTee) RunChanged(runID string) {
	t.recorder.RunChanged(runID)
	t.reg.RunChanged(runID)
}

func (t scriptsTee) RunOutput(runID, device string, seq int, o proto.CommandOutput) {
	t.recorder.RunOutput(runID, device, seq, o)
	t.reg.RunOutput(runID, device, seq, o)
}

func (t scriptsTee) CommandsChanged(name string) {
	t.recorder.CommandsChanged(name)
	t.reg.CommandsChanged(name)
}

type sseEvent struct{ name, data string }

// sseEvents streams every named event from /api/events.
func (h *harness) sseEvents(ctx context.Context, cookie *http.Cookie) <-chan sseEvent {
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
	ch := make(chan sseEvent, 256)
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
			case strings.HasPrefix(line, "data: "):
				select {
				case ch <- sseEvent{event, strings.TrimPrefix(line, "data: ")}:
				default:
				}
			case line == "":
				event = ""
			}
		}
	}()
	return ch
}

func nextSSE(t *testing.T, ch <-chan sseEvent, name string, want func(data string) bool) string {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("SSE stream ended")
			}
			if ev.name == name && want(ev.data) {
				return ev.data
			}
		case <-timeout:
			t.Fatalf("no matching SSE %s event", name)
		}
	}
}

// The agent is untrusted: a session that sends oversized or endless
// command_output, with no real agent behind it, never grows the server's
// in-flight buffer past 64 KiB per stream, and neither does its result.
func TestHostileOutputIsBounded(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("evil")
	ra := h.startRawAgent(proto.Hello{
		ProtocolVersion: proto.Version, AgentVersion: "9.9.9", DeviceName: "evil", Token: token,
		OS: "linux", Arch: "amd64", Commands: []proto.CommandDef{{Name: "x", TimeoutS: 60}},
	})
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("evil") })
	id, err := h.hub.RequestCommand(context.Background(), "evil", "x", "test")
	if err != nil {
		t.Fatal(err)
	}
	live := func() (string, string, bool, int) {
		stdout, stderr, truncated, seq, ok := h.hub.LiveOutput(id)
		if !ok {
			t.Fatal("run is not in flight")
		}
		return stdout, stderr, truncated, seq
	}

	// One chunk far over the cap, ending in a marker.
	ra.reply(id, proto.TypeCommandOutput, proto.CommandOutput{Seq: 1, Stream: proto.StreamStdout,
		Data: strings.Repeat("a", 900<<10) + "MARK-1\n"})
	waitFor(t, 3*time.Second, func() bool { _, _, _, seq := live(); return seq == 1 })
	stdout, _, truncated, _ := live()
	if len(stdout) != proto.MaxCommandOutput || !strings.HasSuffix(stdout, "MARK-1\n") || !truncated {
		t.Fatalf("after oversized chunk: %d bytes, truncated %v", len(stdout), truncated)
	}

	// Then a stream that never stops: 100 chunks of 4 KiB on each stream,
	// six times the cap. (Kept modest so heartbeats are not starved under
	// the race detector.) Multi-byte text on stderr checks the cut never
	// splits a character.
	for i := range 100 {
		ra.reply(id, proto.TypeCommandOutput, proto.CommandOutput{Seq: i, Stream: proto.StreamStdout, Data: strings.Repeat("b", 4<<10)})
		ra.reply(id, proto.TypeCommandOutput, proto.CommandOutput{Seq: i, Stream: proto.StreamStderr, Data: strings.Repeat("é", 2<<10)})
	}
	// Streams the server does not know are dropped, not stored under a new key.
	ra.reply(id, proto.TypeCommandOutput, proto.CommandOutput{Stream: "stdlog", Data: "nope"})
	ra.reply(id, proto.TypeCommandOutput, proto.CommandOutput{Stream: proto.StreamStdout, Data: "MARK-2\n"})
	waitFor(t, 5*time.Second, func() bool { stdout, _, _, _ := live(); return strings.HasSuffix(stdout, "MARK-2\n") })
	stdout, stderr, truncated, seq := live()
	if len(stdout) > proto.MaxCommandOutput || len(stderr) > proto.MaxCommandOutput || len(stderr) < proto.MaxCommandOutput-4 || !truncated {
		t.Fatalf("buffers: stdout %d stderr %d truncated %v", len(stdout), len(stderr), truncated)
	}
	if strings.Contains(stdout, "nope") || strings.ContainsRune(stderr, '�') || seq != 202 {
		t.Fatalf("seq %d, stderr valid %v", seq, !strings.ContainsRune(stderr, '�'))
	}
	// What goes on to the dashboard is bounded per event too, and the
	// oversized chunk is marked as having lost its start.
	chunks := h.events.outputFor(id)
	if len(chunks) != 202 || !chunks[0].Skipped || chunks[1].Skipped {
		t.Fatalf("%d events, first skipped %v", len(chunks), chunks[0].Skipped)
	}
	for i, c := range chunks {
		if len(c.Data) > proto.MaxCommandOutput || c.Seq != i+1 {
			t.Fatalf("event %d: %d bytes, seq %d", i, len(c.Data), c.Seq)
		}
	}

	// GET /api/runs/{id} mid-run serves the bounded tail, not the store row.
	cookie := h.loginCookie()
	body := string(h.apiGet(cookie, "/api/runs/"+id))
	if len(body) > 3*proto.MaxCommandOutput || !strings.Contains(body, `MARK-2\n"`) || !strings.Contains(body, `"output_seq":202`) {
		t.Fatalf("mid-run GET: %d bytes", len(body))
	}

	// The final result is cut the same way.
	ra.reply(id, proto.TypeCommandResult, proto.CommandResult{Status: proto.RunOK,
		Stdout: strings.Repeat("c", 900<<10) + "MARK-3\n", Stderr: "short"})
	h.waitStatus(t, id, proto.RunOK, 3*time.Second)
	r, _ := h.st.GetRun(context.Background(), id)
	if len(r.StdoutTail) != proto.MaxCommandOutput || !strings.HasSuffix(r.StdoutTail, "MARK-3\n") || r.StderrTail != "short" || !r.Truncated {
		t.Fatalf("stored: stdout %d bytes, stderr %q, truncated %v", len(r.StdoutTail), r.StderrTail, r.Truncated)
	}
	if _, _, _, _, ok := h.hub.LiveOutput(id); ok {
		t.Fatal("finished run still buffered")
	}
	// Output after the result, or for a run that never existed, is dropped.
	ra.reply(id, proto.TypeCommandOutput, proto.CommandOutput{Stream: proto.StreamStdout, Data: "late"})
	ra.reply("01ARZ3NDEKTSV4RRFFQ69G5FAV", proto.TypeCommandOutput, proto.CommandOutput{Stream: proto.StreamStdout, Data: "ghost"})
	time.Sleep(100 * time.Millisecond)
	if n := len(h.events.outputFor(id)); n != 202 {
		t.Fatalf("late output published: %d events", n)
	}
}

// With no result at all, a timed-out or lost run keeps the streamed tail.
func TestStreamedTailSurvivesLostRun(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("gone")
	ra := h.startRawAgent(proto.Hello{
		ProtocolVersion: proto.Version, AgentVersion: "9.9.9", DeviceName: "gone", Token: token,
		OS: "linux", Arch: "amd64", Commands: []proto.CommandDef{{Name: "x", TimeoutS: 60}},
	})
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("gone") })
	id, err := h.hub.RequestCommand(context.Background(), "gone", "x", "test")
	if err != nil {
		t.Fatal(err)
	}
	ra.reply(id, proto.TypeCommandOutput, proto.CommandOutput{Stream: proto.StreamStdout, Data: strings.Repeat("a", 100<<10) + "last line\n"})
	waitFor(t, 3*time.Second, func() bool { return len(h.events.outputFor(id)) == 1 })
	ra.conn.CloseNow()
	h.waitStatus(t, id, proto.RunLost, 3*time.Second)
	r, _ := h.st.GetRun(context.Background(), id)
	if len(r.StdoutTail) != proto.MaxCommandOutput || !strings.HasSuffix(r.StdoutTail, "last line\n") || !r.Truncated {
		t.Fatalf("stored: %d bytes, truncated %v", len(r.StdoutTail), r.Truncated)
	}
}

// Everything in commands_update is untrusted, like hello.
func TestCommandsUpdateIsSanitised(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("evil")
	var many []proto.CommandDef
	for i := range 200 {
		many = append(many, proto.CommandDef{Name: "c-" + string(rune('a'+i%26)) + string(rune('a'+i/26)), TimeoutS: 5})
	}
	ra := h.startRawAgent(proto.Hello{
		ProtocolVersion: proto.Version, AgentVersion: "9.9.9", DeviceName: "evil", Token: token, OS: "linux", Arch: "amd64",
		// A hello with repeated and invalid names used to fail the handshake
		// on the primary key; now the bad entries are dropped.
		Commands: append([]proto.CommandDef{{Name: "dup", TimeoutS: 5}, {Name: "dup", TimeoutS: 9}, {Name: "Not Valid", TimeoutS: 5}}, many...),
		Problems: []proto.CommandProblem{{Name: "../etc", Reason: "x"}, {Name: "held", Reason: strings.Repeat("r", 5000)}},
	})
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("evil") })
	ctx := context.Background()
	d, _ := h.st.GetDevice(ctx, "evil")
	cmds, _ := h.st.ListCommands(ctx, d.ID)
	problems, _ := h.st.ListCommandProblems(ctx, d.ID)
	if len(cmds) != proto.MaxCommands || len(problems) != 1 || problems[0].Name != "held" || len(problems[0].Reason) != proto.MaxCommandTextLen {
		t.Fatalf("hello: %d commands, problems %d", len(cmds), len(problems))
	}

	env, _ := proto.New(proto.TypeCommandsUpdate, proto.CommandsUpdate{
		Commands: []proto.CommandDef{
			{Name: "ok", Description: strings.Repeat("d", 5000), TimeoutS: 5, Confirm: true},
			{Name: "bad name", TimeoutS: 5}, {Name: "ok", TimeoutS: 7},
		},
		Problems:    []proto.CommandProblem{{Name: "BAD", Reason: "x"}, {Name: "held", Reason: "r"}},
		ConfigError: strings.Repeat("e", 5000),
	})
	ra.send(env)
	waitFor(t, 3*time.Second, func() bool { return h.events.count("commands:evil") == 1 })
	d, _ = h.st.GetDevice(ctx, "evil")
	cmds, _ = h.st.ListCommands(ctx, d.ID)
	problems, _ = h.st.ListCommandProblems(ctx, d.ID)
	if len(cmds) != 1 || cmds[0].Name != "ok" || cmds[0].TimeoutS != 5 || !cmds[0].Confirm || len(cmds[0].Description) != proto.MaxCommandTextLen {
		t.Fatalf("commands: %+v", cmds)
	}
	if len(problems) != 1 || problems[0].Name != "held" || len(d.CommandsConfigError) != proto.MaxCommandTextLen {
		t.Fatalf("problems %+v, config error %d bytes", problems, len(d.CommandsConfigError))
	}
	// The same update again changes nothing: no second event, no audit row.
	ra.send(env)
	time.Sleep(200 * time.Millisecond)
	if n := h.events.count("commands:evil"); n != 1 {
		t.Fatalf("unchanged update fired %d events", n)
	}
	audits, _ := h.st.ListAudit(ctx, 50, 0)
	n := 0
	for _, a := range audits {
		if a.Action == "commands_update" {
			n++
			if a.Actor != "agent:evil" || !strings.Contains(a.Detail, "added: ok") || !strings.Contains(a.Detail, "removed: ") {
				t.Fatalf("audit row: %+v", a)
			}
		}
	}
	if n != 1 {
		t.Fatalf("commands_update audit rows: %d", n)
	}
}

// A protocol-1 agent has no command_cancel: the server answers 409 and
// sends it nothing.
func TestCancelAgainstProtocol1Session(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("old")
	ra := h.startRawAgent(proto.Hello{
		ProtocolVersion: 1, AgentVersion: "0.1.0", DeviceName: "old", Token: token,
		OS: "linux", Arch: "amd64", Commands: []proto.CommandDef{{Name: "x", TimeoutS: 60}},
	})
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("old") })
	id, err := h.hub.RequestCommand(context.Background(), "old", "x", "test")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return len(ra.received(proto.TypeCommandRequest)) == 1 })
	if err := h.hub.CancelRun(context.Background(), id, "test"); err != hub.ErrCancelUnsupported {
		t.Fatalf("hub: %v", err)
	}
	cookie := h.loginCookie()
	if code := h.apiCall(cookie, http.MethodPost, "/api/runs/"+id+"/cancel", nil); code != http.StatusConflict {
		t.Fatalf("cancel against protocol 1: %d", code)
	}
	time.Sleep(100 * time.Millisecond)
	if got := ra.received(proto.TypeCommandCancel); len(got) != 0 {
		t.Fatalf("protocol-1 agent was sent %d command_cancel", len(got))
	}
	if s := h.runStatus(id); s != proto.RunRunning {
		t.Fatalf("run status %q", s)
	}
	// The v1 agent still completes the run the old way.
	ra.reply(id, proto.TypeCommandResult, proto.CommandResult{Status: proto.RunOK, Stdout: "done\n"})
	h.waitStatus(t, id, proto.RunOK, 3*time.Second)
}

// What the server puts on the wire for a run: a name to start it and a bare
// run ID to stop it. Nothing else.
func TestRequestAndCancelWireShape(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("wire")
	ra := h.startRawAgent(proto.Hello{
		ProtocolVersion: proto.Version, AgentVersion: "9.9.9", DeviceName: "wire", Token: token,
		OS: "linux", Arch: "amd64", Commands: []proto.CommandDef{
			{Name: "x", TimeoutS: 60}, {Name: "bye", TimeoutS: 60, ExpectDisconnect: true},
		},
	})
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("wire") })
	cookie := h.loginCookie()
	id, err := h.hub.RequestCommand(context.Background(), "wire", "x", "test")
	if err != nil {
		t.Fatal(err)
	}
	// A body on the cancel request is ignored, whatever it claims.
	if code := h.apiCall(cookie, http.MethodPost, "/api/runs/"+id+"/cancel", map[string]any{"name": "other", "args": []string{"-rf"}}); code != http.StatusAccepted {
		t.Fatalf("cancel: %d", code)
	}
	waitFor(t, 3*time.Second, func() bool { return len(ra.received(proto.TypeCommandCancel)) == 1 })
	req, cancel := ra.received(proto.TypeCommandRequest)[0], ra.received(proto.TypeCommandCancel)[0]
	if string(req.Payload) != `{"name":"x"}` || req.ID != id {
		t.Fatalf("command_request: id %s payload %s", req.ID, req.Payload)
	}
	if len(cancel.Payload) != 0 || cancel.ID != id {
		t.Fatalf("command_cancel: id %s payload %s", cancel.ID, cancel.Payload)
	}
	// Still running until the agent says otherwise.
	if s := h.runStatus(id); s != proto.RunRunning {
		t.Fatalf("status after cancel sent: %q", s)
	}
	ra.reply(id, proto.TypeCommandResult, proto.CommandResult{Status: proto.RunCancelled, ExitCode: -1, Stdout: "partial\n"})
	h.waitStatus(t, id, proto.RunCancelled, 3*time.Second)
	if r, _ := h.st.GetRun(context.Background(), id); r.ExitCode != nil || r.StdoutTail != "partial\n" {
		t.Fatalf("cancelled run: %+v", r)
	}

	// Finished, unknown and dispatched runs cannot be cancelled.
	if code := h.apiCall(cookie, http.MethodPost, "/api/runs/"+id+"/cancel", nil); code != http.StatusConflict {
		t.Fatalf("cancel of a finished run: %d", code)
	}
	if code := h.apiCall(cookie, http.MethodPost, "/api/runs/01ARZ3NDEKTSV4RRFFQ69G5FAV/cancel", nil); code != http.StatusNotFound {
		t.Fatalf("cancel of an unknown run: %d", code)
	}
	bye, err := h.hub.RequestCommand(context.Background(), "wire", "bye", "test")
	if err != nil {
		t.Fatal(err)
	}
	ra.reply(bye, proto.TypeCommandResult, proto.CommandResult{Status: proto.RunDispatched})
	h.waitStatus(t, bye, proto.RunDispatched, 3*time.Second)
	if code := h.apiCall(cookie, http.MethodPost, "/api/runs/"+bye+"/cancel", nil); code != http.StatusConflict {
		t.Fatalf("cancel of a dispatched run: %d", code)
	}
	if n := len(ra.received(proto.TypeCommandCancel)); n != 1 {
		t.Fatalf("command_cancel sent %d times", n)
	}
	// Unauthenticated: the session cookie guards it like every /api route.
	resp, err := h.srv.Client().Post(h.srv.URL+"/api/runs/"+id+"/cancel", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cancel without a session: %d", resp.StatusCode)
	}
	audits, _ := h.st.ListAudit(context.Background(), 50, 0)
	n := 0
	for _, a := range audits {
		if a.Action == "command_cancel" {
			n++
			if a.Target != "wire/x" || a.Detail != id || a.Actor != "admin" {
				t.Fatalf("audit row: %+v", a)
			}
		}
	}
	if n != 1 {
		t.Fatalf("command_cancel audit rows: %d", n)
	}
}
