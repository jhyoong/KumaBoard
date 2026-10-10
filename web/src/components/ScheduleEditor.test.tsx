// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { ScheduleEditor } from './ScheduleEditor'
import type { Device } from '../api'

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true

const device: Device = {
  name: 'linux-server', state: 'online', connected: true, os: 'linux', arch: 'amd64',
  agent_version: '0.4.0', protocol_version: 1, desired_agent_version: '',
  capabilities: [], commands: [], mac: '', normally_off: false, schedule: null,
  last_seen: '2026-10-10T08:00:00Z', last_disconnect_at: '', incompatible: false, terminal_enabled: false,
  reject_reason: '', metrics: null, upgrade_dispatch: null, latest_upgrade: null,
}

type Body = Parameters<Parameters<typeof ScheduleEditor>[0]['onSave']>[0]

let root: Root | null = null
let container: HTMLDivElement
const onSave = vi.fn(async (_body: Body) => {})

// render delivers a device summary; after the first call it stands in for a
// "device" or "metrics" SSE event.
async function render(d: Partial<Device> = {}) {
  if (!root) {
    container = document.createElement('div')
    document.body.appendChild(container)
    root = createRoot(container)
  }
  await act(async () => { root!.render(<ScheduleEditor key={device.name} device={{ ...device, ...d }} onSave={onSave} />) })
}

const checkbox = (label: string) =>
  [...container.querySelectorAll('label')].find((l) => l.textContent === label)!.querySelector('input')!

async function click(el: HTMLElement) {
  await act(async () => { el.click() })
}

const save = () => click([...container.querySelectorAll('button')].find((b) => b.textContent === 'Save')!)

afterEach(() => {
  act(() => root?.unmount())
  root = null
  container.remove()
  onSave.mockClear()
})

describe('ScheduleEditor', () => {
  it('keeps unsaved edits when a metrics message changes last_seen', async () => {
    await render()
    await click(checkbox('Terminal enabled'))
    await render({ last_seen: '2026-10-10T08:00:30Z' })
    expect(checkbox('Terminal enabled').checked).toBe(true)
  })

  it('follows the device while the form is clean', async () => {
    await render()
    await render({ normally_off: true })
    expect(checkbox('Normally off').checked).toBe(true)
  })

  it('saves a device with no MAC, sending the edited fields', async () => {
    await render()
    await click(checkbox('Terminal enabled'))
    await save()
    expect(onSave).toHaveBeenCalledWith({
      mac: '', normally_off: false, terminal_enabled: true,
      schedule: { expected_offline: [], grace_period_s: 0 },
    })
    expect(container.textContent).toContain('saved')
  })

  it('shows the saved values until the device reports them, then follows it again', async () => {
    await render()
    await click(checkbox('Terminal enabled'))
    await save()
    // Metrics arrive before the device update: still the saved values.
    await render({ last_seen: '2026-10-10T08:00:30Z' })
    expect(checkbox('Terminal enabled').checked).toBe(true)
    await render({ terminal_enabled: true })
    expect(checkbox('Terminal enabled').checked).toBe(true)
    // Clean again, so a change made elsewhere shows up.
    await render({ terminal_enabled: false })
    expect(checkbox('Terminal enabled').checked).toBe(false)
  })
})
