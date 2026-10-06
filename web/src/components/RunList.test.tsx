// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { RunList } from './RunList'
import type { Run } from '../api'

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true

const run = (over: Partial<Run> = {}): Run => ({
  id: 'r1', device: 'nas', command: 'backup', requested_by: 'admin',
  requested_at: '2026-10-06T10:00:00Z', started_at: '', finished_at: '', exit_code: null,
  status: 'running', stdout_tail: '', stderr_tail: '', truncated: false, ...over,
})

let root: Root | null = null
let host: HTMLDivElement | null = null

function render(runs: Run[], onStop?: (r: Run) => void) {
  if (!host) {
    host = document.createElement('div')
    document.body.appendChild(host)
    root = createRoot(host)
  }
  act(() => root!.render(<RunList runs={runs} onStop={onStop} />))
  return host
}

const buttons = (el: HTMLElement, label: string) =>
  [...el.querySelectorAll('button')].filter((b) => b.textContent === label)

afterEach(() => {
  act(() => root?.unmount())
  host?.remove()
  root = null
  host = null
})

describe('RunList', () => {
  it('opens a running row by itself and shows its output', () => {
    const el = render([run({ stdout_tail: 'line 1\n' }), run({ id: 'r0', status: 'ok', stdout_tail: 'old\n' })])
    const panes = el.querySelectorAll('pre')
    expect(panes).toHaveLength(1)
    expect(panes[0].textContent).toBe('line 1\n')
  })

  it('follows new output and keeps the row open once the run ends', () => {
    let el = render([run()])
    expect(el.textContent).toContain('No output yet.')
    el = render([run({ stdout_tail: 'line 1\nline 2\n' })])
    expect(el.querySelector('pre')?.textContent).toBe('line 1\nline 2\n')
    el = render([run({ status: 'ok', exit_code: 0, stdout_tail: 'line 1\nline 2\nstored\n' })])
    expect(el.querySelector('pre')?.textContent).toBe('line 1\nline 2\nstored\n')
    expect(el.textContent).toContain('exit 0')
  })

  it('stays pinned to the bottom unless the user scrolled up', () => {
    let el = render([run({ stdout_tail: 'a\n' })])
    const pre = el.querySelector('pre')!
    // jsdom does no layout: give the pane a scrollable size by hand.
    let height = 1000
    Object.defineProperty(pre, 'scrollHeight', { configurable: true, get: () => height })
    Object.defineProperty(pre, 'clientHeight', { configurable: true, get: () => 100 })
    height = 1200
    el = render([run({ stdout_tail: 'a\nb\n' })])
    expect(pre.scrollTop).toBe(1200)

    // The user scrolls up to read something: new output must not yank them back.
    pre.scrollTop = 300
    act(() => { pre.dispatchEvent(new Event('scroll', { bubbles: true })) })
    height = 1400
    render([run({ stdout_tail: 'a\nb\nc\n' })])
    expect(pre.scrollTop).toBe(300)

    // Back at the bottom, following resumes.
    pre.scrollTop = 1300
    act(() => { pre.dispatchEvent(new Event('scroll', { bubbles: true })) })
    height = 1600
    render([run({ stdout_tail: 'a\nb\nc\nd\n' })])
    expect(pre.scrollTop).toBe(1600)
  })

  it('says the output is the last 64 KiB when truncated', () => {
    const el = render([run({ stdout_tail: 'tail\n', truncated: true })])
    expect(el.textContent).toContain('showing the last 64 KiB')
    expect(el.textContent).not.toContain('output truncated at 64 KiB')
  })

  it('offers Stop on running rows only, and only when the agent can cancel', () => {
    const onStop = vi.fn()
    const runs = [run(), run({ id: 'r2', status: 'dispatched' }), run({ id: 'r3', status: 'cancelled' })]
    let el = render(runs, onStop)
    const stop = buttons(el, 'Stop')
    expect(stop).toHaveLength(1)
    act(() => stop[0].click())
    expect(onStop).toHaveBeenCalledTimes(1)
    expect(onStop.mock.calls[0][0].id).toBe('r1')

    // Protocol-1 device: the page passes no onStop.
    act(() => root?.unmount())
    host?.remove()
    root = null
    host = null
    el = render(runs)
    expect(buttons(el, 'Stop')).toHaveLength(0)
  })

  it('shows the new statuses', () => {
    const el = render([run({ status: 'cancelled' }), run({ id: 'r2', status: 'refused', stderr_tail: 'refused: /x is writable by the agent account\n' })])
    expect(el.textContent).toContain('cancelled')
    expect(el.textContent).toContain('refused')
    // Finished rows stay closed until asked.
    expect(el.querySelectorAll('pre')).toHaveLength(0)
    act(() => buttons(el, 'output')[0].click())
    expect(el.querySelector('pre')?.textContent).toContain('writable by the agent account')
  })
})
