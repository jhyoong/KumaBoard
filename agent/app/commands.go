package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"reflect"
	"time"

	"github.com/jhyoong/KumaBoard/agent/config"
	"github.com/jhyoong/KumaBoard/agent/transport"
	"github.com/jhyoong/KumaBoard/proto"
)

// defaultConfigPoll is how often the agent looks at its own config file and
// re-runs the script permission check.
const defaultConfigPoll = 5 * time.Second

// commandState is what the agent tells the server about its commands.
type commandState struct {
	defs      []proto.CommandDef
	problems  []proto.CommandProblem
	configErr string
}

func (s commandState) equal(o commandState) bool {
	return s.configErr == o.configErr && reflect.DeepEqual(s.defs, o.defs) && reflect.DeepEqual(s.problems, o.problems)
}

// configWatch is the reload state, guarded by App.mu.
type configWatch struct {
	interval time.Duration
	poke     chan struct{} // wakes the loop after an interval change

	// What the file looked like at the last read, to skip unchanged ticks.
	size    int64
	modTime time.Time
	sum     [sha256.Size]byte
	read    bool

	configErr string        // why the last reload was rejected, or ""
	announced *commandState // last state the server was told, nil if none
}

// SetConfigPollInterval overrides how often the config file is polled.
// Useful in tests.
func (a *App) SetConfigPollInterval(d time.Duration) {
	a.mu.Lock()
	a.watch.interval = d
	a.mu.Unlock()
	select {
	case a.watch.poke <- struct{}{}:
	default:
	}
}

// SetFileCheck replaces the script permission check. Useful in tests, where
// a script the test wrote can never pass the real one.
func (a *App) SetFileCheck(check func(path string) (resolved string, err error)) {
	a.runner.SetCheck(check)
}

// currentCommands is the command state right now: the permission check is
// run fresh on every call.
func (a *App) currentCommands() commandState {
	defs, problems := a.runner.Declared()
	a.mu.Lock()
	defer a.mu.Unlock()
	return commandState{defs: defs, problems: problems, configErr: a.watch.configErr}
}

// watchConfig polls the config file and the script permission check until
// ctx ends. It is the only thing that ever triggers a reload: the server has
// no message that does.
func (a *App) watchConfig(ctx context.Context) {
	for {
		a.mu.Lock()
		interval := a.watch.interval
		a.mu.Unlock()
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-a.watch.poke:
			t.Stop()
			continue
		case <-t.C:
		}
		a.reloadCommands()
		a.announceCommands()
	}
}

// reloadCommands re-reads the config file if it changed and swaps in its
// commands: block. A file that does not parse leaves the previous commands
// in place and records why.
func (a *App) reloadCommands() {
	if a.cfg.Path == "" {
		return
	}
	fi, err := os.Stat(a.cfg.Path)
	if err != nil {
		a.configUnreadable(err)
		return
	}
	a.mu.Lock()
	w := &a.watch
	unchanged := w.read && fi.Size() == w.size && fi.ModTime().Equal(w.modTime)
	a.mu.Unlock()
	if unchanged {
		return
	}
	b, err := os.ReadFile(a.cfg.Path)
	if err != nil {
		a.configUnreadable(err)
		return
	}
	sum := sha256.Sum256(b)
	a.mu.Lock()
	first := !w.read
	same := w.read && bytes.Equal(sum[:], w.sum[:])
	w.size, w.modTime, w.sum, w.read = fi.Size(), fi.ModTime(), sum, true
	a.mu.Unlock()
	if same {
		return
	}
	cmds, err := config.ParseCommands(b)
	if err != nil {
		// Keep the previous commands; the device page shows why.
		a.setConfigErr(err.Error())
		return
	}
	if changed := a.cfg.RestartOnlyChanges(b); len(changed) > 0 {
		a.log.Warn("config change ignored until the agent restarts; only commands: is reloaded", "settings", changed)
	}
	a.runner.SetCommands(cmds)
	a.setConfigErr("")
	if !first {
		a.log.Info("commands reloaded from config", "count", len(cmds))
	}
}

// configUnreadable records a stat or read failure and forgets what the file
// looked like, so it is parsed again once it is back even if unchanged.
func (a *App) configUnreadable(err error) {
	a.setConfigErr("config: " + err.Error())
	a.mu.Lock()
	a.watch.read = false
	a.mu.Unlock()
}

func (a *App) setConfigErr(msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if msg != "" && msg != a.watch.configErr {
		a.log.Error("config not reloaded; keeping the previous commands", "err", msg)
	}
	a.watch.configErr = msg
}

// announceCommands sends commands_update when the command state differs
// from what the server was last told. While disconnected it does nothing:
// the next hello carries the current set.
func (a *App) announceCommands() {
	cur := a.currentCommands()
	a.mu.Lock()
	send := a.send
	// announced is nil only before the first hello, which will carry cur.
	stale := send != nil && a.watch.announced != nil && !a.watch.announced.equal(cur)
	a.mu.Unlock()
	if !stale {
		return
	}
	env, err := proto.New(proto.TypeCommandsUpdate, proto.CommandsUpdate{
		Commands: cur.defs, Problems: cur.problems, ConfigError: cur.configErr,
	})
	if err != nil {
		return
	}
	if err := send.Send(env); err != nil {
		return
	}
	for _, p := range cur.problems {
		a.log.Warn("command not available", "command", p.Name, "reason", p.Reason)
	}
	a.mu.Lock()
	if a.send == send {
		a.watch.announced = &cur
	}
	a.mu.Unlock()
}

// commandOutput returns the sink that streams one run's output as
// command_output replies to its request, so the envelope ID is the run ID.
func commandOutput(env *proto.Envelope, send transport.Sender) func(proto.CommandOutput) {
	return func(o proto.CommandOutput) {
		if reply, err := proto.Reply(env, proto.TypeCommandOutput, o); err == nil {
			send.Send(reply)
		}
	}
}
