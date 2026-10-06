import { useState } from 'react'
import { getUpgrade, type UpgradeDetail, type UpgradeEntry, type UpgradeEvent } from '../api'
import { when } from '../format'
import { NO_DETAIL_TEXT, between, clockTime, duration, isFailed, reasonText, stepDurations } from '../upgrade'

export function UpgradeStateBadge({ state }: { state: string }) {
  return (
    <span className={`rounded px-2 py-0.5 text-xs font-medium ${
      state === 'verified' ? 'bg-success-soft text-success' :
      isFailed(state) ? 'bg-danger-soft text-danger' :
      'bg-warning-soft text-warning'
    }`}>{state}</span>
  )
}

// DetailText shows free text that came with an upgrade report. An agent
// wrote it, so it is data and nothing else: one React text node in a <pre>.
// Never parse it, linkify it or hand it to dangerouslySetInnerHTML.
function DetailText({ text }: { text: string }) {
  return (
    <pre className="max-h-96 overflow-auto whitespace-pre-wrap break-words rounded border border-border bg-inset p-2 text-xs text-fg">
      {text}
    </pre>
  )
}

// The server and the operator close upgrades too (timeout, abandon); only
// text from those two is not the agent's. Anything else is treated as agent
// text, the untrusted case.
function detailCaption(source: string): string {
  return source === 'server' || source === 'admin'
    ? 'Recorded by the server'
    : 'Reported by the agent, shown as received'
}

// OutputBlock is the expandable block for an upgrade's detail text, with a
// button that copies the string exactly as received.
function OutputBlock({ text, source, defaultOpen }: { text: string; source: string; defaultOpen: boolean }) {
  const [open, setOpen] = useState(defaultOpen)
  const [copied, setCopied] = useState(false)

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(true)
    } catch { /* no clipboard access; the text is still selectable */ }
  }

  return (
    <div className="mt-2">
      <div className="flex items-center gap-3 text-xs">
        <button className="text-link hover:underline" onClick={() => setOpen(!open)}>{open ? 'Hide output' : 'Show output'}</button>
        {open && <button className="text-link hover:underline" onClick={copy}>{copied ? 'Copied' : 'Copy'}</button>}
      </div>
      {open && (
        <div className="mt-1">
          <div className="mb-1 text-xs text-fg-subtle">{detailCaption(source)}</div>
          <DetailText text={text} />
        </div>
      )}
    </div>
  )
}

// Timeline lists the recorded steps. An event the server stored without
// applying (a late or superseded report) is greyed. skipDetail is the text
// the output block already shows, so it is not printed twice.
function Timeline({ events, skipDetail }: { events: UpgradeEvent[]; skipDetail: string }) {
  const steps = stepDurations(events)
  return (
    <ol className="mt-2 space-y-0.5 text-xs">
      {events.map((e, i) => (
        <li key={i} className={e.applied ? '' : 'opacity-50'}>
          <div className="flex flex-wrap items-baseline gap-x-2">
            <span className="font-mono text-fg">{e.state}</span>
            {e.reason && <span className="font-mono text-fg-subtle">{e.reason}</span>}
            <span className="text-fg-muted">{clockTime(e.ts)}</span>
            {steps[i] !== null && <span className="text-fg-subtle">+{duration(steps[i]!)}</span>}
            {e.source !== 'agent' && <span className="text-fg-subtle">from {e.source}</span>}
            {!e.applied && <span className="text-fg-subtle">not applied</span>}
          </div>
          {e.detail && e.detail !== skipDetail && (
            <div className="mt-1 mb-1">
              <div className="mb-1 text-fg-subtle">{detailCaption(e.source)}</div>
              <DetailText text={e.detail} />
            </div>
          )}
        </li>
      ))}
    </ol>
  )
}

// UpgradeDetails is everything known about one upgrade below its summary
// line: what the reason means, the timeline and the reported output. events
// is absent until the detail fetch lands, and empty for upgrades that
// predate the events table; both show the summary only.
export function UpgradeDetails({ upgrade, events }: { upgrade: UpgradeEntry; events?: UpgradeEvent[] | null }) {
  const failed = isFailed(upgrade.state)
  const closedBy = upgrade.closed_by || 'agent'
  const detail = upgrade.failure_detail ?? ''
  return (
    <div>
      {upgrade.failure_reason && (
        <div className="mt-1 text-fg-muted">{reasonText(upgrade.failure_reason, upgrade.to_version)}</div>
      )}
      {events && events.length > 0 && <Timeline events={events} skipDetail={detail} />}
      {detail ? (
        // Keyed so the block opens by itself when a running upgrade fails.
        <OutputBlock key={`${upgrade.id}:${upgrade.state}`} text={detail} source={closedBy} defaultOpen={failed} />
      ) : failed && closedBy === 'agent' && (
        <div className="mt-2 text-xs text-fg-subtle">{NO_DETAIL_TEXT}</div>
      )}
    </div>
  )
}

function HistoryRow({ device, upgrade }: { device: string; upgrade: UpgradeEntry }) {
  const [open, setOpen] = useState(false)
  const [detail, setDetail] = useState<UpgradeDetail | null>(null)
  const [error, setError] = useState('')

  const toggle = () => {
    setOpen(!open)
    if (open) return
    setError('')
    getUpgrade(device, upgrade.id).then(setDetail).catch((e) => setError((e as Error).message))
  }

  const took = between(upgrade.started_at, upgrade.finished_at)
  return (
    <li className="border-b border-border py-2 text-sm">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <span>{upgrade.from_version} &rarr; {upgrade.to_version}</span>
        <UpgradeStateBadge state={upgrade.state} />
        {upgrade.failure_reason && <span className="font-mono text-xs text-fg-subtle">{upgrade.failure_reason}</span>}
        <span className="ml-auto text-fg-subtle">{when(upgrade.started_at)}</span>
        <span className="text-fg-subtle">{took === null ? '-' : duration(took)}</span>
        <button className="text-link hover:underline" onClick={toggle}>{open ? 'hide' : 'details'}</button>
      </div>
      {open && (
        <div>
          <UpgradeDetails upgrade={detail ?? upgrade} events={detail?.events} />
          {error && <div className="mt-1 text-xs text-danger">{error}</div>}
        </div>
      )}
    </li>
  )
}

// UpgradeHistory lists earlier upgrades one line each. Opening a row fetches
// that upgrade's timeline and output.
export function UpgradeHistory({ device, upgrades }: { device: string; upgrades: UpgradeEntry[] }) {
  if (upgrades.length === 0) return null
  return (
    <div>
      <h3 className="text-sm font-medium">Earlier upgrades</h3>
      <ul>{upgrades.map((u) => <HistoryRow key={u.id} device={device} upgrade={u} />)}</ul>
    </div>
  )
}
