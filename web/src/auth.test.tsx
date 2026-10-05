// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import App from './App'
import { api } from './api'
import { loginURL, resetAuthRedirect, safeNext } from './auth'

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true

// FakeEventSource stands in for /api/events. A 401 on an EventSource is only
// visible as onerror, which is what the browser delivers.
class FakeEventSource {
  static instances: FakeEventSource[] = []
  onopen: ((ev: Event) => void) | null = null
  onerror: ((ev: Event) => void) | null = null
  closed = false
  constructor() { FakeEventSource.instances.push(this) }
  addEventListener() {}
  close() { this.closed = true }
}

type Reply = { status: number; body?: unknown }
let reply: (url: string, init?: RequestInit) => Reply
const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
  const r = reply(url, init)
  return {
    ok: r.status >= 200 && r.status < 300,
    status: r.status,
    statusText: r.status === 401 ? 'Unauthorized' : 'OK',
    json: async () => r.body ?? {},
  }
})

const unauthorized: Reply = { status: 401, body: { error: 'unauthorized' } }
const device = { name: 'foo', state: 'online', connected: true, commands: [], capabilities: [], metrics: null, schedule: null, os: 'linux', arch: 'amd64' }

// authed answers like a logged-in server with one device.
function authed(url: string): Reply {
  if (url === '/api/devices') return { status: 200, body: [device] }
  if (url.includes('/metrics/history')) return { status: 200, body: { device: 'foo', window: '1h', samples: [] } }
  return { status: 200, body: [] }
}

let root: Root | null = null
let container: HTMLDivElement
let loginNavs: string[]

async function flush() {
  for (let i = 0; i < 5; i++) {
    await act(async () => { await new Promise((r) => setTimeout(r, 0)) })
  }
}

async function mount(path: string) {
  window.history.replaceState(null, '', path)
  loginNavs = []
  container = document.createElement('div')
  document.body.appendChild(container)
  root = createRoot(container)
  await act(async () => { root!.render(<App />) })
  await flush()
}

const failStreams = () => FakeEventSource.instances.forEach((s) => s.onerror?.(new Event('error')))
const devicesFetches = () => fetchMock.mock.calls.filter(([u]) => u === '/api/devices').length

async function submitLogin() {
  const form = container.querySelector('form')!
  await act(async () => { form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })) })
  await flush()
}

beforeEach(() => {
  resetAuthRedirect()
  FakeEventSource.instances = []
  fetchMock.mockClear()
  vi.stubGlobal('fetch', fetchMock)
  vi.stubGlobal('EventSource', FakeEventSource)
  vi.stubGlobal('ResizeObserver', class { observe() {} unobserve() {} disconnect() {} })
  // Count router navigations that land on /login (push or replace).
  for (const m of ['pushState', 'replaceState'] as const) {
    const orig = History.prototype[m]
    vi.spyOn(window.history, m).mockImplementation(function (this: History, ...args: Parameters<History['pushState']>) {
      if (String(args[2] ?? '').includes('/login')) loginNavs.push(String(args[2]))
      return orig.apply(this, args)
    })
  }
})

afterEach(async () => {
  await act(async () => { root?.unmount() })
  root = null
  container.remove()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('auth redirect', () => {
  it('sends a fresh session at / to /login exactly once', async () => {
    reply = () => unauthorized
    await mount('/')
    failStreams()
    await flush()

    expect(window.location.pathname).toBe('/login')
    expect(window.location.search).toBe('')
    expect(loginNavs).toHaveLength(1)
    expect(container.textContent).toContain('Sign in')
    expect(container.textContent).not.toContain('No devices registered yet')
    // The dashboard's stream is torn down and not retried.
    expect(FakeEventSource.instances.every((s) => s.closed)).toBe(true)
    const fetches = fetchMock.mock.calls.length
    await flush()
    expect(fetchMock.mock.calls.length).toBe(fetches)
  })

  it('redirects once, with next, when concurrent calls hit 401 mid-use', async () => {
    reply = authed
    await mount('/devices/foo')
    await act(async () => { FakeEventSource.instances[0].onopen?.(new Event('open')) })
    await flush()
    expect(devicesFetches()).toBe(1)
    expect(container.textContent).toContain('foo')

    // Session expires: the stream drops and several calls fail together.
    reply = () => unauthorized
    await act(async () => {
      failStreams()
      await Promise.allSettled([api('/api/audit'), api('/api/devices'), api('/api/releases')])
    })
    await flush()

    expect(loginNavs).toEqual(['/login?next=%2Fdevices%2Ffoo'])
    expect(window.location.pathname).toBe('/login')
    expect(container.textContent).toContain('Sign in')
    expect(container.textContent).not.toContain('Unknown device')
  })

  it('returns to the preserved next destination after login', async () => {
    reply = authed
    await mount('/login?next=%2Fdevices%2Ffoo')
    await submitLogin()

    expect(fetchMock).toHaveBeenCalledWith('/api/login', expect.objectContaining({ method: 'POST' }))
    expect(window.location.pathname).toBe('/devices/foo')
  })

  it('stays on /login and shows the error when the login POST is 401', async () => {
    reply = (url) => (url === '/api/login' ? { status: 401, body: { error: 'invalid credentials' } } : unauthorized)
    await mount('/login?next=%2Faudit')
    await submitLogin()

    expect(window.location.pathname).toBe('/login')
    expect(window.location.search).toBe('?next=%2Faudit')
    expect(loginNavs).toHaveLength(0)
    expect(container.textContent).toContain('invalid credentials')
  })
})

describe('safeNext', () => {
  it('accepts app paths and rejects off-site or login targets', () => {
    expect(safeNext('/devices/foo?x=1')).toBe('/devices/foo?x=1')
    expect(safeNext(null)).toBe('/')
    expect(safeNext('https://evil.example/')).toBe('/')
    expect(safeNext('//evil.example/')).toBe('/')
    expect(safeNext('/\\evil.example/')).toBe('/')
    expect(safeNext('/login?next=/')).toBe('/')
  })

  it('omits next for the root', () => {
    expect(loginURL('/')).toBe('/login')
    expect(loginURL('/audit')).toBe('/login?next=%2Faudit')
  })
})
