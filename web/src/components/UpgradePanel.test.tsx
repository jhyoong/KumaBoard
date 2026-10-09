// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { MemoryRouter } from 'react-router'
import { UpgradePanel } from './UpgradePanel'
import type { Device, LatestUpgrade, UpgradeDetail, UpgradeDispatch, UpgradeEntry, UpgradeEvent } from '../api'

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true

const device: Device = {
  name: 'linux-server', state: 'online', connected: true, os: 'linux', arch: 'amd64',
  agent_version: '0.4.0', protocol_version: 1, desired_agent_version: '0.5.0',
  capabilities: [], commands: [], mac: '', normally_off: false, schedule: null,
  last_seen: '', last_disconnect_at: '', incompatible: false, terminal_enabled: false,
  reject_reason: '', metrics: null, upgrade_dispatch: null, latest_upgrade: null,
}

function upgrade(state: string, over: Partial<UpgradeEntry> = {}): UpgradeEntry {
  return {
    id: 'u1', device_id: 1, from_version: '0.4.0', to_version: '0.5.0', requested_by: 'admin',
    started_at: '2026-09-29T10:00:00Z', updated_at: '2026-09-29T10:00:00Z', finished_at: null, state,
    failure_reason: '', failure_detail: '', closed_by: '', stalled: false, ...over,
  }
}

function event(state: string, ts: string, over: Partial<UpgradeEvent> = {}): UpgradeEvent {
  return { ts, source: 'agent', state, reason: '', detail: '', applied: true, ...over }
}

function dispatch(code: string, over: Partial<UpgradeDispatch> = {}): UpgradeDispatch {
  return { code, message: '', blocking_device: '', upgrade_id: '', at: '2026-09-29T10:00:00Z', ...over }
}

function latestOf(u: UpgradeEntry, over: Partial<LatestUpgrade> = {}): LatestUpgrade {
  return {
    id: u.id, from_version: u.from_version, to_version: u.to_version, state: u.state,
    failure_reason: u.failure_reason, updated_at: u.updated_at, stalled: u.stalled, ...over,
  }
}

type Reply = { status: number; body?: unknown }
let upgrades: UpgradeEntry[]
// Events per upgrade id, served by the detail endpoint.
let events: Record<string, UpgradeEvent[] | null>
let retryReply: Reply
let abandonReply: Reply
let patchReply: Reply
const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
  const path = url.split('?')[0]
  const one = /\/upgrades\/([^/]+)$/.exec(path)
  let r: Reply = { status: 200, body: [] }
  if (path.endsWith('/upgrades/retry') && init?.method === 'POST') r = retryReply
  else if (path.endsWith('/abandon') && init?.method === 'POST') r = abandonReply
  else if (init?.method === 'PATCH') r = patchReply
  else if (path.endsWith('/upgrades')) r = { status: 200, body: upgrades }
  else if (path === '/api/releases') {
    r = { status: 200, body: ['0.5.0', '0.6.0'].map((version) => ({ version, os: 'linux', arch: 'amd64', added_at: '' })) }
  } else if (one) {
    const u = upgrades.find((x) => x.id === one[1])
    r = u
      ? { status: 200, body: { ...u, events: events[u.id] ?? [] } satisfies UpgradeDetail }
      : { status: 404, body: { error: 'not found' } }
  }
  return {
    ok: r.status >= 200 && r.status < 300,
    status: r.status,
    statusText: 'OK',
    json: async () => r.body ?? {},
  }
})

// calls counts the requests to one path, ignoring the query string.
const calls = (path: string, method = 'GET') =>
  fetchMock.mock.calls.filter(([url, init]) => url.split('?')[0] === path && (init?.method ?? 'GET') === method).length

const LIST = '/api/devices/linux-server/upgrades'

let root: Root | null = null
let container: HTMLDivElement

async function flush() {
  for (let i = 0; i < 5; i++) {
    await act(async () => { await new Promise((r) => setTimeout(r, 0)) })
  }
}

// The panel links to other device pages, so it needs a router around it.
const panel = (d: Partial<Device>) => <MemoryRouter><UpgradePanel device={{ ...device, ...d }} /></MemoryRouter>

async function mount(d: Partial<Device> = {}) {
  container = document.createElement('div')
  document.body.appendChild(container)
  root = createRoot(container)
  await act(async () => { root!.render(panel(d)) })
  await flush()
}

// rerender delivers a new device summary, as a "device" or "metrics" SSE
// event would.
async function rerender(d: Partial<Device> = {}) {
  await act(async () => { root!.render(panel(d)) })
  await flush()
}

const button = (label: string) =>
  [...container.querySelectorAll('button')].find((b) => b.textContent === label)

async function click(label: string) {
  await act(async () => { button(label)!.click() })
  await flush()
}

beforeEach(() => {
  upgrades = []
  events = {}
  retryReply = { status: 202 }
  abandonReply = { status: 200, body: { status: 'abandoned' } }
  patchReply = { status: 200, body: device }
  fetchMock.mockClear()
  vi.stubGlobal('fetch', fetchMock)
  vi.stubGlobal('confirm', () => true)
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

describe('UpgradePanel dispatch line', () => {
  const line = () => container.querySelector('[data-dispatch]')

  it('is absent when the server has nothing to report', async () => {
    await mount()
    expect(line()).toBeNull()
  })

  it.each([
    ['sent', {}, 'Upgrade request sent.'],
    ['waiting', {}, 'Not dispatched yet: the upgrade that was blocking this one has ended. Press Upgrade now.'],
    ['prev_failed', {}, 'Not dispatched: an earlier upgrade to 0.5.0 failed on this device. Fix the cause below, then press Retry.'],
    ['no_release', {}, 'Not dispatched: no 0.5.0 release has been ingested for linux/amd64.'],
    ['not_connected', {}, 'Not dispatched: the device is offline. It will be offered 0.5.0 when it next connects.'],
    ['send_failed', { message: 'write: broken pipe' }, 'Not dispatched: the request could not be sent. write: broken pipe'],
  ] as [string, Partial<UpgradeDispatch>, string][])('shows %s', async (code, over, text) => {
    await mount({ upgrade_dispatch: dispatch(code, over) })
    expect(line()?.textContent).toBe(text)
    expect(line()?.querySelector('a')).toBeNull()
  })

  it('links in_flight to the blocking device', async () => {
    await mount({
      upgrade_dispatch: dispatch('in_flight', { blocking_device: 'mac-desktop', blocking_state: 'restarting' }),
    })
    expect(line()?.textContent)
      .toBe('Not dispatched: an upgrade on mac-desktop is in progress (restarting). One upgrade runs at a time.')
    const a = line()!.querySelector('a')!
    expect(a.textContent).toBe('mac-desktop')
    expect(a.getAttribute('href')).toBe('/devices/mac-desktop')
  })

  async function pick(version: string) {
    const select = container.querySelector('select')!
    await act(async () => {
      select.value = version
      select.dispatchEvent(new Event('change', { bubbles: true }))
    })
    await flush()
  }

  it('reports what the PATCH response says, not "upgrade dispatching"', async () => {
    patchReply = {
      status: 200,
      body: { ...device, desired_agent_version: '0.6.0', upgrade_dispatch: dispatch('in_flight', { blocking_device: 'mac-desktop' }) },
    }
    await mount()
    await pick('0.6.0')

    expect(calls('/api/devices/linux-server', 'PATCH')).toBe(1)
    expect(container.textContent).not.toContain('upgrade dispatching')
    expect(container.textContent).toContain('Target set to 0.6.0')
    expect(line()?.textContent).toContain('Not dispatched: an upgrade on mac-desktop is in progress')
  })

  it('shows "sent" from the PATCH response until the device summary moves on', async () => {
    patchReply = { status: 200, body: { ...device, desired_agent_version: '0.6.0', upgrade_dispatch: dispatch('sent') } }
    await mount()
    await pick('0.6.0')
    expect(line()?.textContent).toBe('Upgrade request sent.')

    await rerender({ desired_agent_version: '0.6.0', upgrade_dispatch: dispatch('send_failed', { at: '2026-09-29T10:00:05Z' }) })
    expect(line()?.textContent).toBe('Not dispatched: the request could not be sent.')
  })
})

describe('UpgradePanel current upgrade', () => {
  const failed = (over: Partial<UpgradeEntry> = {}) => upgrade('failed', {
    finished_at: '2026-09-29T10:00:03Z', updated_at: '2026-09-29T10:00:03Z',
    failure_reason: 'selftest_config_rejected', closed_by: 'agent',
    failure_detail: 'selftest of 0.5.0 exited 1 after 0.4s\n--- stderr (last 4 KiB) ---\nerror: selftest: config: bad', ...over,
  })

  it('explains the reason code', async () => {
    upgrades = [failed()]
    await mount()
    expect(container.textContent).toContain("Version 0.5.0 refused this device's current config file.")
  })

  it('shows an unknown reason code as it is', async () => {
    upgrades = [failed({ failure_reason: 'some_new_reason' })]
    await mount()
    expect(container.textContent).toContain('some_new_reason')
  })

  it('shows the agent output as received, open when the upgrade failed', async () => {
    upgrades = [failed()]
    await mount()
    const pre = container.querySelector('pre')!
    expect(pre.textContent).toBe(upgrades[0].failure_detail)
    expect(pre.className).toContain('whitespace-pre-wrap')
    expect(container.textContent).toContain('Reported by the agent, shown as received')
    expect(button('Hide output')).toBeDefined()
  })

  it('renders markup in the detail as text, creating no element', async () => {
    const detail = '<img src=x onerror="window.pwned=1"> <a href="https://evil.example">click</a> [x](https://evil.example) https://evil.example'
    upgrades = [failed({ failure_detail: detail })]
    events = { u1: [event('failed', '2026-09-29T10:00:03Z', { reason: 'selftest_failed', detail: '<script>window.pwned=1</script>' })] }
    await mount()

    const pres = [...container.querySelectorAll('pre')]
    expect(pres.map((p) => p.textContent)).toEqual(['<script>window.pwned=1</script>', detail])
    for (const pre of pres) expect(pre.children.length).toBe(0)
    expect(container.querySelector('img')).toBeNull()
    expect(container.querySelector('script')).toBeNull()
    expect(container.querySelector('a')).toBeNull()
    expect((window as { pwned?: number }).pwned).toBeUndefined()
  })

  it('copies the raw detail string', async () => {
    const writeText = vi.fn(async () => {})
    vi.stubGlobal('navigator', { clipboard: { writeText } })
    upgrades = [failed()]
    await mount()
    await click('Copy')
    expect(writeText).toHaveBeenCalledWith(upgrades[0].failure_detail)
    expect(button('Copied')).toBeDefined()
  })

  it('keeps the output closed until asked when the upgrade did not fail', async () => {
    upgrades = [upgrade('verified', { failure_detail: 'note', closed_by: 'agent' })]
    await mount()
    expect(container.querySelector('pre')).toBeNull()
    await click('Show output')
    expect(container.querySelector('pre')?.textContent).toBe('note')
  })

  it('does not caption server text as the agent\'s', async () => {
    upgrades = [failed({ failure_reason: 'timed_out', closed_by: 'server', failure_detail: 'no report from the agent for 15m while downloading' })]
    await mount()
    expect(container.textContent).toContain('Recorded by the server')
    expect(container.textContent).not.toContain('Reported by the agent')
  })

  it('says so when an old agent sent no detail', async () => {
    upgrades = [failed({ failure_detail: '', failure_reason: 'selftest_failed' })]
    await mount()
    expect(container.textContent).toContain('This agent version does not report details.')
    expect(container.querySelector('pre')).toBeNull()
  })

  it('does not blame the agent for an abandoned upgrade without detail', async () => {
    upgrades = [failed({ failure_detail: '', failure_reason: 'abandoned', closed_by: 'admin' })]
    await mount()
    expect(container.textContent).toContain('Abandoned by an operator.')
    expect(container.textContent).not.toContain('does not report details')
  })

  it('draws the timeline with step times, sources and late events greyed', async () => {
    upgrades = [failed({ failure_detail: '' })]
    events = {
      u1: [
        event('requested', '2026-09-29T10:00:00Z', { source: 'server' }),
        event('downloading', '2026-09-29T10:00:02Z'),
        event('selftest', '2026-09-29T10:01:32Z'),
        event('failed', '2026-09-29T10:01:32.400Z', { reason: 'selftest_config_rejected' }),
        event('verifying', '2026-09-29T10:01:40Z', { applied: false }),
      ],
    }
    await mount()

    expect(fetchMock).toHaveBeenCalledWith(`${LIST}/u1`, expect.anything())
    const rows = [...container.querySelectorAll('ol li')]
    expect(rows).toHaveLength(5)
    expect(rows[0].textContent).toContain('requested')
    expect(rows[0].textContent).toContain('from server')
    expect(rows[0].textContent).not.toContain('+')
    expect(rows[0].textContent).toContain(new Date('2026-09-29T10:00:00Z').toLocaleTimeString())
    expect(rows[1].textContent).toContain('+2.0s')
    expect(rows[1].textContent).not.toContain('from ')
    expect(rows[2].textContent).toContain('+1m 30s')
    expect(rows[3].textContent).toContain('selftest_config_rejected')
    expect(rows[3].textContent).toContain('+0.4s')
    expect(rows[3].className).not.toContain('opacity-50')
    expect(rows[4].className).toContain('opacity-50')
    expect(rows[4].textContent).toContain('not applied')
  })

  it('shows the summary only for an upgrade with no events', async () => {
    upgrades = [failed()]
    events = { u1: null }
    await mount()
    expect(container.querySelector('ol')).toBeNull()
    expect(container.textContent).toContain('0.4.0 \u2192 0.5.0')
    expect(container.querySelector('pre')?.textContent).toBe(upgrades[0].failure_detail)
  })

  it('flags a stalled upgrade', async () => {
    upgrades = [upgrade('restarting', { stalled: true, updated_at: new Date(Date.now() - 12 * 60_000).toISOString() })]
    await mount()
    const badge = [...container.querySelectorAll('span')].find((s) => s.textContent === 'Stalled')
    expect(badge).toBeDefined()
    expect(container.textContent).toContain('No report for 12m. If the device is down, fix it by hand and then Abandon')
    expect(button('Abandon')).toBeDefined()
  })

  it('picks the stalled flag up from the device summary without refetching', async () => {
    upgrades = [upgrade('restarting')]
    await mount({ latest_upgrade: latestOf(upgrades[0]) })
    expect(container.textContent).not.toContain('Stalled')
    const before = calls(LIST)

    await rerender({ latest_upgrade: latestOf(upgrades[0], { stalled: true }) })
    expect(container.textContent).toContain('Stalled')
    expect(calls(LIST)).toBe(before)
  })

  it('shows no stalled badge otherwise', async () => {
    upgrades = [upgrade('restarting')]
    await mount()
    expect(container.textContent).not.toContain('Stalled')
    expect(container.textContent).not.toContain('No report for')
  })
})

describe('UpgradePanel history', () => {
  beforeEach(() => {
    upgrades = [
      upgrade('verified', { id: 'u3', from_version: '0.4.0', to_version: '0.5.0' }),
      upgrade('failed', {
        id: 'u2', from_version: '0.3.0', to_version: '0.4.0', failure_reason: 'download_failed', closed_by: 'agent',
        failure_detail: 'GET /releases/x: status 404', started_at: '2026-09-28T10:00:00Z', finished_at: '2026-09-28T10:00:07Z',
      }),
      upgrade('verified', {
        id: 'u1', from_version: '0.2.0', to_version: '0.3.0', started_at: '2026-09-27T10:00:00Z', finished_at: '2026-09-27T10:01:30Z',
      }),
    ]
    events = { u2: [event('requested', '2026-09-28T10:00:00Z', { source: 'server' }), event('failed', '2026-09-28T10:00:07Z', { reason: 'download_failed' })] }
  })

  const rows = () => [...container.querySelectorAll('ul > li')]

  it('lists the remaining upgrades one line each', async () => {
    await mount()
    expect(rows()).toHaveLength(2)
    expect(rows()[0].textContent).toContain('0.3.0 \u2192 0.4.0')
    expect(rows()[0].textContent).toContain('failed')
    expect(rows()[0].textContent).toContain('download_failed')
    expect(rows()[0].textContent).toContain('7.0s')
    expect(rows()[1].textContent).toContain('0.2.0 \u2192 0.3.0')
    expect(rows()[1].textContent).toContain('1m 30s')
    // Collapsed: no explanation, no output.
    expect(rows()[0].textContent).not.toContain('The agent could not download')
    expect(rows()[0].querySelector('pre')).toBeNull()
  })

  it('fetches the detail endpoint when a row is expanded', async () => {
    await mount()
    expect(calls(`${LIST}/u2`)).toBe(0)
    await act(async () => { rows()[0].querySelector('button')!.click() })
    await flush()

    expect(calls(`${LIST}/u2`)).toBe(1)
    expect(calls(`${LIST}/u1`)).toBe(0)
    expect(rows()[0].textContent).toContain('The agent could not download the new binary. The device was not changed.')
    expect(rows()[0].querySelectorAll('ol li')).toHaveLength(2)
    expect(rows()[0].querySelector('pre')?.textContent).toBe('GET /releases/x: status 404')
  })

  it('shows nothing when there is only the current upgrade', async () => {
    upgrades = upgrades.slice(0, 1)
    await mount()
    expect(rows()).toHaveLength(0)
    expect(container.textContent).not.toContain('Earlier upgrades')
  })
})

describe('UpgradePanel live refresh', () => {
  it('asks for a bounded list', async () => {
    await mount()
    expect(fetchMock).toHaveBeenCalledWith(`${LIST}?limit=20`, expect.anything())
  })

  it('refetches when the latest upgrade changes', async () => {
    upgrades = [upgrade('downloading')]
    await mount({ latest_upgrade: latestOf(upgrades[0]) })
    expect(calls(LIST)).toBe(1)
    expect(container.textContent).toContain('downloading')

    upgrades = [upgrade('verifying', { updated_at: '2026-09-29T10:00:09Z' })]
    await rerender({ latest_upgrade: latestOf(upgrades[0]) })
    expect(calls(LIST)).toBe(2)
    expect(container.textContent).toContain('verifying')

    // An annotation moves updated_at and nothing else.
    await rerender({ latest_upgrade: latestOf(upgrades[0], { updated_at: '2026-09-29T10:00:20Z' }) })
    expect(calls(LIST)).toBe(3)
  })

  it('does not refetch on an unrelated device change', async () => {
    upgrades = [upgrade('downloading')]
    const latest = latestOf(upgrades[0])
    await mount({ latest_upgrade: latest })
    const before = fetchMock.mock.calls.length

    await rerender({
      latest_upgrade: { ...latest }, last_seen: '2026-09-29T10:00:30Z', state: 'stale',
      metrics: {
        cpu_percent: 50, mem_total_bytes: 1, mem_used_bytes: 1, mem_used_percent: 100, disk_total_bytes: 1,
        disk_used_bytes: 1, disk_used_percent: 100, uptime_s: 1, load1: 0, load5: 0, load15: 0,
      },
    })
    expect(fetchMock.mock.calls.length).toBe(before)
  })

  it('reloads after an abandon', async () => {
    upgrades = [upgrade('downloading')]
    await mount()
    expect(calls(LIST)).toBe(1)

    upgrades = [upgrade('failed', { failure_reason: 'abandoned', closed_by: 'admin' })]
    await click('Abandon')

    expect(calls(`${LIST}/u1/abandon`, 'POST')).toBe(1)
    expect(calls(LIST)).toBe(2)
    expect(container.textContent).toContain('Abandoned by an operator.')
    expect(button('Abandon')).toBeUndefined()
    expect(button('Retry')).toBeDefined()
  })

  it('reloads and shows the refusal when the upgrade was no longer in flight', async () => {
    upgrades = [upgrade('downloading')]
    abandonReply = { status: 409, body: { error: 'upgrade is not in flight', code: 'not_in_flight' } }
    await mount()

    upgrades = [upgrade('verified')]
    await click('Abandon')

    const msg = [...container.querySelectorAll('div')].find((d) => d.textContent === 'upgrade is not in flight')
    expect(msg?.className).toContain('text-danger')
    expect(calls(LIST)).toBe(2)
    expect(button('Abandon')).toBeUndefined()
  })
})
