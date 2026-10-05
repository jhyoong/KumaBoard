import { useEffect, useRef, useState } from 'react'
import { useParams, Link } from 'react-router'
import { api } from '../api'
import { Terminal as XTerm } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { connectTerminal, terminalSocketURL } from '../terminal'
import type { TerminalHandle, TerminalState } from '../terminal'

const statusText: Record<TerminalState['phase'], string> = {
  requesting: 'Requesting ticket...',
  connecting: 'Connecting...',
  waiting: 'Waiting for agent...',
  live: 'Connected',
  ended: '',
}

export function TerminalView() {
  const { name = '' } = useParams()
  const termRef = useRef<HTMLDivElement>(null)
  const handleRef = useRef<TerminalHandle | null>(null)
  const [state, setState] = useState<TerminalState>({ phase: 'requesting' })
  // Bumped by Reconnect; each attempt gets a fresh xterm and socket.
  const [attempt, setAttempt] = useState(0)

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
    if (termRef.current) {
      term.open(termRef.current)
      fit.fit()
    }

    const handle = connectTerminal({
      requestTicket: () => api<{ ticket: string; session_id: string }>(
        `/api/devices/${encodeURIComponent(name)}/terminal`,
        { method: 'POST', body: JSON.stringify({ cols: term.cols, rows: term.rows }) },
      ),
      createSocket: (url) => new WebSocket(url),
      url: terminalSocketURL(location),
      onState: setState,
      onOutput: (bytes) => term.write(bytes),
    })
    handleRef.current = handle

    const onData = term.onData((data) => handle.input(data))
    const onResize = term.onResize(({ cols, rows }) => handle.resize(cols, rows))
    const ro = new ResizeObserver(() => fit.fit())
    if (termRef.current) ro.observe(termRef.current)

    return () => {
      ro.disconnect()
      onData.dispose()
      onResize.dispose()
      handle.close()
      if (handleRef.current === handle) handleRef.current = null
      term.dispose()
    }
  }, [name, attempt])

  const ended = state.phase === 'ended'

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-3">
        <Link to={`/devices/${name}`} className="text-sm text-link hover:underline">
          &larr; {name}
        </Link>
        {!ended && <span className="text-sm text-fg-subtle">{statusText[state.phase]}</span>}
        {!ended ? (
          <button
            onClick={() => handleRef.current?.close()}
            className="rounded border border-danger-line px-2 py-0.5 text-sm text-danger hover:bg-danger-soft"
          >
            Disconnect
          </button>
        ) : (
          <button
            onClick={() => setAttempt((a) => a + 1)}
            className="rounded border border-border-strong px-2 py-0.5 text-sm hover:bg-surface-muted"
          >
            Reconnect
          </button>
        )}
      </div>
      {ended && state.message && (
        <div className={state.tone === 'error'
          ? 'rounded bg-danger-soft p-3 text-sm text-danger'
          : 'rounded bg-info-soft p-3 text-sm text-info'}
        >
          {state.message}
        </div>
      )}
      {/* xterm itself keeps its fixed dark palette in both themes. */}
      <div ref={termRef} className="rounded border border-border-strong bg-[#1e1e2e]" style={{ height: '70vh' }} />
    </div>
  )
}
