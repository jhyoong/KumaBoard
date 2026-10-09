import { describe, expect, it } from 'vitest';
import type { UpgradeDispatch } from './api';
import {
  age, between, dispatchParts, dispatchText, duration, isFailed, isInFlight, reasonText, stepDurations,
} from './upgrade';

const NOW = Date.parse('2026-10-06T12:00:00Z');
const ctx = { target: '0.2.5', os: 'darwin', arch: 'arm64', now: NOW };

const dispatch = (over: Partial<UpgradeDispatch> = {}): UpgradeDispatch => ({
  code: 'sent', message: '', blocking_device: '', upgrade_id: '', at: '2026-10-06T11:59:00Z', ...over,
});

describe('state sets', () => {
  it('sorts every state into in flight, failed or neither', () => {
    for (const s of ['requested', 'downloading', 'verifying', 'selftest', 'swapped', 'restarting']) {
      expect(isInFlight(s)).toBe(true);
      expect(isFailed(s)).toBe(false);
    }
    for (const s of ['failed', 'rolled_back']) {
      expect(isInFlight(s)).toBe(false);
      expect(isFailed(s)).toBe(true);
    }
    expect(isInFlight('verified')).toBe(false);
    expect(isFailed('verified')).toBe(false);
  });
});

describe('reasonText', () => {
  const cases: [string, string][] = [
    ['download_failed', 'The agent could not download the new binary. The device was not changed.'],
    ['verify_failed', 'The downloaded binary failed its size, hash or signature check. The device was not changed. Check that the release was signed with a key this agent trusts.'],
    ['selftest_config_rejected', "Version 0.2.5 refused this device's current config file. The device was not changed. Edit the config so 0.2.5 accepts it, or pick a version that does, then Retry."],
    ['selftest_failed', 'The new binary failed its local self-check. The device was not changed.'],
    ['swap_failed', 'The agent could not replace its binary. Check free space and that the agent account can write its install directory.'],
    ['busy', 'The agent was already running an upgrade.'],
    ['no_handshake', 'The new version started but could not connect within 2 minutes, so the agent rolled back.'],
    ['startup_failed', 'The new version could not start, so the agent rolled back.'],
    ['crash_loop', 'The new version kept restarting without connecting, so the agent rolled back.'],
    ['handshake_at_from_version', 'The device reconnected on the old version. No rollback report was received.'],
    ['timed_out', 'The agent stopped reporting. The device was not changed unless a later step appears below.'],
    ['abandoned', 'Abandoned by an operator.'],
    ['send_failed', 'The server could not send the request.'],
  ];

  it.each(cases)('explains %s', (code, text) => {
    expect(reasonText(code, '0.2.5')).toBe(text);
  });

  it('falls back to the code itself when it is unknown', () => {
    expect(reasonText('handshake_rejected', '0.2.5')).toBe('handshake_rejected');
    expect(reasonText('invalid_reason')).toBe('invalid_reason');
    expect(reasonText('')).toBe('');
  });

  it('does not mistake an Object.prototype name for a known code', () => {
    expect(reasonText('toString')).toBe('toString');
    expect(reasonText('constructor')).toBe('constructor');
  });
});

describe('dispatchText', () => {
  it('sent', () => {
    expect(dispatchText(dispatch(), ctx)).toBe('Upgrade request sent.');
  });

  it('in_flight names the blocking device, its state and how long', () => {
    const d = dispatch({
      code: 'in_flight', blocking_device: 'mac-desktop',
      blocking_state: 'restarting', blocking_since: '2026-10-06T11:48:00Z',
    });
    expect(dispatchText(d, ctx))
      .toBe('Not dispatched: an upgrade on mac-desktop is in progress (restarting, 12m). One upgrade runs at a time.');
    const parts = dispatchParts(d, ctx);
    expect(parts[1]).toEqual({ text: 'mac-desktop', bold: true, device: 'mac-desktop' });
  });

  it('in_flight without state or start time drops the parenthesis', () => {
    expect(dispatchText(dispatch({ code: 'in_flight', blocking_device: 'mac-desktop' }), ctx))
      .toBe('Not dispatched: an upgrade on mac-desktop is in progress. One upgrade runs at a time.');
    expect(dispatchText(dispatch({ code: 'in_flight', blocking_device: 'mac-desktop', blocking_state: 'swapped' }), ctx))
      .toBe('Not dispatched: an upgrade on mac-desktop is in progress (swapped). One upgrade runs at a time.');
  });

  it('in_flight with no device named links nowhere', () => {
    const parts = dispatchParts(dispatch({ code: 'in_flight' }), ctx);
    expect(parts[1].text).toBe('another device');
    expect(parts[1].device).toBeUndefined();
  });

  it('waiting', () => {
    const d = dispatch({ code: 'waiting' });
    expect(dispatchText(d, ctx))
      .toBe('Not dispatched yet: the upgrade that was blocking this one has ended. Press Upgrade now.');
    expect(dispatchParts(d, ctx)[1]).toEqual({ text: 'Upgrade now', bold: true });
  });

  it('prev_failed', () => {
    const d = dispatch({ code: 'prev_failed' });
    expect(dispatchText(d, ctx))
      .toBe('Not dispatched: an earlier upgrade to 0.2.5 failed on this device. Fix the cause below, then press Retry.');
    expect(dispatchParts(d, ctx)[1]).toEqual({ text: 'Retry', bold: true });
  });

  it('no_release', () => {
    expect(dispatchText(dispatch({ code: 'no_release' }), ctx))
      .toBe('Not dispatched: no 0.2.5 release has been ingested for darwin/arm64.');
  });

  it('not_connected', () => {
    expect(dispatchText(dispatch({ code: 'not_connected' }), ctx))
      .toBe('Not dispatched: the device is offline. It will be offered 0.2.5 when it next connects.');
  });

  it('send_failed appends the server message', () => {
    expect(dispatchText(dispatch({ code: 'send_failed', message: 'write: broken pipe' }), ctx))
      .toBe('Not dispatched: the request could not be sent. write: broken pipe');
    expect(dispatchText(dispatch({ code: 'send_failed' }), ctx))
      .toBe('Not dispatched: the request could not be sent.');
  });

  it('shows the server message for a code it does not know', () => {
    expect(dispatchText(dispatch({ code: 'paused', message: 'upgrades are paused' }), ctx)).toBe('upgrades are paused');
    expect(dispatchText(dispatch({ code: 'paused' }), ctx)).toBe('paused');
  });
});

describe('step durations', () => {
  it('measures each step from the one before it', () => {
    const events = [
      { ts: '2026-10-06T10:00:00Z' },
      { ts: '2026-10-06T10:00:02Z' },
      { ts: '2026-10-06T10:01:32.5Z' },
      { ts: '2026-10-06T10:01:32.9Z' },
    ];
    expect(stepDurations(events)).toEqual([null, 2000, 90_500, 400]);
    expect(stepDurations([])).toEqual([]);
  });

  it('never goes negative and survives a bad timestamp', () => {
    expect(stepDurations([{ ts: '2026-10-06T10:00:05Z' }, { ts: '2026-10-06T10:00:00Z' }])).toEqual([null, 0]);
    expect(stepDurations([{ ts: 'nonsense' }, { ts: '2026-10-06T10:00:00Z' }])).toEqual([null, null]);
  });

  it('formats a duration', () => {
    expect(duration(400)).toBe('0.4s');
    expect(duration(2000)).toBe('2.0s');
    expect(duration(42_000)).toBe('42s');
    expect(duration(90_500)).toBe('1m 30s');
    expect(duration(120_000)).toBe('2m');
    expect(duration(2 * 3600_000 + 10 * 60_000)).toBe('2h 10m');
    expect(duration(26 * 3600_000)).toBe('1d 2h');
    expect(duration(-1)).toBe('-');
  });

  it('formats an age and a span between timestamps', () => {
    expect(age('2026-10-06T11:59:15Z', NOW)).toBe('45s');
    expect(age('2026-10-06T11:48:00Z', NOW)).toBe('12m');
    expect(age('2026-10-06T09:00:00Z', NOW)).toBe('3h');
    expect(age('2026-10-04T12:00:00Z', NOW)).toBe('2d');
    expect(age('2026-10-06T12:00:30Z', NOW)).toBe('0s');
    expect(age('', NOW)).toBe('');
    expect(age(undefined, NOW)).toBe('');
    expect(between('2026-10-06T10:00:00Z', '2026-10-06T10:00:07Z')).toBe(7000);
    expect(between('2026-10-06T10:00:00Z', null)).toBeNull();
  });
});
