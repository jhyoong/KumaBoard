import { useEffect, useRef, useState } from 'react'
import type { Run } from '../api'
import { when } from '../format'

const colour: Record<string, string> = {
  ok: 'text-success', disconnected_as_expected: 'text-success',
  running: 'text-info', dispatched: 'text-info',
  failed: 'text-danger', timeout: 'text-danger', no_disconnect: 'text-danger',
  unknown_command: 'text-danger', refused: 'text-danger',
  busy: 'text-warning', cancelled: 'text-warning', lost: 'text-fg-subtle',
}

// OutputPane shows one stream. While the run is live it follows the output
// like tail -f, unless the user has scrolled up to read something.
function OutputPane({ text, live, className }: { text: string; live: boolean; className: string }) {
  const ref = useRef<HTMLPreElement>(null)
  const pinned = useRef(true)
  useEffect(() => {
    const el = ref.current
    if (el && live && pinned.current) el.scrollTop = el.scrollHeight
  }, [text, live])
  return (
    <pre
      ref={ref}
      onScroll={(e) => {
        const el = e.currentTarget
        pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 8
      }}
      className={`max-h-96 overflow-auto rounded border border-border p-2 text-xs ${className}`}
    >
      {text}
    </pre>
  )
}

function RunRow({ run, onStop }: { run: Run; onStop?: (run: Run) => void }) {
  const running = run.status === 'running'
  // A row that is running when it appears opens by itself and stays open
  // when the run ends, so the result is not snatched away.
  const [open, setOpen] = useState(running)
  const hasOutput = run.stdout_tail || run.stderr_tail
  return (
    <li className="border-b border-border py-2 text-sm">
      <div className="flex items-center gap-3">
        <span className="font-mono">{run.command}</span>
        <span className={`font-medium ${colour[run.status] ?? ''}`}>{run.status}</span>
        {run.exit_code !== null && <span className="text-fg-subtle">exit {run.exit_code}</span>}
        {running && onStop && (
          <button className="rounded border border-danger px-2 text-xs text-danger hover:bg-danger-soft" onClick={() => onStop(run)}>Stop</button>
        )}
        <span className="ml-auto text-fg-subtle">{when(run.requested_at)}</span>
        {(hasOutput || running) && <button className="text-link hover:underline" onClick={() => setOpen(!open)}>{open ? 'hide' : 'output'}</button>}
      </div>
      {open && (
        <div className="mt-2 space-y-1">
          {/* Console output keeps a fixed dark palette in both themes, like the terminal. */}
          {run.stdout_tail && <OutputPane text={run.stdout_tail} live={running} className="bg-gray-900 text-gray-100" />}
          {run.stderr_tail && <OutputPane text={run.stderr_tail} live={running} className="bg-red-950 text-red-100" />}
          {running && !hasOutput && <div className="text-xs text-fg-subtle">No output yet.</div>}
          {run.truncated && <div className="text-xs text-fg-subtle">showing the last 64 KiB</div>}
        </div>
      )}
    </li>
  )
}

// onStop, when given, puts a Stop button on running rows. Leave it out for
// devices whose agent cannot cancel (protocol 1).
export function RunList({ runs, onStop }: { runs: Run[]; onStop?: (run: Run) => void }) {
  if (runs.length === 0) return <p className="text-sm text-fg-subtle">No runs yet.</p>
  return <ul>{runs.map((r) => <RunRow key={r.id} run={r} onStop={onStop} />)}</ul>
}
