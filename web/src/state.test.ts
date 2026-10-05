import { describe, it, expect } from 'vitest';
import { reduce, loadDevices, emptyState } from './state';
import type { Device, Metrics, Run } from './api';

const stub: Device = {
  name: 'dev1',
  state: 'online',
  connected: true,
  os: 'linux',
  arch: 'amd64',
  agent_version: '0.1.0',
  protocol_version: 1,
  desired_agent_version: '0.1.0',
  capabilities: [],
  commands: [],
  mac: '',
  normally_off: false,
  schedule: null,
  last_seen: '2025-01-01T00:00:00Z',
  last_disconnect_at: '',
  incompatible: false,
  terminal_enabled: false,
  reject_reason: '',
  metrics: null,
};

describe('state', () => {
  it('upserts devices via loadDevices and device events', () => {
    let s = loadDevices(emptyState, [stub, { ...stub, name: 'dev2' }]);
    expect(Object.keys(s.devices)).toEqual(['dev1', 'dev2']);

    s = reduce(s, { type: 'device', device: { ...stub, name: 'dev1', state: 'stale' } });
    expect(s.devices['dev1'].state).toBe('stale');
  });

  it('keeps existing order across snapshot reloads', () => {
    let s = loadDevices(emptyState, [{ ...stub, name: 'b' }, { ...stub, name: 'c' }]);
    s = reduce(s, { type: 'device', device: { ...stub, name: 'a' } });
    s = loadDevices(s, [
      { ...stub, name: 'a', state: 'stale' }, { ...stub, name: 'b' }, { ...stub, name: 'd' },
    ]);
    expect(Object.keys(s.devices)).toEqual(['b', 'a', 'd']);
    expect(s.devices['a'].state).toBe('stale');
  });

  it('attaches metrics to known device and ignores unknown', () => {
    const m: Metrics = {
      cpu_percent: 42, mem_total_bytes: 1e9, mem_used_bytes: 5e8,
      mem_used_percent: 50, disk_total_bytes: 1e10, disk_used_bytes: 5e9,
      disk_used_percent: 50, uptime_s: 3600, load1: 1, load5: 1, load15: 1,
    };
    let s = loadDevices(emptyState, [stub]);
    s = reduce(s, { type: 'metrics', device: 'dev1', metrics: m });
    expect(s.devices['dev1'].metrics?.cpu_percent).toBe(42);

    // unknown device is ignored
    const s2 = reduce(s, { type: 'metrics', device: 'nope', metrics: m });
    expect(s2).toBe(s);
  });

  it('passes gpus through on metrics events', () => {
    const m: Metrics = {
      cpu_percent: 1, mem_total_bytes: 1e9, mem_used_bytes: 5e8,
      mem_used_percent: 50, disk_total_bytes: 1e10, disk_used_bytes: 5e9,
      disk_used_percent: 50, uptime_s: 3600, load1: 1, load5: 1, load15: 1,
      gpus: [{ index: 0, name: 'Apple M2 Max', vendor: 'apple', util_percent: 12 }],
    };
    let s = loadDevices(emptyState, [stub]);
    s = reduce(s, { type: 'metrics', device: 'dev1', metrics: m });
    expect(s.devices['dev1'].metrics?.gpus?.[0].util_percent).toBe(12);
    expect(s.devices['dev1'].metrics?.gpus?.[0].mem_total_bytes).toBeUndefined();
  });

  it('prepends new runs and replaces existing by id', () => {
    const r1: Run = {
      id: 'r1', device: 'dev1', command: 'ping', requested_by: 'admin',
      requested_at: '', started_at: '', finished_at: '', exit_code: 0,
      status: 'running', stdout_tail: '', stderr_tail: '', truncated: false,
    };
    let s = reduce(emptyState, { type: 'run', run: r1 });
    expect(s.runs['dev1']).toHaveLength(1);

    const r2: Run = { ...r1, id: 'r2', status: 'ok' };
    s = reduce(s, { type: 'run', run: r2 });
    expect(s.runs['dev1']).toHaveLength(2);
    expect(s.runs['dev1'][0].id).toBe('r2'); // prepended

    const r1done: Run = { ...r1, status: 'ok', exit_code: 0 };
    s = reduce(s, { type: 'run', run: r1done });
    expect(s.runs['dev1']).toHaveLength(2);
    expect(s.runs['dev1'].find((r) => r.id === 'r1')?.status).toBe('ok');
  });
});
