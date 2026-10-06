import { describe, it, expect } from 'vitest';
import { reduce, loadDevices, emptyState, liveRun, MAX_OUTPUT, SKIP_MARKER } from './state';
import type { Device, Metrics, Run, RunOutput } from './api';

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

describe('run_output', () => {
  const running: Run = {
    id: 'r1', device: 'dev1', command: 'backup', requested_by: 'admin',
    requested_at: '', started_at: '', finished_at: '', exit_code: null,
    status: 'running', stdout_tail: '', stderr_tail: '', truncated: false,
  };
  const chunk = (seq: number, data: string, over: Partial<RunOutput> = {}): RunOutput => ({
    run_id: 'r1', device: 'dev1', seq, stream: 'stdout', data, skipped: false, ...over,
  });
  const feed = (s: typeof emptyState, ...chunks: RunOutput[]) =>
    chunks.reduce((acc, output) => reduce(acc, { type: 'run_output', output }), s);
  const shown = (s: typeof emptyState, run: Run = running) => liveRun(run, s.output[run.id]);

  it('appends chunks to the matching run, per stream', () => {
    let s = reduce(emptyState, { type: 'run', run: running });
    s = feed(s, chunk(1, 'one\n'), chunk(2, 'oops\n', { stream: 'stderr' }), chunk(3, 'two\n'));
    const r = shown(s);
    expect(r.stdout_tail).toBe('one\ntwo\n');
    expect(r.stderr_tail).toBe('oops\n');
    expect(r.truncated).toBe(false);
    // The run list itself is untouched; only the overlay changes.
    expect(s.runs['dev1'][0].stdout_tail).toBe('');
  });

  it('keeps output for a run it has no row for yet, and other runs apart', () => {
    const s = feed(emptyState, chunk(1, 'early\n'), chunk(1, 'other\n', { run_id: 'r2' }));
    expect(shown(s).stdout_tail).toBe('early\n');
    expect(liveRun({ ...running, id: 'r2' }, s.output['r2']).stdout_tail).toBe('other\n');
  });

  it('ignores replayed and out-of-order chunks', () => {
    const s1 = feed(emptyState, chunk(1, 'a'), chunk(2, 'b'));
    const s2 = feed(s1, chunk(2, 'b'), chunk(1, 'a'));
    expect(s2).toBe(s1);
    expect(shown(s2).stdout_tail).toBe('ab');
  });

  it('trims each stream to the last 64 KiB', () => {
    const block = 'x'.repeat(10 * 1024);
    let s = emptyState;
    for (let i = 1; i <= 20; i++) s = feed(s, chunk(i, i === 20 ? block + 'END' : block));
    s = feed(s, chunk(21, 'err', { stream: 'stderr' }));
    const out = s.output['r1'];
    const kept = out.chunks.filter((c) => c.stream === 'stdout').reduce((n, c) => n + c.data.length, 0);
    expect(kept).toBeLessThanOrEqual(MAX_OUTPUT);
    expect(out.dropped).toBe(true);
    const r = shown(s);
    expect(r.stdout_tail.length).toBeLessThanOrEqual(MAX_OUTPUT);
    expect(r.stdout_tail.endsWith('END')).toBe(true);
    expect(r.stderr_tail).toBe('err');
    expect(r.truncated).toBe(true);
    // Dropping old chunks is not a gap in the stream: no marker.
    expect(r.stdout_tail).not.toContain(SKIP_MARKER);
  });

  it('keeps a single chunk larger than the cap and shows its tail', () => {
    const s = feed(emptyState, chunk(1, 'y'.repeat(MAX_OUTPUT + 500) + 'END'));
    expect(s.output['r1'].chunks).toHaveLength(1);
    const r = shown(s);
    expect(r.stdout_tail.length).toBe(MAX_OUTPUT);
    expect(r.stdout_tail.endsWith('END')).toBe(true);
    expect(r.truncated).toBe(true);
  });

  it('inserts a marker for a skipped chunk', () => {
    const s = feed(emptyState, chunk(1, 'before\n'), chunk(2, 'after\n', { skipped: true }));
    const r = shown(s);
    expect(r.stdout_tail).toBe('before\n' + SKIP_MARKER + 'after\n');
    expect(r.truncated).toBe(true);
  });

  it('inserts a marker when events were lost in between', () => {
    const s = feed(emptyState, chunk(1, 'a\n'), chunk(4, 'd\n'));
    expect(shown(s).stdout_tail).toBe('a\n' + SKIP_MARKER + 'd\n');
  });

  it('continues from the tail a page loaded mid-run, without repeating it', () => {
    // The fetch returned output up to seq 2; chunks 1 and 2 also arrived live.
    const loaded: Run = { ...running, stdout_tail: 'one\ntwo\n', output_seq: 2 };
    const s = feed(emptyState, chunk(1, 'one\n'), chunk(2, 'two\n'), chunk(3, 'three\n'));
    expect(shown(s, loaded).stdout_tail).toBe('one\ntwo\nthree\n');
    // Loaded at seq 2 but the first live chunk is 5: mark the hole.
    const late = feed(emptyState, chunk(5, 'five\n'));
    expect(shown(late, loaded).stdout_tail).toBe('one\ntwo\n' + SKIP_MARKER + 'five\n');
  });

  it('a running run event does not wipe streamed output; a finished one replaces it', () => {
    let s = reduce(emptyState, { type: 'run', run: running });
    s = feed(s, chunk(1, 'streamed\n'));
    s = reduce(s, { type: 'run', run: { ...running } });
    expect(shown(s, s.runs['dev1'][0]).stdout_tail).toBe('streamed\n');

    const done: Run = { ...running, status: 'ok', exit_code: 0, stdout_tail: 'stored\n' };
    s = reduce(s, { type: 'run', run: done });
    expect(s.output['r1']).toBeUndefined();
    expect(shown(s, s.runs['dev1'][0]).stdout_tail).toBe('stored\n');
    // A stray late chunk cannot alter a finished run.
    const after = feed(s, chunk(2, 'late\n'));
    expect(after).toBe(s);
  });

  it('leaves a run with no streamed output alone', () => {
    expect(liveRun(running, undefined)).toBe(running);
  });
});
