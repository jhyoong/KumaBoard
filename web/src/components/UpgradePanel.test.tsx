// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { UpgradePanel } from './UpgradePanel'
import type { Device, UpgradeEntry } from '../api'

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true

const device: Device = {
  name: 'linux-server', state: 'online', connected: true, os: 'linux', arch: 'amd64',
  agent_version: '0.4.0', protocol_version: 1, desired_agent_version: '0.5.0',
  capabilities: [], commands: [], mac: '', normally_off: false, schedule: null,
  last_seen: '', last_disconnect_at: '', incompatible: false, terminal_enabled: false,
  reject_reason: '', metrics: null,
}

function upgrade(state: string): UpgradeEntry {
  return {
    id: 'u1', device_id: 1, from_version: '0.4.0', to_version: '0.5.0', requested_by: 'admin',
    started_at: '2026-09-29T10:00:00Z', finished_at: null, state, failure_reason: '',
  }
}

type Reply = { status: number; body?: unknown }
let upgrades: UpgradeEntry[]
let retryReply: Reply
const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
  let r: Reply = { status: 200, body: [] }
  if (url.endsWith('/upgrades/retry') && init?.method === 'POST') r = retryReply
  else if (url.endsWith('/upgrades')) r = { status: 200, body: upgrades }
  return {
    ok: r.status >= 200 && r.status < 300,
    status: r.status,
    statusText: 'OK',
    json: async () => r.body ?? {},
  }
})

let root: Root | null = null
let container: HTMLDivElement

async function flush() {
  for (let i = 0; i < 5; i++) {
    await act(async () => { await new Promise((r) => setTimeout(r, 0)) })
  }
}

async function mount(d: Partial<Device> = {}) {
  container = document.createElement('div')
  document.body.appendChild(container)
  root = createRoot(container)
  await act(async () => { root!.render(<UpgradePanel device={{ ...device, ...d }} />) })
  await flush()
}

const button = (label: string) =>
  [...container.querySelectorAll('button')].find((b) => b.textContent === label)

beforeEach(() => {
  upgrades = []
  retryReply = { status: 202 }
  fetchMock.mockClear()
  vi.stubGlobal('fetch', fetchMock)
})

afterEach(async () => {
  await act(async () => { root?.unmount() })
  root = null
  container.remove()
  vi.unstubAllGlobals()
})

describe('UpgradePanel Upgrade now', () => {
  it('appears when the target differs from the running version and nothing is in flight', async () => {
    upgrades = [upgrade('verified')]
    await mount()
    expect(button('Upgrade now')).toBeDefined()
  })

  it('is absent when the versions match', async () => {
    await mount({ desired_agent_version: '0.4.0' })
    expect(button('Upgrade now')).toBeUndefined()
  })

  it('is absent while an upgrade is in flight', async () => {
    upgrades = [upgrade('downloading')]
    await mount()
    expect(button('Upgrade now')).toBeUndefined()
    expect(button('Abandon')).toBeDefined()
  })

  it('POSTs a retry and reports the dispatch', async () => {
    await mount()
    await act(async () => { button('Upgrade now')!.click() })
    await flush()

    expect(fetchMock).toHaveBeenCalledWith('/api/devices/linux-server/upgrades/retry', expect.objectContaining({ method: 'POST' }))
    const msg = [...container.querySelectorAll('div')].find((d) => d.textContent === 'Upgrade dispatched to linux-server')
    expect(msg?.className).toContain('text-fg-muted')
  })

  it('disables the button while the POST is in flight', async () => {
    let release!: () => void
    const gate = new Promise<void>((r) => { release = r })
    await mount()
    fetchMock.mockImplementationOnce(async () => {
      await gate
      return { ok: true, status: 202, statusText: 'OK', json: async () => ({}) }
    })
    await act(async () => { button('Upgrade now')!.click() })

    expect(button('Dispatching…')?.disabled).toBe(true)
    release()
    await flush()
    expect(container.textContent).toContain('Upgrade dispatched to linux-server')
  })

  it('shows the server refusal verbatim as an error', async () => {
    retryReply = { status: 409, body: { error: 'retry not dispatched: device offline' } }
    await mount()
    await act(async () => { button('Upgrade now')!.click() })
    await flush()

    const msg = [...container.querySelectorAll('div')].find((d) => d.textContent === 'retry not dispatched: device offline')
    expect(msg?.className).toContain('text-danger')
    expect(button('Upgrade now')).toBeDefined()
  })

  it('keeps the Retry button on a failed upgrade', async () => {
    upgrades = [upgrade('failed')]
    await mount()
    expect(button('Retry')).toBeDefined()
  })
})
