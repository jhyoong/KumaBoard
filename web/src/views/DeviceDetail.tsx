import { useEffect, useState, type ReactNode } from 'react'
import { useNavigate, useParams, Link } from 'react-router'
import { api, type CommandDef, type GPU, type HistorySample, type Metrics, type MetricsHistory, type Run, type Schedule } from '../api'
import { liveRun, type State } from '../state'
import { canStop, confirmRun, mergeRuns, runningRun } from '../commands'
import type { LiveStatus } from '../live'
import { DeviceRail } from '../components/DeviceRail'
import { StateBadge } from '../components/StateBadge'
import { RunList } from '../components/RunList'
import { TimeSeriesChart } from '../components/TimeSeriesChart'
import { ScheduleEditor } from '../components/ScheduleEditor'
import { UpgradePanel } from '../components/UpgradePanel'
import { bytes, uptime, when } from '../format'
import { gpuDetailRows, gpuSummary } from '../gpu'
import { TEMP_SCALE_MAX_C, tempClass, tempText } from '../temp'
import { RANGES, bytesScale, historySeries, mergeLive, type HistoryWindow } from '../history'

const CHART_RANGES: HistoryWindow[] = ['24h', '7d', '30d']

function pctOf(used: number, total: number): string {
  return total > 0 ? `${((used / total) * 100).toFixed(1)}%` : '-'
}

function StatCard({ title, value, children }: { title: ReactNode; value: string; children?: ReactNode }) {
  return (
    <div className="rounded-lg border border-border bg-surface p-3">
      <div className="text-xs text-fg-subtle">{title}</div>
      <div className="text-xl font-semibold text-fg">{value}</div>
      {children && <div className="mt-1 space-y-0.5 text-xs text-fg-muted">{children}</div>}
    </div>
  )
}

function GPUCard({ g }: { g: GPU }) {
  const rows = gpuDetailRows(g)
  return (
    <div className="rounded-lg border border-border bg-surface p-3">
      <div className="flex items-center gap-2">
        <span className="truncate text-sm font-medium text-fg" title={g.name}>{g.name}</span>
        {g.suspended && <span className="rounded bg-surface-muted px-1.5 text-xs text-fg-muted">suspended</span>}
      </div>
      <div className="text-xs text-fg-subtle">GPU{g.index} · {g.vendor}</div>
      {rows.length > 0 ? (
        <dl className="mt-1 grid grid-cols-[auto_1fr] gap-x-3 text-xs">
          {rows.map(([k, v]) => (
            <div key={k} className="contents">
              <dt className="text-fg-subtle">{k}</dt>
              <dd className="text-fg-muted">{v}</dd>
            </div>
          ))}
        </dl>
      ) : (
        <div className="mt-1 text-xs text-fg-muted">{gpuSummary(g)}</div>
      )}
    </div>
  )
}

function CurrentMetrics({ m }: { m: Metrics }) {
  return (
    <div className="space-y-3">
      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <StatCard title="CPU" value={`${m.cpu_percent.toFixed(1)}%`}>
          <div>Load {m.load1.toFixed(2)} / {m.load5.toFixed(2)} / {m.load15.toFixed(2)}</div>
          {m.temp_c !== undefined && (
            <div className={tempClass(m.temp_c) || undefined}>
              Temp {tempText(m.temp_c)}{m.temp_sensor && ` · ${m.temp_sensor}`}
            </div>
          )}
        </StatCard>
        <StatCard title="Memory" value={pctOf(m.mem_used_bytes, m.mem_total_bytes)}>
          <div>{bytes(m.mem_used_bytes)} / {bytes(m.mem_total_bytes)}</div>
        </StatCard>
        <StatCard title="Storage" value={`${m.disk_used_percent.toFixed(1)}%`}>
          <div>{bytes(m.disk_used_bytes)} / {bytes(m.disk_total_bytes)}</div>
        </StatCard>
        <StatCard title="Uptime" value={uptime(m.uptime_s)} />
      </div>
      {m.gpus && m.gpus.length > 0 && (
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
          {m.gpus.map((g) => <GPUCard key={g.index} g={g} />)}
        </div>
      )}
    </div>
  )
}

// MetricsCharts shows server-backed 15-minute history for the chosen range.
// 24h refetches every minute and folds the live SSE sample into its last bucket.
function MetricsCharts({ name, live }: { name: string; live: Metrics | null }) {
  const [win, setWin] = useState<HistoryWindow>('7d')
  const [data, setData] = useState<{ window: HistoryWindow; samples: HistorySample[] } | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    let cancelled = false
    const load = () => {
      api<MetricsHistory>(`/api/devices/${name}/metrics/history?window=${win}`)
        .then((h) => {
          if (cancelled) return
          setData({ window: win, samples: h.samples })
          setError('')
        })
        .catch((e) => { if (!cancelled) setError((e as Error).message) })
    }
    load()
    const id = win === '24h' ? setInterval(load, 60_000) : undefined
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [name, win])

  const range = RANGES[win]
  const now = Math.floor(Date.now() / 1000)
  const loaded = data?.window === win
  const samples = data && loaded ? mergeLive(data.samples, win === '24h' ? live : null, now, range) : []
  const s = historySeries(samples)
  const empty = loaded ? 'No data in this range' : 'Loading…'
  const common = { t: s.t, start: now - range.windowS, end: now, bucketS: range.bucketS, empty }

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-3">
        <div className="inline-flex overflow-hidden rounded border border-border-strong text-sm" role="group" aria-label="History range">
          {CHART_RANGES.map((r) => (
            <button
              key={r}
              aria-pressed={r === win}
              className={r === win ? 'bg-accent px-3 py-1 text-on-accent' : 'px-3 py-1 text-fg-muted hover:bg-surface-muted'}
              onClick={() => setWin(r)}
            >
              {r}
            </button>
          ))}
        </div>
        <span className="text-xs text-fg-subtle">15-minute averages</span>
        {error && <span className="text-xs text-danger">{error}</span>}
      </div>
      <div className="grid grid-cols-1 gap-3 lg:grid-cols-2 4xl:grid-cols-3">
        <TimeSeriesChart label="CPU %" values={s.cpu} color="var(--color-chart-1)" {...common} />
        <TimeSeriesChart label="Memory %" values={s.mem} color="var(--color-chart-2)" {...common} />
        <TimeSeriesChart label="Disk %" values={s.disk} color="var(--color-chart-3)" {...common} />
        <TimeSeriesChart
          label="GPU usage"
          values={s.gpu ?? []}
          color="var(--color-chart-4)"
          {...common}
          empty={loaded && samples.length > 0 ? 'No GPU reported in this range' : empty}
        />
        <TimeSeriesChart
          label="GPU memory"
          {...common}
          values={s.gpuMem?.used ?? []}
          reference={s.gpuMem?.total}
          max={s.gpuMem ? bytesScale(s.gpuMem.used, s.gpuMem.total) : 100}
          format={bytes}
          color="var(--color-chart-5)"
          empty={loaded && samples.length > 0 ? 'No GPU reported in this range' : empty}
        />
        <TimeSeriesChart
          label="Temperature °C"
          {...common}
          values={s.temp ?? []}
          max={TEMP_SCALE_MAX_C}
          format={(v) => `${v.toFixed(0)}°C`}
          color="var(--color-chart-3)"
          empty={loaded && samples.length > 0 ? 'No temperature reported in this range' : empty}
        />
      </div>
    </div>
  )
}

export function DeviceDetail({ state, live, onWake }: { state: State; live: LiveStatus; onWake: (name: string) => void }) {
  const { name = '' } = useParams()
  const navigate = useNavigate()
  const device = state.devices[name]
  const [fetched, setFetched] = useState<Run[]>([])
  const [msg, setMsg] = useState('')

  useEffect(() => {
    api<Run[]>(`/api/devices/${name}/runs`).then(setFetched).catch(() => {})
  }, [name])

  if (!device) {
    if (live.unauthenticated) return <p className="text-fg-subtle">Not signed in. Redirecting to login...</p>
    if (live.lastUpdate === null) return <p className="text-fg-subtle">Loading...</p>
    return <p className="text-fg-subtle">Unknown device.</p>
  }
  // Runs still in flight show their streamed output as it arrives.
  const runs = mergeRuns(state.runs[name] ?? [], fetched).map((r) => liveRun(r, state.output[r.id]))
  const stoppable = canStop(device)

  const run = async (c: CommandDef) => {
    if (!confirmRun(name, c)) return
    try {
      await api(`/api/devices/${name}/commands/${c.name}`, { method: 'POST' })
      setMsg(`${c.name} requested`)
    } catch (e) {
      setMsg(`${c.name}: ${(e as Error).message}`)
    }
  }

  const stop = async (r: Run) => {
    try {
      await api(`/api/runs/${r.id}/cancel`, { method: 'POST' })
      setMsg(`${r.command}: stop requested`)
    } catch (e) {
      setMsg(`${r.command}: ${(e as Error).message}`)
    }
  }

  const save = async (body: { mac: string; normally_off: boolean; schedule: Schedule; terminal_enabled: boolean }) => {
    await api(`/api/devices/${name}`, { method: 'PATCH', body: JSON.stringify(body) })
  }

  const revoke = async () => {
    if (!confirm(`Revoke the token for ${name}? The agent will be disconnected and must be re-registered.`)) return
    await api(`/api/devices/${name}/revoke`, { method: 'POST' })
    navigate('/')
  }

  return (
    <div className="3xl:flex 3xl:items-start 3xl:gap-6">
      <DeviceRail devices={Object.values(state.devices)} current={name} />
      <div className="space-y-6 3xl:min-w-0 3xl:flex-1">
        <div className="flex items-center gap-3">
          <h1 className="text-2xl font-semibold">{device.name}</h1>
          <StateBadge state={device.state} />
          <span className="text-sm text-fg-subtle">{device.os}/{device.arch} · agent {device.agent_version || '?'} · proto {device.protocol_version || '?'} · last seen {when(device.last_seen)}</span>
        </div>
        {device.incompatible && <div className="rounded bg-danger-soft p-2 text-sm text-danger">Incompatible agent, needs redeploy</div>}

        {/* One column below 2xl (the wrappers are display:contents, so order-*
            fixes the sequence); from 2xl a fluid primary pane plus a sticky aside. */}
        <div className="flex flex-col gap-6 2xl:flex-row 2xl:items-start">
          <div className="contents 2xl:block 2xl:min-w-0 2xl:flex-1 2xl:space-y-6">
            <section className="order-1">
              <h2 className="mb-2 font-medium">Current metrics</h2>
              {device.metrics
                ? <CurrentMetrics m={device.metrics} />
                : <p className="text-sm text-fg-subtle">No live metrics{device.connected ? ' yet' : ' while disconnected'}.</p>}
            </section>

            <section className="order-2">
              <h2 className="mb-2 font-medium">History</h2>
              <MetricsCharts name={name} live={device.connected ? device.metrics : null} />
            </section>

            <section className="order-5">
              <h2 className="mb-2 font-medium">Run history</h2>
              <RunList runs={runs} onStop={stoppable ? stop : undefined} />
            </section>
          </div>

          <aside className="contents 2xl:sticky 2xl:top-6 2xl:block 2xl:max-h-[calc(100vh-3rem)] 2xl:w-[26rem] 2xl:shrink-0 2xl:self-start 2xl:space-y-6 2xl:overflow-y-auto">
            <section className="order-3">
              <h2 className="mb-2 font-medium">Commands</h2>
              <div className="flex flex-wrap gap-2">
                {device.capabilities.includes('wol-target') && (
                  <button className="rounded bg-accent px-3 py-1 text-sm text-on-accent hover:bg-accent-hover disabled:opacity-50" disabled={device.connected} onClick={() => onWake(name)}>Wake</button>
                )}
                {device.connected && device.capabilities.includes('terminal') && (
                  <Link
                    to={`/devices/${name}/terminal`}
                    className="rounded bg-neutral-action px-3 py-1 text-sm text-on-accent hover:bg-neutral-action-hover"
                  >
                    Terminal
                  </Link>
                )}
                {device.commands.map((c) => {
                  const active = stoppable ? runningRun(runs, c.name) : undefined
                  return (
                    <span key={c.name} className="inline-flex">
                      <button title={c.description} className={`rounded border border-border-strong px-3 py-1 text-sm hover:bg-surface-muted disabled:opacity-50 ${active ? 'rounded-r-none' : ''}`} disabled={!device.connected} onClick={() => run(c)}>
                        {c.name}
                      </button>
                      {active && (
                        <button title={`Stop ${c.name}`} className="rounded-r border border-l-0 border-danger px-2 py-1 text-sm text-danger hover:bg-danger-soft" onClick={() => stop(active)}>Stop</button>
                      )}
                    </span>
                  )
                })}
                {device.commands.length === 0 && <span className="text-sm text-fg-subtle">No commands declared.</span>}
              </div>
              {msg && <div className="mt-2 text-sm text-fg-muted">{msg}</div>}
              {device.commands_config_error && (
                <div className="mt-2 rounded bg-danger-soft p-2 text-sm text-danger">
                  The agent could not reload its config, so these are the commands from the last good one: {device.commands_config_error}
                </div>
              )}
              {(device.command_problems ?? []).length > 0 && (
                <ul className="mt-2 space-y-1 text-sm text-fg-muted">
                  {(device.command_problems ?? []).map((p) => (
                    <li key={p.name}><span className="font-mono">{p.name}</span> not available: {p.reason}</li>
                  ))}
                </ul>
              )}
            </section>

            <section className="order-4">
              <h2 className="mb-2 font-medium">Agent upgrade</h2>
              <UpgradePanel device={device} />
            </section>

            <section className="order-6 max-w-lg">
              <h2 className="mb-2 font-medium">Settings</h2>
              <ScheduleEditor key={device.name + device.last_seen} device={device} onSave={save} />
              <button className="mt-4 text-sm text-danger hover:underline" onClick={revoke}>Revoke token</button>
            </section>
          </aside>
        </div>
      </div>
    </div>
  )
}
