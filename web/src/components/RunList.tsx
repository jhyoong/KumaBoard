import { useState } from 'react'
import type { Run } from '../api'
import { when } from '../format'

const colour: Record<string, string> = {
  ok: 'text-success', disconnected_as_expected: 'text-success',
  running: 'text-info', dispatched: 'text-info',
  failed: 'text-danger', timeout: 'text-danger', no_disconnect: 'text-danger',
  unknown_command: 'text-danger', busy: 'text-warning', lost: 'text-fg-subtle',
}

function RunRow({ run }: { run: Run }) {
  const [open, setOpen] = useState(false)
  const hasOutput = run.stdout_tail || run.stderr_tail
  return (
    <li className="border-b border-border py-2 text-sm">
      <div className="flex items-center gap-3">
        <span className="font-mono">{run.command}</span>
        <span className={`font-medium ${colour[run.status] ?? ''}`}>{run.status}</span>
        {run.exit_code !== null && <span className="text-fg-subtle">exit {run.exit_code}</span>}
        <span className="ml-auto text-fg-subtle">{when(run.requested_at)}</span>
        {hasOutput && <button className="text-link hover:underline" onClick={() => setOpen(!open)}>{open ? 'hide' : 'output'}</button>}
      </div>
      {open && (
        <div className="mt-2 space-y-1">
          {/* Console output keeps a fixed dark palette in both themes, like the terminal. */}
          {run.stdout_tail && <pre className="overflow-x-auto rounded border border-border bg-gray-900 p-2 text-xs text-gray-100">{run.stdout_tail}</pre>}
          {run.stderr_tail && <pre className="overflow-x-auto rounded border border-border bg-red-950 p-2 text-xs text-red-100">{run.stderr_tail}</pre>}
          {run.truncated && <div className="text-xs text-fg-subtle">output truncated at 64 KiB</div>}
        </div>
      )}
    </li>
  )
}

export function RunList({ runs }: { runs: Run[] }) {
  if (runs.length === 0) return <p className="text-sm text-fg-subtle">No runs yet.</p>
  return <ul>{runs.map((r) => <RunRow key={r.id} run={r} />)}</ul>
}
