import { useState } from 'react'
import type { Device, Schedule, Window } from '../api'

type Props = { device: Device; onSave: (body: { mac: string; normally_off: boolean; schedule: Schedule; terminal_enabled: boolean }) => Promise<void> }

export function ScheduleEditor({ device, onSave }: Props) {
  const [mac, setMac] = useState(device.mac)
  const [normallyOff, setNormallyOff] = useState(device.normally_off)
  const [grace, setGrace] = useState(device.schedule?.grace_period_s ?? 0)
  const [windows, setWindows] = useState<Window[]>(device.schedule?.expected_offline ?? [])
  const [terminalEnabled, setTerminalEnabled] = useState(device.terminal_enabled)
  const [msg, setMsg] = useState('')

  const update = (i: number, patch: Partial<Window>) => setWindows(windows.map((w, j) => (j === i ? { ...w, ...patch } : w)))

  const save = async () => {
    try {
      await onSave({ mac, normally_off: normallyOff, schedule: { expected_offline: windows, grace_period_s: grace }, terminal_enabled: terminalEnabled })
      setMsg('saved')
    } catch (e) {
      setMsg((e as Error).message)
    }
  }

  return (
    <div className="space-y-3 text-sm">
      <label className="block">MAC<input className="mt-1 w-full rounded border border-border-strong bg-inset p-1" value={mac} onChange={(e) => setMac(e.target.value)} /></label>
      <label className="flex items-center gap-2"><input type="checkbox" checked={normallyOff} onChange={(e) => setNormallyOff(e.target.checked)} />Normally off</label>
      <label className="flex items-center gap-2"><input type="checkbox" checked={terminalEnabled} onChange={(e) => setTerminalEnabled(e.target.checked)} />Terminal enabled</label>
      <label className="block">Grace period (seconds)<input type="number" className="mt-1 w-32 rounded border border-border-strong bg-inset p-1" value={grace} onChange={(e) => setGrace(Number(e.target.value))} /></label>
      <div>
        <div className="mb-1 font-medium">Expected offline windows</div>
        {windows.map((w, i) => (
          <div key={i} className="mb-1 flex gap-2">
            <input className="w-24 rounded border border-border-strong bg-inset p-1" value={w.days} onChange={(e) => update(i, { days: e.target.value })} placeholder="* or mon,tue" />
            <input className="w-20 rounded border border-border-strong bg-inset p-1" value={w.from} onChange={(e) => update(i, { from: e.target.value })} placeholder="01:00" />
            <input className="w-20 rounded border border-border-strong bg-inset p-1" value={w.to} onChange={(e) => update(i, { to: e.target.value })} placeholder="05:00" />
            <button className="text-danger" onClick={() => setWindows(windows.filter((_, j) => j !== i))}>remove</button>
          </div>
        ))}
        <button className="text-link hover:underline" onClick={() => setWindows([...windows, { days: '*', from: '01:00', to: '05:00' }])}>add window</button>
      </div>
      <div className="flex items-center gap-3">
        <button className="rounded bg-accent px-3 py-1 text-on-accent hover:bg-accent-hover" onClick={save}>Save</button>
        <span className="text-fg-subtle">{msg}</span>
      </div>
    </div>
  )
}
