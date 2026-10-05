# Phase 3b: Web Terminal — Frontend, Selftest, and Integration

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build the xterm.js frontend, terminal_enabled toggle, agent selftest, and integration tests for the web terminal feature.

**Architecture:** The frontend uses xterm.js to render a terminal, connected via WebSocket to the server broker built in phase 3a. The agent selftest validates PTY support before an upgrade is committed.

**Tech Stack:** TypeScript/React (xterm, @xterm/addon-fit), Go

**Prerequisite:** Phase 3a (Tasks 1-6) must be complete before starting this plan.

---

### Task 1: Frontend — install xterm.js and create terminal view

**Files:**
- Modify: `web/package.json` (add xterm deps)
- Create: `web/src/views/Terminal.tsx`
- Modify: `web/src/App.tsx` (add route)
- Modify: `web/src/views/DeviceDetail.tsx` (add Terminal button)

**Step 1: Install xterm.js**

Run: `cd /path/to/KumaBoard/web && npm install xterm @xterm/addon-fit`

**Step 2: Create the Terminal view**

Create `web/src/views/Terminal.tsx`:

```tsx
import { useEffect, useRef, useState } from 'react'
import { useParams, Link } from 'react-router'
import { api } from '../api'
import { Terminal as XTerm } from 'xterm'
import { FitAddon } from '@xterm/addon-fit'
import 'xterm/css/xterm.css'

type Status = 'connecting' | 'open' | 'closed' | 'error'

export function TerminalView() {
  const { name = '' } = useParams()
  const termRef = useRef<HTMLDivElement>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const xtermRef = useRef<XTerm | null>(null)
  const [status, setStatus] = useState<Status>('connecting')
  const [errorMsg, setErrorMsg] = useState('')
  const [exitCode, setExitCode] = useState<number | null>(null)

  useEffect(() => {
    const term = new XTerm({
      cursorBlink: true,
      fontSize: 14,
      theme: {
        background: '#1e1e2e',
        foreground: '#cdd6f4',
        cursor: '#f5e0dc',
      },
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    xtermRef.current = term

    if (termRef.current) {
      term.open(termRef.current)
      fit.fit()
    }

    let ws: WebSocket | null = null

    async function connect() {
      try {
        const res = await api<{ ticket: string; session_id: string }>(
          `/api/devices/${name}/terminal`,
          {
            method: 'POST',
            body: JSON.stringify({ cols: term.cols, rows: term.rows }),
          }
        )

        const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
        ws = new WebSocket(`${proto}//${location.host}/ws/terminal`)
        ws.binaryType = 'arraybuffer'
        wsRef.current = ws

        ws.onopen = () => {
          ws!.send(JSON.stringify({ ticket: res.ticket }))
          setStatus('open')
        }

        ws.onmessage = (ev) => {
          if (ev.data instanceof ArrayBuffer) {
            term.write(new Uint8Array(ev.data))
          } else {
            try {
              const ctrl = JSON.parse(ev.data)
              if (ctrl.type === 'exit') {
                setExitCode(ctrl.code)
                setStatus('closed')
              }
            } catch { /* ignore non-JSON text */ }
          }
        }

        ws.onclose = () => {
          if (status !== 'closed') setStatus('closed')
        }

        ws.onerror = () => {
          setStatus('error')
          setErrorMsg('WebSocket connection failed')
        }

        term.onData((data) => {
          if (ws?.readyState === WebSocket.OPEN) {
            const encoder = new TextEncoder()
            ws.send(encoder.encode(data))
          }
        })

        term.onResize(({ cols, rows }) => {
          if (ws?.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify({ type: 'resize', cols, rows }))
          }
        })

        const ro = new ResizeObserver(() => fit.fit())
        if (termRef.current) ro.observe(termRef.current)

        return () => ro.disconnect()
      } catch (err) {
        setStatus('error')
        setErrorMsg((err as Error).message)
      }
    }

    const cleanup = connect()

    return () => {
      cleanup?.then((fn) => fn?.())
      ws?.close()
      term.dispose()
    }
  }, [name])

  function disconnect() {
    wsRef.current?.close()
    setStatus('closed')
  }

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-3">
        <Link to={`/devices/${name}`} className="text-sm text-blue-600 hover:underline">
          &larr; {name}
        </Link>
        <span className="text-sm text-gray-500">
          {status === 'open' && 'Connected'}
          {status === 'connecting' && 'Connecting...'}
          {status === 'closed' && (exitCode !== null ? `Shell exited (${exitCode})` : 'Disconnected')}
          {status === 'error' && `Error: ${errorMsg}`}
        </span>
        {status === 'open' && (
          <button
            onClick={disconnect}
            className="rounded border border-red-300 px-2 py-0.5 text-sm text-red-600 hover:bg-red-50"
          >
            Disconnect
          </button>
        )}
      </div>
      <div ref={termRef} className="rounded border border-gray-300" style={{ height: '70vh' }} />
    </div>
  )
}
```

**Step 3: Add the route**

In `web/src/App.tsx`, add the import and route:

```tsx
import { TerminalView } from './views/Terminal';

// Inside the Shell component's Routes:
<Route path="/devices/:name/terminal" element={<TerminalView />} />
```

**Step 4: Add Terminal button to DeviceDetail**

In `web/src/views/DeviceDetail.tsx`, add a Link import and a Terminal button in the Commands section:

```tsx
import { Link } from 'react-router'  // already imported via useNavigate, just add Link

// In the Commands section's button flex div, after the wake button:
{device.connected && device.capabilities.includes('terminal') && (
  <Link
    to={`/devices/${name}/terminal`}
    className="rounded bg-gray-800 px-3 py-1 text-sm text-white hover:bg-gray-700"
  >
    Terminal
  </Link>
)}
```

**Step 5: Build frontend**

Run: `cd /path/to/KumaBoard/web && npm run build`
Expected: builds without errors

**Step 6: Commit**

```bash
git add web/
git commit -m "feat(web): xterm.js terminal view with ticket-based WebSocket connection"
```

---

### Task 2: Frontend — terminal_enabled toggle in settings

**Files:**
- Modify: `web/src/views/DeviceDetail.tsx` (pass `terminal_enabled` to save)
- Modify: `web/src/components/ScheduleEditor.tsx` (add toggle)
- Modify: `web/src/api.ts` (add `terminal_enabled` to `Device`)

**Step 1: Add terminal_enabled to Device type**

In `web/src/api.ts`, add to the `Device` interface:

```typescript
terminal_enabled: boolean;
```

**Step 2: Add toggle to ScheduleEditor**

In `web/src/components/ScheduleEditor.tsx`, add a `terminal_enabled` checkbox. The component currently receives a `device` prop and an `onSave` callback. Add `terminal_enabled` to the state and the save payload.

Read the component first to understand its structure, then add:
- A `terminalEnabled` state initialized from `device.terminal_enabled`
- A checkbox in the form
- Include `terminal_enabled` in the `onSave` payload

**Step 3: Update the save handler in DeviceDetail**

In `web/src/views/DeviceDetail.tsx`, update the `save` function to include `terminal_enabled`:

```tsx
const save = async (body: { mac: string; normally_off: boolean; schedule: Schedule; terminal_enabled: boolean }) => {
  await api(`/api/devices/${name}`, { method: 'PATCH', body: JSON.stringify(body) })
}
```

**Step 4: Update Summary type to include terminal_enabled**

In `server/registry/registry.go`, add `TerminalEnabled` to the `Summary` struct:

```go
TerminalEnabled bool `json:"terminal_enabled"`
```

And set it in `summarise`:

```go
TerminalEnabled: d.TerminalEnabled,
```

**Step 5: Build and test**

Run: `cd /path/to/KumaBoard/web && npm run build`
Run: `cd /path/to/KumaBoard && go build ./...`
Expected: both build

**Step 6: Commit**

```bash
git add web/src/ server/registry/registry.go
git commit -m "feat(web): terminal_enabled toggle in device settings"
```

---

### Task 3: Agent — selftest PTY check

**Files:**
- Modify: `cmd/kuma-agent/main.go` (extend `runSelftest`)
- Create: `cmd/kuma-agent/selftest_pty_unix.go`
- Create: `cmd/kuma-agent/selftest_pty_windows.go`

**Step 1: Add PTY selftest when terminal capability is configured**

In `cmd/kuma-agent/main.go`, inside `runSelftest`, after the collectors check:

```go
for _, cap := range cfg.Capabilities {
	if cap == proto.CapTerminal {
		if err := selftestPTY(); err != nil {
			return fmt.Errorf("selftest: pty: %w", err)
		}
		break
	}
}
```

Create a helper (can be in the same file or a new file — same file is simpler since selftest is small):

```go
func selftestPTY() error {
	// Build-tag-specific implementation
	return selftestPTYPlatform()
}
```

**Step 2: Create the platform-specific implementations**

Create `cmd/kuma-agent/selftest_pty_unix.go`:

```go
//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/creack/pty"
)

func selftestPTYPlatform() error {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("failed to start pty: %w", err)
	}
	defer ptmx.Close()

	ptmx.Write([]byte("exit\n"))

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		return fmt.Errorf("pty selftest timed out")
	}
	fmt.Println("pty: ok")
	return nil
}
```

Create `cmd/kuma-agent/selftest_pty_windows.go`:

```go
//go:build windows

package main

import "fmt"

func selftestPTYPlatform() error {
	fmt.Println("pty: skipped (windows)")
	return nil
}
```

**Step 3: Build**

Run: `cd /path/to/KumaBoard && go build ./...`
Expected: compiles

**Step 4: Commit**

```bash
git add cmd/kuma-agent/
git commit -m "feat(agent): selftest PTY check when terminal capability is enabled"
```

---

### Task 4: Integration test — full terminal round-trip

**Files:**
- Create: `internal/integration/terminal_test.go`

**Step 1: Write the integration test**

This test uses the existing integration harness (see `internal/integration/harness_test.go`) to stand up a server and agent, request a terminal ticket, connect both sides, and verify data flows.

Create `internal/integration/terminal_test.go`:

```go
package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestTerminalRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	h := newHarness(t)
	h.registerDevice(t, "term-dev", withCapabilities([]string{"metrics", "terminal"}), withTerminalEnabled(true))
	h.connectAgent(t, "term-dev")

	// Request a terminal ticket
	resp := h.apiPost(t, "/api/devices/term-dev/terminal", map[string]int{"cols": 80, "rows": 24})
	if resp.StatusCode != 200 {
		t.Fatalf("ticket request: %d", resp.StatusCode)
	}
	var ticket struct {
		Ticket    string `json:"ticket"`
		SessionID string `json:"session_id"`
	}
	json.NewDecoder(resp.Body).Decode(&ticket)
	if ticket.Ticket == "" || ticket.SessionID == "" {
		t.Fatal("empty ticket response")
	}
}
```

Note: This test depends on the harness supporting `withCapabilities`, `withTerminalEnabled`, and `apiPost` helpers. If those don't exist, they need to be added to `harness_test.go`. The implementation agent should check the harness and adapt accordingly.

**Step 2: Run test**

Run: `cd /path/to/KumaBoard && go test ./internal/integration/ -run TestTerminalRoundTrip -v`
Expected: PASS (or adjust based on what the harness supports)

**Step 3: Commit**

```bash
git add internal/integration/terminal_test.go
git commit -m "test: integration test for terminal ticket request"
```

---

### Task 5: Clean up and final verification

**Files:**
- All modified files

**Step 1: Run all tests**

Run: `cd /path/to/KumaBoard && go test ./... -v`
Expected: all PASS

**Step 2: Build both binaries**

Run: `cd /path/to/KumaBoard && go build -o kumaboard ./cmd/kumaboard && go build -o kuma-agent ./cmd/kuma-agent`
Expected: both build

**Step 3: Build frontend**

Run: `cd /path/to/KumaBoard/web && npm run build`
Expected: builds

**Step 4: Run linter on frontend**

Run: `cd /path/to/KumaBoard/web && npm run lint`
Expected: no errors

**Step 5: Final commit if any cleanup was needed**

```bash
git add -A
git commit -m "chore: phase 3 cleanup and final verification"
```
