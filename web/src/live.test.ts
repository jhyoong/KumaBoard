import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Device } from './api';
import {
  BACKOFF_MAX_MS, LiveSync, POLL_MS, STALE_MS, backoffDelay, initialStatus, isStale, parseEvent,
} from './live';
import type { LiveStatus, StreamLike } from './live';
import type { SSEEvent } from './state';

class FakeStream implements StreamLike {
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  listeners: Record<string, (ev: MessageEvent) => void> = {};
  closed = false;
  addEventListener(name: string, fn: (ev: MessageEvent) => void) { this.listeners[name] = fn; }
  close() { this.closed = true; }
  open() { this.onopen?.(new Event('open')); }
  fail() { this.onerror?.(new Event('error')); }
  emit(name: string, data: unknown) {
    this.listeners[name]?.(new MessageEvent(name, { data: JSON.stringify(data) }));
  }
}

const dev = (name: string) => ({ name, state: 'online' }) as Device;

function setup(isAuthError?: (err: unknown) => boolean) {
  const streams: FakeStream[] = [];
  const events: SSEEvent[] = [];
  const snapshots: Device[][] = [];
  let status: LiveStatus = initialStatus;
  const fetchDevices = vi.fn(async () => [dev('a')]);
  const sync = new LiveSync({
    openStream: () => { const s = new FakeStream(); streams.push(s); return s; },
    fetchDevices,
    onEvent: (e) => events.push(e),
    onDevices: (d) => snapshots.push(d),
    onStatus: (s) => { status = s; },
    isAuthError,
  });
  return { sync, streams, events, snapshots, fetchDevices, status: () => status };
}

describe('backoffDelay', () => {
  it('doubles from 1s and caps at 30s', () => {
    expect([0, 1, 2, 3, 4].map(backoffDelay)).toEqual([1000, 2000, 4000, 8000, 16000]);
    expect(backoffDelay(5)).toBe(BACKOFF_MAX_MS);
    expect(backoffDelay(50)).toBe(BACKOFF_MAX_MS);
  });
});

describe('isStale', () => {
  const base: LiveStatus = { ...initialStatus, lastUpdate: 1000 };
  it('never stale while the stream is open, even when quiet', () => {
    expect(isStale({ ...base, stream: 'open' }, 1000 + 60_000)).toBe(false);
  });
  it('stale once down and older than 2x the refresh interval', () => {
    const down: LiveStatus = { ...base, stream: 'down' };
    expect(isStale(down, 1000 + STALE_MS)).toBe(false);
    expect(isStale(down, 1000 + STALE_MS + 1)).toBe(true);
  });
  it('stale with no data at all unless open', () => {
    expect(isStale({ ...initialStatus, stream: 'down' }, 0)).toBe(true);
    expect(isStale({ ...initialStatus, stream: 'open' }, 0)).toBe(false);
  });
});

describe('parseEvent', () => {
  it('maps named server payloads to reducer events', () => {
    expect(parseEvent('device', JSON.stringify(dev('a')))).toEqual({ type: 'device', device: dev('a') });
    expect(parseEvent('metrics', '{"device":"a","metrics":{"cpu_percent":5}}'))
      .toEqual({ type: 'metrics', device: 'a', metrics: { cpu_percent: 5 } });
    expect(parseEvent('run', '{"id":"r1","device":"a"}')?.type).toBe('run');
  });
  it('rejects malformed or unknown events', () => {
    expect(parseEvent('device', 'not json')).toBeNull();
    expect(parseEvent('device', '{}')).toBeNull();
    expect(parseEvent('metrics', '{"device":"a"}')).toBeNull();
    expect(parseEvent('bogus', '{"name":"a"}')).toBeNull();
  });

  it('maps run_output and rejects malformed chunks', () => {
    expect(parseEvent('run_output', '{"run_id":"r1","device":"a","seq":3,"stream":"stderr","data":"x\\n","skipped":true}'))
      .toEqual({ type: 'run_output', output: { run_id: 'r1', device: 'a', seq: 3, stream: 'stderr', data: 'x\n', skipped: true } });
    // skipped defaults to false; unknown fields are not carried along.
    expect(parseEvent('run_output', '{"run_id":"r1","device":"a","seq":1,"stream":"stdout","data":"","extra":1}'))
      .toEqual({ type: 'run_output', output: { run_id: 'r1', device: 'a', seq: 1, stream: 'stdout', data: '', skipped: false } });
    expect(parseEvent('run_output', '{"run_id":"r1","device":"a","seq":1,"stream":"stdlog","data":"x"}')).toBeNull();
    expect(parseEvent('run_output', '{"run_id":"r1","device":"a","seq":"1","stream":"stdout","data":"x"}')).toBeNull();
    expect(parseEvent('run_output', '{"device":"a","seq":1,"stream":"stdout","data":"x"}')).toBeNull();
    expect(parseEvent('run_output', '{"run_id":"r1","device":"a","seq":1,"stream":"stdout"}')).toBeNull();
  });
});

describe('LiveSync', () => {
  beforeEach(() => { vi.useFakeTimers(); });
  afterEach(() => { vi.useRealTimers(); });

  it('resyncs on open and applies named events without polling', async () => {
    const t = setup();
    t.sync.start();
    expect(t.status().stream).toBe('connecting');
    t.streams[0].open();
    await vi.advanceTimersByTimeAsync(0);
    expect(t.status()).toMatchObject({ stream: 'open', polling: false, error: null });
    expect(t.fetchDevices).toHaveBeenCalledTimes(1);
    expect(t.snapshots).toHaveLength(1);

    t.streams[0].emit('metrics', { device: 'a', metrics: { cpu_percent: 7 } });
    t.streams[0].emit('device', dev('a'));
    expect(t.events.map((e) => e.type)).toEqual(['metrics', 'device']);

    // Healthy stream: no parallel polling, however long we wait.
    await vi.advanceTimersByTimeAsync(POLL_MS * 10);
    expect(t.fetchDevices).toHaveBeenCalledTimes(1);
    t.sync.stop();
  });

  it('falls back to 5s polling while down and stops polling on reconnect', async () => {
    const t = setup();
    t.sync.start();
    t.streams[0].open();
    await vi.advanceTimersByTimeAsync(0);
    t.fetchDevices.mockClear();

    t.streams[0].fail();
    expect(t.streams[0].closed).toBe(true);
    expect(t.status()).toMatchObject({ stream: 'down', polling: true });
    await vi.advanceTimersByTimeAsync(0);
    expect(t.fetchDevices).toHaveBeenCalledTimes(1); // immediate poll

    // Reconnect attempt after 1s; it fails again, next retry in 2s.
    await vi.advanceTimersByTimeAsync(1000);
    expect(t.streams).toHaveLength(2);
    t.streams[1].fail();
    await vi.advanceTimersByTimeAsync(1999);
    expect(t.streams).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(t.streams).toHaveLength(3);

    // 3s elapsed since failure -> no second poll yet; at 5s there is.
    expect(t.fetchDevices).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(2000);
    expect(t.fetchDevices).toHaveBeenCalledTimes(2);

    t.streams[2].open();
    await vi.advanceTimersByTimeAsync(0);
    expect(t.status()).toMatchObject({ stream: 'open', polling: false });
    const calls = t.fetchDevices.mock.calls.length; // includes the resync
    await vi.advanceTimersByTimeAsync(POLL_MS * 4);
    expect(t.fetchDevices).toHaveBeenCalledTimes(calls);
    t.sync.stop();
  });

  it('resets backoff after a successful open and caps at 30s', async () => {
    const t = setup();
    t.sync.start();
    for (let i = 0; i < 7; i++) {
      t.streams[t.streams.length - 1].fail();
      await vi.advanceTimersByTimeAsync(backoffDelay(i));
    }
    expect(t.streams).toHaveLength(8);
    t.streams[7].fail();
    await vi.advanceTimersByTimeAsync(BACKOFF_MAX_MS - 1);
    expect(t.streams).toHaveLength(8);
    await vi.advanceTimersByTimeAsync(1);
    expect(t.streams).toHaveLength(9);

    t.streams[8].open();
    t.streams[8].fail();
    await vi.advanceTimersByTimeAsync(1000);
    expect(t.streams).toHaveLength(10);
    t.sync.stop();
  });

  it('records fetch errors and clears them on the next success', async () => {
    const t = setup();
    t.fetchDevices.mockRejectedValueOnce(new Error('boom'));
    t.sync.start();
    t.streams[0].fail();
    await vi.advanceTimersByTimeAsync(0);
    expect(t.status().error).toBe('boom');
    expect(t.status().lastUpdate).toBeNull();
    await vi.advanceTimersByTimeAsync(POLL_MS);
    expect(t.status().error).toBeNull();
    expect(t.status().lastUpdate).not.toBeNull();
    t.sync.stop();
  });

  it('stop tears down stream, retries and polling', async () => {
    const t = setup();
    t.sync.start();
    t.streams[0].fail();
    await vi.advanceTimersByTimeAsync(0);
    t.sync.stop();
    t.fetchDevices.mockClear();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(t.streams).toHaveLength(1);
    expect(t.fetchDevices).not.toHaveBeenCalled();
  });

  it('halts reconnects and polling once a fetch fails auth', async () => {
    const authErr = new Error('unauthorized');
    const t = setup((err) => err === authErr);
    t.fetchDevices.mockRejectedValue(authErr);
    t.sync.start();
    t.streams[0].fail();
    await vi.advanceTimersByTimeAsync(0);
    expect(t.fetchDevices).toHaveBeenCalledTimes(1);
    expect(t.status()).toMatchObject({ stream: 'down', polling: false, unauthenticated: true, error: null });

    await vi.advanceTimersByTimeAsync(60_000);
    expect(t.streams).toHaveLength(1);
    expect(t.fetchDevices).toHaveBeenCalledTimes(1);
  });
});
