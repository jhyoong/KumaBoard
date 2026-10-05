import { useEffect, useState } from 'react'
import { api, type Device, type UpgradeEntry, type ReleaseEntry } from '../api'
import { when } from '../format'

const IN_FLIGHT = new Set(['requested', 'downloading', 'verifying', 'selftest', 'swapped', 'restarting'])
const FAILED = new Set(['rolled_back', 'failed'])

export function UpgradePanel({ device }: { device: Device }) {
  const [upgrades, setUpgrades] = useState<UpgradeEntry[]>([])
  const [releases, setReleases] = useState<ReleaseEntry[]>([])
  const [target, setTarget] = useState(device.desired_agent_version || '')
  const [msg, setMsg] = useState('')
  const [msgIsError, setMsgIsError] = useState(false)
  const [dispatching, setDispatching] = useState(false)

  const showMsg = (text: string, isError = false) => {
    setMsg(text)
    setMsgIsError(isError)
  }

  const loadUpgrades = () => {
    api<UpgradeEntry[]>(`/api/devices/${device.name}/upgrades`).then(setUpgrades).catch(() => {})
  }

  useEffect(() => {
    loadUpgrades()
    if (device.os && device.arch) {
      api<ReleaseEntry[]>(`/api/releases?os=${device.os}&arch=${device.arch}`).then(setReleases).catch(() => {})
    }
  }, [device.name, device.os, device.arch])

  useEffect(() => {
    setTarget(device.desired_agent_version || '')
  }, [device.desired_agent_version])

  const latest = upgrades[0]
  const isInFlight = latest && IN_FLIGHT.has(latest.state)
  const isFailed = latest && FAILED.has(latest.state)
  const canUpgrade = target !== '' && target !== device.agent_version && !isInFlight

  const setVersion = async (v: string) => {
    try {
      await api(`/api/devices/${device.name}`, {
        method: 'PATCH',
        body: JSON.stringify({ desired_agent_version: v }),
      })
      setTarget(v)
      if (!v) {
        showMsg('Target cleared')
      } else if (v !== device.agent_version) {
        // The server dispatches the upgrade itself on PATCH.
        showMsg(`Target set to ${v} — upgrade dispatching`)
        loadUpgrades()
      } else {
        showMsg(`Target set to ${v}`)
      }
    } catch (e) {
      showMsg((e as Error).message, true)
    }
  }

  const retry = async () => {
    setDispatching(true)
    try {
      await api(`/api/devices/${device.name}/upgrades/retry`, { method: 'POST' })
      showMsg(`Upgrade dispatched to ${device.name}`)
      loadUpgrades()
    } catch (e) {
      showMsg((e as Error).message, true)
    } finally {
      setDispatching(false)
    }
  }

  const abandon = async () => {
    if (!latest || !confirm('Abandon this upgrade?')) return
    try {
      await api(`/api/devices/${device.name}/upgrades/${latest.id}/abandon`, { method: 'POST' })
      showMsg('Abandoned')
    } catch (e) {
      showMsg((e as Error).message, true)
    }
  }

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-3">
        <label className="text-sm font-medium">Target version</label>
        <select
          className="rounded border border-border-strong bg-inset px-2 py-1 text-sm"
          value={target}
          onChange={(e) => setVersion(e.target.value)}
        >
          <option value="">-- none --</option>
          {releases.map((r) => (
            <option key={r.version} value={r.version}>{r.version}</option>
          ))}
        </select>
        {canUpgrade && (
          <button
            className="rounded bg-accent px-3 py-1 text-xs text-on-accent hover:bg-accent-hover disabled:opacity-50"
            onClick={retry}
            disabled={dispatching}
          >
            {dispatching ? 'Dispatching…' : 'Upgrade now'}
          </button>
        )}
      </div>

      {latest && (
        <div className="rounded border border-border bg-surface p-3 text-sm">
          <div className="flex items-center gap-2">
            <span className="font-medium">Latest upgrade:</span>
            <span>{latest.from_version} &rarr; {latest.to_version}</span>
            <span className={`rounded px-2 py-0.5 text-xs font-medium ${
              latest.state === 'verified' ? 'bg-success-soft text-success' :
              isFailed ? 'bg-danger-soft text-danger' :
              'bg-warning-soft text-warning'
            }`}>{latest.state}</span>
          </div>
          {latest.failure_reason && (
            <div className="mt-1 text-fg-subtle">Reason: {latest.failure_reason}</div>
          )}
          <div className="mt-1 text-fg-subtle">Started {when(latest.started_at)}</div>
          <div className="mt-2 flex gap-2">
            {isFailed && <button className="rounded bg-accent px-3 py-1 text-xs text-on-accent hover:bg-accent-hover" onClick={retry}>Retry</button>}
            {isInFlight && <button className="rounded bg-danger-strong px-3 py-1 text-xs text-on-accent" onClick={abandon}>Abandon</button>}
          </div>
        </div>
      )}

      {msg && <div className={`text-sm ${msgIsError ? 'text-danger' : 'text-fg-muted'}`}>{msg}</div>}
    </div>
  )
}
