import { useEffect, useRef, useState } from 'react'
import { Link } from 'react-router'
import {
  abandonUpgrade, api, getUpgrade, listUpgrades, retryUpgrade, setDesiredVersion,
  type Device, type ReleaseEntry, type UpgradeDetail, type UpgradeDispatch, type UpgradeEntry,
} from '../api'
import { when } from '../format'
import { age, dispatchParts, isFailed, isInFlight } from '../upgrade'
import { UpgradeDetails, UpgradeHistory, UpgradeStateBadge } from './UpgradeHistory'

// DispatchLine says what became of the server's last attempt to offer this
// device its target version.
function DispatchLine({ dispatch, device }: { dispatch: UpgradeDispatch; device: Device }) {
  const parts = dispatchParts(dispatch, {
    target: device.desired_agent_version, os: device.os, arch: device.arch, now: Date.now(),
  })
  const tone = dispatch.code === 'sent' ? 'text-fg-muted' : dispatch.code === 'send_failed' ? 'text-danger' : 'text-warning'
  return (
    <div className={`text-sm ${tone}`} data-dispatch={dispatch.code}>
      {parts.map((p, i) => {
        if (p.device) {
          return <Link key={i} to={`/devices/${p.device}`} className="font-semibold underline">{p.text}</Link>
        }
        return p.bold ? <strong key={i}>{p.text}</strong> : <span key={i}>{p.text}</span>
      })}
    </div>
  )
}

export function UpgradePanel({ device }: { device: Device }) {
  const [upgrades, setUpgrades] = useState<UpgradeEntry[]>([])
  // The newest upgrade with its events, once fetched.
  const [current, setCurrent] = useState<UpgradeDetail | null>(null)
  const [releases, setReleases] = useState<ReleaseEntry[]>([])
  const [target, setTarget] = useState(device.desired_agent_version || '')
  // The dispatch outcome a PATCH just returned, shown until the device
  // summary catches up. undefined: none, use the device's own.
  const [patched, setPatched] = useState<UpgradeDispatch | null | undefined>(undefined)
  const [msg, setMsg] = useState('')
  const [msgIsError, setMsgIsError] = useState(false)
  const [dispatching, setDispatching] = useState(false)
  // Counts loads so a slow response cannot overwrite a newer one.
  const loadSeq = useRef(0)

  const showMsg = (text: string, isError = false) => {
    setMsg(text)
    setMsgIsError(isError)
  }

  const loadUpgrades = () => {
    const seq = ++loadSeq.current
    listUpgrades(device.name).then((list) => {
      if (seq !== loadSeq.current) return
      setUpgrades(list)
      if (list.length === 0) return
      // The timeline is extra; without it the card shows the summary.
      getUpgrade(device.name, list[0].id)
        .then((d) => { if (seq === loadSeq.current) setCurrent(d) })
        .catch(() => {})
    }).catch(() => {})
  }

  // These three change only when an upgrade does, so the metrics republish
  // every 30 s does not refetch.
  useEffect(() => {
    loadUpgrades()
  }, [device.name, device.latest_upgrade?.id, device.latest_upgrade?.state, device.latest_upgrade?.updated_at])

  useEffect(() => {
    if (device.os && device.arch) {
      api<ReleaseEntry[]>(`/api/releases?os=${device.os}&arch=${device.arch}`).then(setReleases).catch(() => {})
    }
  }, [device.name, device.os, device.arch])

  useEffect(() => {
    setTarget(device.desired_agent_version || '')
  }, [device.desired_agent_version])

  useEffect(() => {
    setPatched(undefined)
  }, [device.name, device.upgrade_dispatch?.code, device.upgrade_dispatch?.at])

  const latest = upgrades[0]
  const inFlight = latest && isInFlight(latest.state)
  const failed = latest && isFailed(latest.state)
  const canUpgrade = target !== '' && target !== device.agent_version && !inFlight
  const dispatch = patched !== undefined ? patched : device.upgrade_dispatch ?? null
  // The sweep raises the flag on the device summary without touching
  // updated_at, so nothing is refetched; read it from there as well.
  const stalled = latest && inFlight
    && (latest.stalled || (device.latest_upgrade?.id === latest.id && device.latest_upgrade.stalled))

  const setVersion = async (v: string) => {
    try {
      // The server evaluates the upgrade itself on PATCH and says what
      // happened in the summary it returns.
      const d = await setDesiredVersion(device.name, v)
      setTarget(v)
      setPatched(d?.upgrade_dispatch ?? null)
      showMsg(v ? `Target set to ${v}` : 'Target cleared')
      if (v && v !== device.agent_version) loadUpgrades()
    } catch (e) {
      showMsg((e as Error).message, true)
    }
  }

  const retry = async () => {
    setDispatching(true)
    try {
      await retryUpgrade(device.name)
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
      await abandonUpgrade(device.name, latest.id)
      showMsg('Abandoned')
    } catch (e) {
      // 404 or 409: the upgrade is not what this page shows any more.
      showMsg((e as Error).message, true)
    }
    loadUpgrades()
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
      <div className="text-xs text-fg-subtle">
        Before the swap, the target version is run against this device's current config file. If it rejects the config, nothing is changed.
      </div>

      {dispatch && <DispatchLine dispatch={dispatch} device={device} />}

      {latest && (
        <div className="rounded border border-border bg-surface p-3 text-sm">
          <div className="flex flex-wrap items-center gap-2">
            <span className="font-medium">Latest upgrade:</span>
            <span>{latest.from_version} &rarr; {latest.to_version}</span>
            <UpgradeStateBadge state={latest.state} />
            {stalled && <span className="rounded bg-danger-soft px-2 py-0.5 text-xs font-medium text-danger">Stalled</span>}
          </div>
          {stalled && (
            <div className="mt-1 text-danger">
              No report for {age(latest.updated_at, Date.now()) || 'a while'}. If the device is down, fix it by hand and then Abandon.
            </div>
          )}
          <div className="mt-1 text-fg-subtle">Started {when(latest.started_at)}</div>
          <UpgradeDetails upgrade={latest} events={current?.id === latest.id ? current.events : undefined} />
          <div className="mt-2 flex gap-2">
            {failed && <button className="rounded bg-accent px-3 py-1 text-xs text-on-accent hover:bg-accent-hover" onClick={retry}>Retry</button>}
            {inFlight && <button className="rounded bg-danger-strong px-3 py-1 text-xs text-on-accent" onClick={abandon}>Abandon</button>}
          </div>
        </div>
      )}

      <UpgradeHistory device={device.name} upgrades={upgrades.slice(1)} />

      {msg && <div className={`text-sm ${msgIsError ? 'text-danger' : 'text-fg-muted'}`}>{msg}</div>}
    </div>
  )
}
