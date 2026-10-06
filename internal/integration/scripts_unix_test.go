//go:build !windows

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/agent/app"
	"github.com/jhyoong/KumaBoard/agent/config"
	"github.com/jhyoong/KumaBoard/agent/transport"
	"github.com/jhyoong/KumaBoard/agent/upgrade"
	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/hub"
	"github.com/jhyoong/KumaBoard/server/store"
)

// scriptAgent is a real agent whose config is a real file, loaded and then
// polled the way the service does it.
type scriptAgent struct {
	t    *testing.T
	h    *harness
	ag   *agentHandle
	dir  string // holds config.yaml; scripts go here too
	path string
	head string // everything in the file above commands:
}

// startScriptAgent writes a config with the given commands: block, starts
// the agent on it and waits for the session. prep may adjust the app before
// it connects.
func (h *harness) startScriptAgent(t *testing.T, name, commands string, prep func(*app.App)) *scriptAgent {
	t.Helper()
	dir := t.TempDir()
	token := h.registerDevice(name)
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sa := &scriptAgent{t: t, h: h, dir: dir, path: filepath.Join(dir, "config.yaml")}
	sa.head = fmt.Sprintf("device:\n  name: %s\nserver:\n  url: wss://%s/ws\n  ca_file: %s\ntoken_file: %s\ncapabilities: [metrics, custom-commands]\n",
		name, h.addr, filepath.Join(h.dir, "pki", "ca.pem"), tokenFile)
	sa.write(commands)
	cfg, err := config.Load(sa.path)
	if err != nil {
		t.Fatal(err)
	}
	a := app.New(cfg, h.log, upgrade.StartupResult{})
	t.Cleanup(func() { a.Close(time.Second) })
	a.SetConfigPollInterval(40 * time.Millisecond)
	if prep != nil {
		prep(a)
	}
	sa.ag = h.startApp(a, cfg, func(c *transport.Client) { c.SilenceTimeout = 10 * time.Second })
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected(name) })
	return sa
}

// write replaces the commands: block of the config file. The rest of the
// file stays as it was.
func (sa *scriptAgent) write(commands string) {
	sa.t.Helper()
	sa.writeRaw(sa.head + "commands:\n" + commands)
}

func (sa *scriptAgent) writeRaw(body string) {
	sa.t.Helper()
	// Write-then-rename, as editors do, so the agent never reads half a file.
	tmp := sa.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		sa.t.Fatal(err)
	}
	if err := os.Rename(tmp, sa.path); err != nil {
		sa.t.Fatal(err)
	}
}

func (h *harness) commandNames(t *testing.T, device string) []string {
	t.Helper()
	d, err := h.st.GetDevice(context.Background(), device)
	if err != nil {
		t.Fatal(err)
	}
	cmds, err := h.st.ListCommands(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(cmds))
	for _, c := range cmds {
		names = append(names, c.Name)
	}
	return names
}

func (h *harness) device(t *testing.T, name string) *store.Device {
	t.Helper()
	d, err := h.st.GetDevice(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

const echoCommand = "  echo: {description: Say hi, run: [/bin/echo, hi], timeout_s: 5}\n"

// Rewriting the agent's config file while it is connected adds and removes
// commands with no restart, and nothing but the file can do that.
func TestConfigRewriteAddsAndRemovesCommands(t *testing.T) {
	h := newHarness(t)
	sa := h.startScriptAgent(t, "a", echoCommand, nil)
	if got := h.commandNames(t, "a"); len(got) != 1 || got[0] != "echo" {
		t.Fatalf("at handshake: %v", got)
	}
	if h.device(t, "a").ProtocolVersion != 2 {
		t.Fatal("agent did not declare protocol 2")
	}

	sa.write(echoCommand + "  extra: {run: [/bin/echo, more], timeout_s: 5, confirm: true}\n")
	waitFor(t, 3*time.Second, func() bool { return len(h.commandNames(t, "a")) == 2 })
	if n := h.events.count("commands:a"); n != 1 {
		t.Fatalf("device change events after add: %d", n)
	}
	d := h.device(t, "a")
	cmds, _ := h.st.ListCommands(context.Background(), d.ID)
	if cmds[1].Name != "extra" || !cmds[1].Confirm || cmds[0].Confirm {
		t.Fatalf("commands: %+v", cmds)
	}
	// The new command runs straight away.
	id, err := h.hub.RequestCommand(context.Background(), "a", "extra", "test")
	if err != nil {
		t.Fatal(err)
	}
	h.waitStatus(t, id, proto.RunOK, 3*time.Second)
	if r, _ := h.st.GetRun(context.Background(), id); r.StdoutTail != "more\n" {
		t.Fatalf("run: %+v", r)
	}

	sa.write("  extra: {run: [/bin/echo, more], timeout_s: 5, confirm: true}\n")
	waitFor(t, 3*time.Second, func() bool { got := h.commandNames(t, "a"); return len(got) == 1 && got[0] == "extra" })
	if n := h.events.count("commands:a"); n != 2 {
		t.Fatalf("device change events after remove: %d", n)
	}
	if _, err := h.hub.RequestCommand(context.Background(), "a", "echo", "test"); err != hub.ErrUnknownCommand {
		t.Fatalf("removed command: %v", err)
	}

	// Settings outside commands: are not live: the device keeps its name and
	// session, and only the commands: edit made in the same write lands.
	sa.writeRaw(strings.Replace(sa.head, "capabilities: [metrics, custom-commands]", "capabilities: [metrics, terminal]", 1) +
		"commands:\n" + echoCommand)
	waitFor(t, 3*time.Second, func() bool { got := h.commandNames(t, "a"); return len(got) == 1 && got[0] == "echo" })
	if caps := h.device(t, "a").Capabilities; len(caps) != 2 || caps[1] != proto.CapCustomCommands {
		t.Fatalf("capabilities changed without a restart: %v", caps)
	}
	if h.events.count("connect:a") != 1 {
		t.Fatal("agent reconnected during reloads")
	}
}

// A file that does not parse leaves the commands as they were and says why,
// until a good write clears it.
func TestInvalidConfigKeepsCommandsAndReportsError(t *testing.T) {
	h := newHarness(t)
	sa := h.startScriptAgent(t, "a", echoCommand, nil)

	sa.writeRaw(sa.head + "commands:\n  echo: {run: [/bin/echo, hi\n")
	waitFor(t, 3*time.Second, func() bool { return h.device(t, "a").CommandsConfigError != "" })
	if got := h.commandNames(t, "a"); len(got) != 1 || got[0] != "echo" {
		t.Fatalf("commands changed on a bad file: %v", got)
	}
	// Still runnable with the definition it had.
	id, err := h.hub.RequestCommand(context.Background(), "a", "echo", "test")
	if err != nil {
		t.Fatal(err)
	}
	h.waitStatus(t, id, proto.RunOK, 3*time.Second)

	// The error survives a reconnect: hello cannot carry it, so the agent
	// sends it again.
	sum, err := h.registry.Summary(context.Background(), "a")
	if err != nil || sum.CommandsConfigError == "" || len(sum.Commands) != 1 {
		t.Fatalf("summary: %+v %v", sum, err)
	}
	h.hub.CloseDevice("a", "test")
	waitFor(t, 5*time.Second, func() bool { return h.events.count("connect:a") == 2 })
	waitFor(t, 3*time.Second, func() bool { return h.device(t, "a").CommandsConfigError != "" })

	sa.write(echoCommand + "  extra: {run: [/bin/echo, more], timeout_s: 5}\n")
	waitFor(t, 3*time.Second, func() bool { return h.device(t, "a").CommandsConfigError == "" })
	if got := h.commandNames(t, "a"); len(got) != 2 {
		t.Fatalf("after the fix: %v", got)
	}
}

// A script the agent's own account can modify gets no button. The real
// permission check runs here: the test's temp dir is writable by the test.
func TestWritableScriptIsNotDeclared(t *testing.T) {
	h := newHarness(t)
	scripts := t.TempDir()
	script := filepath.Join(scripts, "backup.sh")
	marker := filepath.Join(scripts, "ran")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.startScriptAgent(t, "a", echoCommand+
		"  backup: {script: "+script+", timeout_s: 5}\n"+
		"  via: {run: [/bin/sh], script: "+script+", timeout_s: 5}\n", nil)

	if got := h.commandNames(t, "a"); len(got) != 1 || got[0] != "echo" {
		t.Fatalf("declared: %v", got)
	}
	d := h.device(t, "a")
	problems, err := h.st.ListCommandProblems(context.Background(), d.ID)
	if err != nil || len(problems) != 2 || problems[0].Name != "backup" || problems[1].Name != "via" {
		t.Fatalf("problems: %+v %v", problems, err)
	}
	if !strings.Contains(problems[0].Reason, "agent account") {
		t.Fatalf("reason: %q", problems[0].Reason)
	}
	sum, err := h.registry.Summary(context.Background(), "a")
	if err != nil || len(sum.CommandProblems) != 2 || len(sum.Commands) != 1 {
		t.Fatalf("summary: %+v %v", sum, err)
	}
	if _, err := h.hub.RequestCommand(context.Background(), "a", "backup", "test"); err != hub.ErrUnknownCommand {
		t.Fatalf("undeclared script: %v", err)
	}
	// A server that asks anyway (it is not trusted either) is refused by
	// the agent: the check is repeated before exec.
	env, _ := proto.New(proto.TypeCommandRequest, proto.CommandRequest{Name: "backup"})
	if err := h.hub.Session("a").Send(env); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a script that failed the check ran")
	}
}

// modeCheck stands in for the permission check where a test needs a script
// that passes: nothing a non-root test creates can pass the real one, which
// has its own unit tests. It keeps the contract (fail if the file or its
// directory is writable, return the resolved path) using mode bits alone.
func modeCheck(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	for _, p := range []string{resolved, filepath.Dir(resolved)} {
		fi, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(p, os.TempDir()) && fi.Mode().Perm()&0o222 != 0 {
			return "", errors.New(p + " is writable by the agent account")
		}
	}
	return resolved, nil
}

// lockedScript writes an executable script into its own read-only directory.
func lockedScript(t *testing.T, name, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "kuma-scripts")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	return p
}

// A script that passed when it was declared but is writable when its button
// is pressed ends as refused, without running.
func TestScriptMadeWritableIsRefused(t *testing.T) {
	h := newHarness(t)
	marker := filepath.Join(t.TempDir(), "ran")
	script := lockedScript(t, "job.sh", "#!/bin/sh\necho ran >> "+marker+"\necho hello from the script\n")
	var agentApp *app.App
	h.startScriptAgent(t, "a", "  job: {script: "+script+", timeout_s: 5}\n", func(a *app.App) {
		a.SetFileCheck(modeCheck)
		agentApp = a
	})
	if got := h.commandNames(t, "a"); len(got) != 1 || got[0] != "job" {
		t.Fatalf("declared: %v", got)
	}
	id, err := h.hub.RequestCommand(context.Background(), "a", "job", "test")
	if err != nil {
		t.Fatal(err)
	}
	h.waitStatus(t, id, proto.RunOK, 3*time.Second)
	if r, _ := h.st.GetRun(context.Background(), id); r.StdoutTail != "hello from the script\n" {
		t.Fatalf("first run: %+v", r)
	}

	// Stop polling so the button is still there, then loosen the file.
	agentApp.SetConfigPollInterval(time.Hour)
	time.Sleep(100 * time.Millisecond)
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	id, err = h.hub.RequestCommand(context.Background(), "a", "job", "test")
	if err != nil {
		t.Fatal(err)
	}
	h.waitStatus(t, id, proto.RunRefused, 3*time.Second)
	r, _ := h.st.GetRun(context.Background(), id)
	if !strings.Contains(r.StderrTail, script+" is writable by the agent account") || r.ExitCode != nil || r.StdoutTail != "" {
		t.Fatalf("refused run: %+v", r)
	}
	if b, _ := os.ReadFile(marker); string(b) != "ran\n" {
		t.Fatalf("script ran %q times", b)
	}

	// The next poll withdraws the button with no config edit, and puts it
	// back once the file is fixed.
	agentApp.SetConfigPollInterval(40 * time.Millisecond)
	waitFor(t, 3*time.Second, func() bool { return len(h.commandNames(t, "a")) == 0 })
	problems, _ := h.st.ListCommandProblems(context.Background(), h.device(t, "a").ID)
	if len(problems) != 1 || problems[0].Name != "job" {
		t.Fatalf("problems: %+v", problems)
	}
	if err := os.Chmod(script, 0o555); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return len(h.commandNames(t, "a")) == 1 })
}

// Output arrives while the command runs, and what is stored at the end is
// exactly what was streamed.
func TestOutputStreamsBeforeResult(t *testing.T) {
	h := newHarness(t)
	h.startScriptAgent(t, "a",
		`  slow: {run: [/bin/sh, -c, "for i in 1 2 3 4 5; do echo out-$i; echo err-$i >&2; sleep 0.3; done"], timeout_s: 20}`+"\n", nil)
	id, err := h.hub.RequestCommand(context.Background(), "a", "slow", "test")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return len(h.events.outputFor(id)) > 0 })
	if s := h.runStatus(id); s != proto.RunRunning {
		t.Fatalf("first chunk arrived with the run already %q", s)
	}
	// Mid-run, the API serves what has been streamed so far.
	cookie := h.loginCookie()
	var mid store.Run
	if err := json.Unmarshal(h.apiGet(cookie, "/api/runs/"+id), &mid); err != nil {
		t.Fatal(err)
	}
	if mid.Status != proto.RunRunning || !strings.HasPrefix(mid.StdoutTail, "out-1\n") || mid.OutputSeq == 0 {
		t.Fatalf("mid-run GET: %+v", mid)
	}
	var list []store.Run
	if err := json.Unmarshal(h.apiGet(cookie, "/api/devices/a/runs"), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !strings.HasPrefix(list[0].StdoutTail, "out-1\n") {
		t.Fatalf("mid-run list: %+v", list)
	}

	h.waitStatus(t, id, proto.RunOK, 10*time.Second)
	r, _ := h.st.GetRun(context.Background(), id)
	chunks := h.events.outputFor(id)
	if len(chunks) < 6 {
		t.Fatalf("expected a chunk per stream per line, got %d", len(chunks))
	}
	if got := joinStream(chunks, proto.StreamStdout); got != r.StdoutTail || got != "out-1\nout-2\nout-3\nout-4\nout-5\n" {
		t.Fatalf("stdout: streamed %q, stored %q", got, r.StdoutTail)
	}
	if got := joinStream(chunks, proto.StreamStderr); got != r.StderrTail || got != "err-1\nerr-2\nerr-3\nerr-4\nerr-5\n" {
		t.Fatalf("stderr: streamed %q, stored %q", got, r.StderrTail)
	}
	if r.Truncated {
		t.Fatal("small output marked truncated")
	}
	// Once finished the stored row is served as is.
	var done store.Run
	json.Unmarshal(h.apiGet(cookie, "/api/runs/"+id), &done)
	if done.OutputSeq != 0 || done.StdoutTail != r.StdoutTail {
		t.Fatalf("finished GET: %+v", done)
	}
}

// 200 KiB of numbered lines: only the last 64 KiB is kept, mid-run and at
// the end, and it ends with the last line.
func TestOutputKeepsLast64KiB(t *testing.T) {
	h := newHarness(t)
	// ~230 KiB in bursts with pauses, so several flush ticks see data.
	h.startScriptAgent(t, "a",
		`  lines: {run: [/bin/sh, -c, "i=0; while [ $i -lt 20000 ]; do echo line-$i; i=$((i+1)); if [ $((i % 4000)) -eq 0 ]; then sleep 0.3; fi; done; echo last-line"], timeout_s: 60}`+"\n", nil)
	id, err := h.hub.RequestCommand(context.Background(), "a", "lines", "test")
	if err != nil {
		t.Fatal(err)
	}
	cookie := h.loginCookie()
	sawMidRun := false
	deadline := time.Now().Add(30 * time.Second)
	for h.runStatus(id) == proto.RunRunning {
		if time.Now().After(deadline) {
			t.Fatal("run did not finish")
		}
		var mid store.Run
		if err := json.Unmarshal(h.apiGet(cookie, "/api/runs/"+id), &mid); err != nil {
			t.Fatal(err)
		}
		if len(mid.StdoutTail) > proto.MaxCommandOutput {
			t.Fatalf("mid-run tail is %d bytes", len(mid.StdoutTail))
		}
		if mid.Status == proto.RunRunning && mid.StdoutTail != "" {
			sawMidRun = true
			if !strings.HasSuffix(mid.StdoutTail, "\n") || !strings.Contains(mid.StdoutTail, "line-") {
				t.Fatalf("mid-run tail does not end on a line: %q", mid.StdoutTail[len(mid.StdoutTail)-20:])
			}
		}
		if stdout, _, _, _, ok := h.hub.LiveOutput(id); ok && len(stdout) > proto.MaxCommandOutput {
			t.Fatalf("in-flight buffer is %d bytes", len(stdout))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sawMidRun {
		t.Fatal("never saw output mid-run")
	}
	h.waitStatus(t, id, proto.RunOK, 3*time.Second)
	r, _ := h.st.GetRun(context.Background(), id)
	if len(r.StdoutTail) > proto.MaxCommandOutput || len(r.StdoutTail) < proto.MaxCommandOutput-16 {
		t.Fatalf("stored tail is %d bytes", len(r.StdoutTail))
	}
	if !strings.HasSuffix(r.StdoutTail, "line-19999\nlast-line\n") || strings.Contains(r.StdoutTail, "line-0\n") || !r.Truncated {
		t.Fatalf("stored tail: truncated %v, ends %q", r.Truncated, r.StdoutTail[len(r.StdoutTail)-30:])
	}
	for i, c := range h.events.outputFor(id) {
		if len(c.Data) > proto.MaxCommandOutput {
			t.Fatalf("chunk %d is %d bytes", i, len(c.Data))
		}
	}
}

// More than 64 KiB inside one flush tick: the agent sends the newest 64 KiB
// and marks the chunk skipped instead of queueing the rest.
func TestOutputBurstIsSkipped(t *testing.T) {
	h := newHarness(t)
	h.startScriptAgent(t, "a",
		`  burst: {run: [/bin/sh, -c, "head -c 1000000 /dev/zero | tr '\\0' a; echo; echo end"], timeout_s: 20}`+"\n", nil)
	id, err := h.hub.RequestCommand(context.Background(), "a", "burst", "test")
	if err != nil {
		t.Fatal(err)
	}
	h.waitStatus(t, id, proto.RunOK, 10*time.Second)
	chunks := h.events.outputFor(id)
	skipped, total := 0, 0
	for _, c := range chunks {
		if c.Skipped {
			skipped++
		}
		if len(c.Data) > proto.MaxCommandOutput {
			t.Fatalf("chunk of %d bytes", len(c.Data))
		}
		total += len(c.Data)
	}
	if skipped == 0 {
		t.Fatalf("no skipped chunk among %d", len(chunks))
	}
	if total >= 1000000 {
		t.Fatalf("the whole burst was streamed: %d bytes in %d chunks", total, len(chunks))
	}
	r, _ := h.st.GetRun(context.Background(), id)
	if !r.Truncated || !strings.HasSuffix(r.StdoutTail, "a\nend\n") || len(r.StdoutTail) != proto.MaxCommandOutput {
		t.Fatalf("stored: %d bytes, truncated %v", len(r.StdoutTail), r.Truncated)
	}
}

// Cancel ends a long run, and the child the script started, within the kill
// grace.
func TestCancelStopsRunAndItsChildren(t *testing.T) {
	h := newHarness(t)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	h.startScriptAgent(t, "a",
		`  long: {run: [/bin/sh, -c, "sleep 30 & echo $! > `+pidFile+`; echo started; wait"], timeout_s: 60}`+"\n"+echoCommand, nil)
	id, err := h.hub.RequestCommand(context.Background(), "a", "long", "test")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { b, _ := os.ReadFile(pidFile); return len(b) > 0 })
	b, _ := os.ReadFile(pidFile)
	child, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || !pidAlive(child) {
		t.Fatalf("child %q not running: %v", b, err)
	}
	waitFor(t, 3*time.Second, func() bool { return len(h.events.outputFor(id)) > 0 })

	cookie := h.loginCookie()
	start := time.Now()
	if code := h.apiCall(cookie, http.MethodPost, "/api/runs/"+id+"/cancel", nil); code != http.StatusAccepted {
		t.Fatalf("cancel: %d", code)
	}
	// 3s is the agent's grace before SIGKILL; SIGTERM alone ends this one.
	h.waitStatus(t, id, proto.RunCancelled, 4*time.Second)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("cancel took %s", d)
	}
	waitFor(t, 2*time.Second, func() bool { return !pidAlive(child) })
	r, _ := h.st.GetRun(context.Background(), id)
	if r.StdoutTail != "started\n" || r.ExitCode != nil || r.FinishedAt == nil {
		t.Fatalf("cancelled run: %+v", r)
	}

	// Cancelling it again: it is no longer in flight.
	if code := h.apiCall(cookie, http.MethodPost, "/api/runs/"+id+"/cancel", nil); code != http.StatusConflict {
		t.Fatalf("cancel of a finished run: %d", code)
	}
	// The busy lock is released: the same command, and others, run again.
	other, _ := h.hub.RequestCommand(context.Background(), "a", "echo", "test")
	h.waitStatus(t, other, proto.RunOK, 3*time.Second)
}

// The busy lock is per command name: two different commands overlap, and
// cancelling one leaves the other alone.
func TestDifferentCommandsOverlap(t *testing.T) {
	h := newHarness(t)
	h.startScriptAgent(t, "a",
		"  one: {run: [/bin/sleep, \"30\"], timeout_s: 60}\n  two: {run: [/bin/sh, -c, \"sleep 0.5; echo two\"], timeout_s: 60}\n", nil)
	one, _ := h.hub.RequestCommand(context.Background(), "a", "one", "test")
	two, _ := h.hub.RequestCommand(context.Background(), "a", "two", "test")
	time.Sleep(150 * time.Millisecond)
	if h.runStatus(one) != proto.RunRunning || h.runStatus(two) != proto.RunRunning {
		t.Fatalf("not overlapping: %s %s", h.runStatus(one), h.runStatus(two))
	}
	if err := h.hub.CancelRun(context.Background(), one, "test"); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(t, one, proto.RunCancelled, 4*time.Second)
	h.waitStatus(t, two, proto.RunOK, 4*time.Second)
}

// R2: a command_request carrying extra fields runs the declared argv and
// nothing else. The agent reads the name; the rest is not looked at.
func TestCommandRequestExtraFieldsAreIgnored(t *testing.T) {
	h := newHarness(t)
	out := filepath.Join(t.TempDir(), "argv")
	h.startScriptAgent(t, "a",
		`  show: {run: [/bin/sh, -c, "echo \"$# $*\" >> `+out+`", sh, fixed], timeout_s: 5}`+"\n", nil)

	env, _ := proto.New(proto.TypeCommandRequest, nil)
	env.Payload = json.RawMessage(`{"name":"show","args":["injected"],"argv":["/bin/rm","-rf","/"],"run":["/bin/sh","-c","echo pwned"],` +
		`"script":"/tmp/evil.sh","env":{"PATH":"/tmp"},"stdin":"injected","timeout_s":1,"cwd":"/"}`)
	if err := h.hub.Session("a").Send(env); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { b, _ := os.ReadFile(out); return len(b) > 0 })
	// And through the normal path, for comparison.
	id, err := h.hub.RequestCommand(context.Background(), "a", "show", "test")
	if err != nil {
		t.Fatal(err)
	}
	h.waitStatus(t, id, proto.RunOK, 3*time.Second)
	b, _ := os.ReadFile(out)
	if string(b) != "1 fixed\n1 fixed\n" {
		t.Fatalf("argv seen by the command: %q", b)
	}
	// A name that is not declared, however it is dressed up, runs nothing.
	bad, _ := proto.New(proto.TypeCommandRequest, nil)
	bad.Payload = json.RawMessage(`{"name":"/bin/sh","run":["/bin/sh","-c","echo pwned >> ` + out + `"]}`)
	h.hub.Session("a").Send(bad)
	time.Sleep(300 * time.Millisecond)
	if b, _ := os.ReadFile(out); string(b) != "1 fixed\n1 fixed\n" {
		t.Fatalf("undeclared name ran something: %q", b)
	}
}

// End to end to the browser: run_output and device events on /api/events.
func TestScriptEventsReachTheDashboard(t *testing.T) {
	h := newHarness(t)
	h.stopServer()
	h.hubOpt.Events = scriptsTee{recorder: h.events, reg: h.registry}
	h.startServer()
	sa := h.startScriptAgent(t, "a",
		`  slow: {run: [/bin/sh, -c, "echo first; sleep 0.5; echo second"], timeout_s: 20}`+"\n", nil)
	cookie := h.loginCookie()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := h.sseEvents(ctx, cookie)
	time.Sleep(100 * time.Millisecond) // let the subscription attach

	id, err := h.hub.RequestCommand(context.Background(), "a", "slow", "test")
	if err != nil {
		t.Fatal(err)
	}
	data := nextSSE(t, events, "run_output", func(d string) bool { return strings.Contains(d, id) })
	var ev struct {
		RunID, Device, Stream, Data string
		Seq                         int
		Skipped                     bool
	}
	if err := json.Unmarshal([]byte(strings.NewReplacer(`"run_id"`, `"RunID"`).Replace(data)), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.RunID != id || ev.Device != "a" || ev.Seq != 1 || ev.Stream != "stdout" || ev.Data != "first\n" || ev.Skipped {
		t.Fatalf("run_output: %s", data)
	}
	nextSSE(t, events, "run", func(d string) bool { return strings.Contains(d, id) && strings.Contains(d, `"status":"ok"`) })

	sa.write(echoCommand)
	data = nextSSE(t, events, "device", func(d string) bool { return strings.Contains(d, `"name":"echo"`) })
	if !strings.Contains(data, `"command_problems":[]`) || !strings.Contains(data, `"commands_config_error":""`) || !strings.Contains(data, `"confirm":false`) {
		t.Fatalf("device event: %s", data)
	}
}
