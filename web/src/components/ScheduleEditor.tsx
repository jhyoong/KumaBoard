import { useState } from 'react'
import type { Device, Schedule, Window } from '../api'

type Props = { device: Device; onSave: (body: { mac: string; normally_off: boolean; schedule: Schedule; terminal_enabled: boolean }) => Promise<void> }

type Form = { mac: string; normallyOff: boolean; grace: number; windows: Window[]; terminalEnabled: boolean }

function formOf(device: Device): Form {
  return {
    mac: device.mac,
    normallyOff: device.normally_off,
    grace: device.schedule?.grace_period_s ?? 0,
    windows: device.schedule?.expected_offline ?? [],
    terminalEnabled: device.terminal_enabled,
  }
}

// A draft holds the form's values in place of the device's. `until` is null
// while the operator has unsaved edits; after a save it is the stored
// settings the draft was saved over, and the draft is dropped once the device
// reports anything else. The device prop changes on every metrics message, so
// the form must not follow it while it is being edited.
type Draft = { form: Form; until: string | null }

export function ScheduleEditor({ device, onSave }: Props) {
  const stored = formOf(device)
  const storedKey = JSON.stringify(stored)
  const [draft, setDraft] = useState<Draft | null>(null)
  const [msg, setMsg] = useState('')

  const held = draft !== null && (draft.until === null || draft.until === storedKey)
  // Dropped for good, or the device returning to its earlier settings would
  // bring a saved draft back.
  if (draft !== null && !held) setDraft(null)
  const form = held ? draft.form : stored
  const { mac, normallyOff, grace, windows, terminalEnabled } = form

  const edit = (patch: Partial<Form>) => {
    setDraft({ form: { ...form, ...patch }, until: null })
    setMsg('')
  }
  const update = (i: number, patch: Partial<Window>) => edit({ windows: windows.map((w, j) => (j === i ? { ...w, ...patch } : w)) })

  const save = async () => {
    try {
      await onSave({ mac, normally_off: normallyOff, schedule: { expected_offline: windows, grace_period_s: grace }, terminal_enabled: terminalEnabled })
      // Edits made while the request was in flight stay as unsaved edits.
      setDraft((d) => (d !== null && d.until === null && d.form !== form ? d : { form, until: storedKey }))
      setMsg('saved')
    } catch (e) {
      setMsg((e as Error).message)
    }
  }

  return (
    <div className="space-y-3 text-sm">
      <label className="block">MAC<input className="mt-1 w-full rounded border border-border-strong bg-inset p-1" value={mac} onChange={(e) => edit({ mac: e.target.value })} /></label>
      <label className="flex items-center gap-2"><input type="checkbox" checked={normallyOff} onChange={(e) => edit({ normallyOff: e.target.checked })} />Normally off</label>
      <label className="flex items-center gap-2"><input type="checkbox" checked={terminalEnabled} onChange={(e) => edit({ terminalEnabled: e.target.checked })} />Terminal enabled</label>
      <label className="block">Grace period (seconds)<input type="number" className="mt-1 w-32 rounded border border-border-strong bg-inset p-1" value={grace} onChange={(e) => edit({ grace: Number(e.target.value) })} /></label>
      <div>
        <div className="mb-1 font-medium">Expected offline windows</div>
        {windows.map((w, i) => (
          <div key={i} className="mb-1 flex gap-2">
            <input className="w-24 rounded border border-border-strong bg-inset p-1" value={w.days} onChange={(e) => update(i, { days: e.target.value })} placeholder="* or mon,tue" />
            <input className="w-20 rounded border border-border-strong bg-inset p-1" value={w.from} onChange={(e) => update(i, { from: e.target.value })} placeholder="01:00" />
            <input className="w-20 rounded border border-border-strong bg-inset p-1" value={w.to} onChange={(e) => update(i, { to: e.target.value })} placeholder="05:00" />
            <button className="text-danger" onClick={() => edit({ windows: windows.filter((_, j) => j !== i) })}>remove</button>
          </div>
        ))}
        <button className="text-link hover:underline" onClick={() => edit({ windows: [...windows, { days: '*', from: '01:00', to: '05:00' }] })}>add window</button>
      </div>
      <div className="flex items-center gap-3">
        <button className="rounded bg-accent px-3 py-1 text-on-accent hover:bg-accent-hover" onClick={save}>Save</button>
        <span className="text-fg-subtle">{msg}</span>
      </div>
    </div>
  )
}
