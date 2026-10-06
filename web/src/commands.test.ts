import { describe, expect, it, vi } from 'vitest';
import type { CommandDef, Run } from './api';
import { canStop, confirmMessage, confirmRun, mergeRuns, runningRun } from './commands';

const cmd = (over: Partial<CommandDef> = {}): CommandDef => ({
  name: 'wipe-cache', description: 'Delete the build cache', timeout_s: 60, expect_disconnect: false, ...over,
});

const run = (over: Partial<Run> = {}): Run => ({
  id: 'r1', device: 'nas', command: 'wipe-cache', requested_by: 'admin',
  requested_at: '2026-10-06T10:00:00Z', started_at: '', finished_at: '', exit_code: null,
  status: 'running', stdout_tail: '', stderr_tail: '', truncated: false, ...over,
});

describe('confirm gate', () => {
  it('starts a command without confirm straight away, never asking', () => {
    const ask = vi.fn(() => false);
    expect(confirmRun('nas', cmd(), ask)).toBe(true);
    expect(confirmRun('nas', cmd({ confirm: false }), ask)).toBe(true);
    expect(ask).not.toHaveBeenCalled();
  });

  it('asks first when confirm is set and obeys the answer', () => {
    const yes = vi.fn(() => true);
    const no = vi.fn(() => false);
    expect(confirmRun('nas', cmd({ confirm: true }), yes)).toBe(true);
    expect(confirmRun('nas', cmd({ confirm: true }), no)).toBe(false);
    expect(yes).toHaveBeenCalledTimes(1);
    expect(no).toHaveBeenCalledTimes(1);
  });

  it('names the device, the command and its description', () => {
    const ask = vi.fn(() => true);
    confirmRun('nas', cmd({ confirm: true }), ask);
    const message = (ask.mock.calls[0] as unknown as [string])[0];
    expect(message).toContain('nas');
    expect(message).toContain('wipe-cache');
    expect(message).toContain('Delete the build cache');
    expect(confirmMessage('nas', cmd({ description: '' }))).toBe('Run "wipe-cache" on nas?');
  });
});

describe('canStop', () => {
  it('needs a connected agent on protocol 2 or later', () => {
    expect(canStop({ connected: true, protocol_version: 2 })).toBe(true);
    expect(canStop({ connected: true, protocol_version: 3 })).toBe(true);
    expect(canStop({ connected: true, protocol_version: 1 })).toBe(false);
    expect(canStop({ connected: true, protocol_version: 0 })).toBe(false);
    expect(canStop({ connected: false, protocol_version: 2 })).toBe(false);
  });
});

describe('runningRun', () => {
  it('finds only a running run of that command', () => {
    const runs = [
      run({ id: 'a', status: 'ok' }), run({ id: 'b', command: 'other' }),
      run({ id: 'c', status: 'dispatched' }), run({ id: 'd' }),
    ];
    expect(runningRun(runs, 'wipe-cache')?.id).toBe('d');
    expect(runningRun(runs, 'other')?.id).toBe('b');
    expect(runningRun(runs, 'missing')).toBeUndefined();
    expect(runningRun([run({ status: 'cancelled' })], 'wipe-cache')).toBeUndefined();
  });
});

describe('mergeRuns', () => {
  it('prefers the live copy and sorts newest first', () => {
    const fetched = [run({ id: 'old', requested_at: '2026-10-06T09:00:00Z', status: 'ok' }), run({ id: 'r1' })];
    const live = [run({ id: 'r1', status: 'ok', stdout_tail: 'done\n' }), run({ id: 'new', requested_at: '2026-10-06T11:00:00Z' })];
    const merged = mergeRuns(live, fetched);
    expect(merged.map((r) => r.id)).toEqual(['new', 'r1', 'old']);
    expect(merged[1].status).toBe('ok');
    expect(merged[1].stdout_tail).toBe('done\n');
  });

  it('keeps the fetched tail while the live copy is still in flight', () => {
    const fetched = [run({ stdout_tail: 'so far\n', truncated: true, output_seq: 7 })];
    const merged = mergeRuns([run()], fetched);
    expect(merged[0].stdout_tail).toBe('so far\n');
    expect(merged[0].output_seq).toBe(7);
    expect(merged[0].truncated).toBe(true);
    // Once the live copy is final, its stored output is what counts.
    const done = mergeRuns([run({ status: 'cancelled', stdout_tail: 'final\n' })], fetched);
    expect(done[0].stdout_tail).toBe('final\n');
    expect(done[0].output_seq).toBeUndefined();
  });
});
