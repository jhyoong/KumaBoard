// Live dashboard sync: SSE stream from /api/events with exponential-backoff
// reconnect, a devices resync on every (re)connect, and a /api/devices poll
// fallback that runs only while the stream is down.

import type { Device, Metrics, Run } from './api';
import type { SSEEvent } from './state';

export const POLL_MS = 5000;
export const STALE_MS = 2 * POLL_MS;
export const BACKOFF_BASE_MS = 1000;
export const BACKOFF_MAX_MS = 30000;

export type StreamStatus = 'connecting' | 'open' | 'down';

export interface LiveStatus {
  stream: StreamStatus;
  polling: boolean;
  // ms timestamp of the last event or successful fetch; null before first data.
  lastUpdate: number | null;
  error: string | null;
  // Set once a fetch fails auth; sync halts until a fresh LiveSync starts.
  unauthenticated: boolean;
}

export const initialStatus: LiveStatus = {
  stream: 'connecting', polling: false, lastUpdate: null, error: null, unauthenticated: false,
};

// backoffDelay returns the reconnect delay for the given 0-based attempt.
export function backoffDelay(attempt: number): number {
  return Math.min(BACKOFF_BASE_MS * 2 ** attempt, BACKOFF_MAX_MS);
}

// isStale reports whether the dashboard data can no longer be trusted. An open
// stream is live by definition: agents report every metrics_interval_s (30s by
// default), so a quiet stream is not a stale one. Once the stream is down, data
// is stale when neither an event nor a poll has landed within STALE_MS.
export function isStale(s: LiveStatus, now: number): boolean {
  if (s.stream === 'open') return false;
  return s.lastUpdate === null || now - s.lastUpdate > STALE_MS;
}

// parseEvent maps a named server SSE event to the reducer's event union. The
// server sends bare payloads (Summary, MetricsEvent, Run) keyed by event name.
export function parseEvent(name: string, data: string): SSEEvent | null {
  let v: unknown;
  try {
    v = JSON.parse(data);
  } catch {
    return null;
  }
  if (!v || typeof v !== 'object') return null;
  switch (name) {
    case 'device':
      return typeof (v as Device).name === 'string' ? { type: 'device', device: v as Device } : null;
    case 'metrics': {
      const m = v as { device?: unknown; metrics?: unknown };
      if (typeof m.device !== 'string' || !m.metrics) return null;
      return { type: 'metrics', device: m.device, metrics: m.metrics as Metrics };
    }
    case 'run':
      return typeof (v as Run).id === 'string' ? { type: 'run', run: v as Run } : null;
  }
  return null;
}

export const EVENT_NAMES = ['device', 'metrics', 'run'] as const;

// StreamLike is the subset of EventSource LiveSync uses (injectable in tests).
export interface StreamLike {
  onopen: ((ev: Event) => void) | null;
  onerror: ((ev: Event) => void) | null;
  addEventListener(name: string, fn: (ev: MessageEvent) => void): void;
  close(): void;
}

export interface LiveDeps {
  openStream: () => StreamLike;
  fetchDevices: () => Promise<Device[]>;
  onEvent: (e: SSEEvent) => void;
  onDevices: (d: Device[]) => void;
  onStatus: (s: LiveStatus) => void;
  // Reports whether a fetchDevices error means the session is gone. Such an
  // error stops reconnects and polling instead of hammering the server.
  isAuthError?: (err: unknown) => boolean;
  now?: () => number;
}

export class LiveSync {
  private deps: LiveDeps;
  private now: () => number;
  private status: LiveStatus = initialStatus;
  private es: StreamLike | null = null;
  private attempt = 0;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private pollTimer: ReturnType<typeof setInterval> | null = null;
  private stopped = false;

  constructor(deps: LiveDeps) {
    this.deps = deps;
    this.now = deps.now ?? Date.now;
  }

  start(): void {
    this.stopped = false;
    this.connect();
  }

  stop(): void {
    this.stopped = true;
    this.es?.close();
    this.es = null;
    if (this.reconnectTimer) clearTimeout(this.reconnectTimer);
    this.reconnectTimer = null;
    this.stopPolling();
  }

  private set(patch: Partial<LiveStatus>): void {
    this.status = { ...this.status, ...patch };
    this.deps.onStatus(this.status);
  }

  private connect(): void {
    this.reconnectTimer = null;
    if (this.stopped) return;
    this.set({ stream: 'connecting' });
    const es = this.deps.openStream();
    this.es = es;
    es.onopen = () => {
      if (this.es !== es) return;
      this.attempt = 0;
      this.stopPolling();
      this.set({ stream: 'open' });
      // Events published while we were disconnected are gone; resync.
      void this.sync();
    };
    es.onerror = () => {
      if (this.es !== es) return;
      // Take over reconnection from EventSource so we control the backoff.
      es.close();
      this.es = null;
      this.set({ stream: 'down' });
      this.startPolling();
      const delay = backoffDelay(this.attempt++);
      this.reconnectTimer = setTimeout(() => this.connect(), delay);
    };
    for (const name of EVENT_NAMES) {
      es.addEventListener(name, (ev) => {
        if (this.es !== es) return;
        const e = parseEvent(name, ev.data);
        if (!e) return;
        this.deps.onEvent(e);
        this.set({ lastUpdate: this.now() });
      });
    }
  }

  private startPolling(): void {
    if (this.pollTimer || this.stopped) return;
    this.set({ polling: true });
    void this.sync();
    this.pollTimer = setInterval(() => void this.sync(), POLL_MS);
  }

  private stopPolling(): void {
    if (this.pollTimer) clearInterval(this.pollTimer);
    this.pollTimer = null;
    if (this.status.polling) this.set({ polling: false });
  }

  private async sync(): Promise<void> {
    try {
      const devices = await this.deps.fetchDevices();
      if (this.stopped) return;
      this.deps.onDevices(devices);
      this.set({ lastUpdate: this.now(), error: null });
    } catch (err) {
      if (this.stopped) return;
      if (this.deps.isAuthError?.(err)) {
        this.stop();
        this.set({ stream: 'down', unauthenticated: true, error: null });
        return;
      }
      this.set({ error: err instanceof Error ? err.message : 'Failed to load devices' });
    }
  }
}
