import { useEffect, useState } from 'react'
import { api, type AuditEntry } from '../api'
import { when } from '../format'

export function Audit() {
  const [entries, setEntries] = useState<AuditEntry[]>([])
  const [done, setDone] = useState(false)

  const load = async (before?: string) => {
    const page = await api<AuditEntry[]>(`/api/audit?limit=100${before ? `&before=${before}` : ''}`)
    setEntries((e) => (before ? [...e, ...page] : page))
    if (page.length < 100) setDone(true)
  }

  useEffect(() => {
    load().catch(() => {})
  }, [])

  return (
    <div>
      <h1 className="mb-4 text-2xl font-semibold">Audit log</h1>
      <table className="w-full text-left text-sm">
        <thead className="border-b border-border text-fg-subtle">
          <tr><th className="py-1">Time</th><th>Actor</th><th>Action</th><th>Target</th><th>Result</th><th>Detail</th></tr>
        </thead>
        <tbody>
          {entries.map((e) => (
            <tr key={e.id} className="border-b border-border">
              <td className="py-1 whitespace-nowrap">{when(e.ts)}</td>
              <td>{e.actor}</td>
              <td>{e.action}</td>
              <td className="font-mono">{e.target}</td>
              <td className={e.result === 'failed' || e.result === 'auth_failed' ? 'text-danger' : ''}>{e.result}</td>
              <td className="text-fg-subtle">{e.detail}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {!done && entries.length > 0 && (
        <button className="mt-3 text-sm text-link hover:underline" onClick={() => load(entries[entries.length - 1].id)}>Load more</button>
      )}
    </div>
  )
}
