// API client for KumaBoard server.

import { handleUnauthorized } from './auth';

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    credentials: 'same-origin',
    ...init,
    headers: {
      'Content-Type': 'application/json',
      ...init?.headers,
    },
  });
  if (!res.ok) {
    // A 401 on the login POST is a bad password, not an expired session.
    if (res.status === 401 && path !== '/api/login') handleUnauthorized();
    let msg = res.statusText;
    try {
      const body = await res.json();
      if (body.error) msg = body.error;
    } catch { /* ignore */ }
    throw new ApiError(res.status, msg);
  }
  if (res.status === 204) return undefined as T;
  return res.json() as Promise<T>;
}

// wakePath is the Wake-on-LAN endpoint for a device. The server looks the MAC
// up by device name; there is no wake-by-MAC route.
export function wakePath(name: string): string {
  return `/api/devices/${encodeURIComponent(name)}/wake`;
}

// ---------- Types ----------

export interface Metrics {
  cpu_percent: number;
  mem_total_bytes: number;
  mem_used_bytes: number;
  mem_used_percent: number;
  disk_total_bytes: number;
  disk_used_bytes: number;
  disk_used_percent: number;
  uptime_s: number;
  load1: number;
  load5: number;
  load15: number;
  // Absent from agents without the gpu capability and from pre-GPU agents.
  gpus?: GPU[];
  // Host CPU/SoC temperature in °C. Absent when the platform has no usable
  // sensor (Windows, sensorless hosts) and from pre-temperature agents.
  temp_c?: number;
  // The sensor temp_c came from, e.g. "k10temp/Tctl".
  temp_sensor?: string;
}

// One adapter's sample. Optional numerics are absent when the platform cannot
// report them (e.g. Apple unified memory has no VRAM total).
export interface GPU {
  index: number;
  name: string;
  vendor: string;
  util_percent?: number;
  mem_used_bytes?: number;
  mem_total_bytes?: number;
  temp_c?: number;
  power_w?: number;
  suspended?: boolean;
}

// One bucket of GET /api/devices/{name}/metrics/history: 30s for window=1h,
// 15 minutes (averaged) for 24h/7d/30d. t is the bucket start in unix seconds;
// gpu_pct is absent when no GPU reported. disk_pct is absent only from
// servers that predate disk history. temp_c is absent when the device reported
// no host temperature.
export interface HistorySample {
  t: number;
  cpu: number;
  mem_pct: number;
  gpu_pct?: number;
  // Summed across GPUs; absent when none reported memory. The total is absent
  // when unknown (Apple unified memory reports no VRAM total).
  gpu_mem_used_bytes?: number;
  gpu_mem_total_bytes?: number;
  disk_pct?: number;
  temp_c?: number;
}

export interface MetricsHistory {
  device: string;
  window: string;
  samples: HistorySample[];
}

export interface CommandDef {
  name: string;
  description: string;
  timeout_s: number;
  expect_disconnect: boolean;
}

export interface Window {
  days: string;
  from: string;
  to: string;
}

export interface Schedule {
  expected_offline: Window[];
  grace_period_s: number;
}

export type DeviceState = 'online' | 'stale' | 'offline_expected' | 'offline_unexpected';

export interface Device {
  name: string;
  state: DeviceState;
  connected: boolean;
  os: string;
  arch: string;
  agent_version: string;
  protocol_version: number;
  desired_agent_version: string;
  capabilities: string[];
  commands: CommandDef[];
  mac: string;
  normally_off: boolean;
  schedule: Schedule | null;
  last_seen: string;
  last_disconnect_at: string;
  incompatible: boolean;
  terminal_enabled: boolean;
  reject_reason: string;
  metrics: Metrics | null;
}

export interface Run {
  id: string;
  device: string;
  command: string;
  requested_by: string;
  requested_at: string;
  started_at: string;
  finished_at: string;
  exit_code: number | null;
  status: string;
  stdout_tail: string;
  stderr_tail: string;
  truncated: boolean;
}

export interface AuditEntry {
  id: string;
  ts: string;
  actor: string;
  action: string;
  target: string;
  result: string;
  detail: string;
}

export interface UpgradeEntry {
  id: string;
  device_id: number;
  from_version: string;
  to_version: string;
  requested_by: string;
  started_at: string;
  finished_at: string | null;
  state: string;
  failure_reason: string;
}

export interface ReleaseEntry {
  version: string;
  os: string;
  arch: string;
  added_at: string;
}
