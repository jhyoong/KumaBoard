// Pure logic for the agent upgrade panel: state sets, reason and dispatch
// text, step durations. No DOM, so it is testable on its own.

import type { UpgradeDispatch, UpgradeEvent } from './api';

export const IN_FLIGHT = new Set(['requested', 'downloading', 'verifying', 'selftest', 'swapped', 'restarting']);
export const FAILED = new Set(['rolled_back', 'failed']);

export function isInFlight(state: string): boolean {
  return IN_FLIGHT.has(state);
}

export function isFailed(state: string): boolean {
  return FAILED.has(state);
}

// ---------- Reasons ----------

const REASONS: Record<string, (to: string) => string> = {
  download_failed: () => 'The agent could not download the new binary. The device was not changed.',
  verify_failed: () => 'The downloaded binary failed its size, hash or signature check. The device was not changed. Check that the release was signed with a key this agent trusts.',
  selftest_config_rejected: (to) => `Version ${to} refused this device's current config file. The device was not changed. Edit the config so ${to} accepts it, or pick a version that does, then Retry.`,
  selftest_failed: () => 'The new binary failed its local self-check. The device was not changed.',
  swap_failed: () => 'The agent could not replace its binary. Check free space and that the agent account can write its install directory.',
  busy: () => 'The agent was already running an upgrade.',
  no_handshake: () => 'The new version started but could not connect within 2 minutes, so the agent rolled back.',
  startup_failed: () => 'The new version could not start, so the agent rolled back.',
  crash_loop: () => 'The new version kept restarting without connecting, so the agent rolled back.',
  handshake_at_from_version: () => 'The device reconnected on the old version. No rollback report was received.',
  timed_out: () => 'The agent stopped reporting. The device was not changed unless a later step appears below.',
  abandoned: () => 'Abandoned by an operator.',
  send_failed: () => 'The server could not send the request.',
};

// reasonText explains a failure reason code. toVersion is the upgrade's
// target. A code this build does not know is shown as it is.
export function reasonText(code: string, toVersion = ''): string {
  // Own-property check: the code is server data and must not find "toString".
  if (!Object.hasOwn(REASONS, code)) return code;
  return REASONS[code](toVersion || 'the target version');
}

// What a failed upgrade shows when the agent sent no detail text.
export const NO_DETAIL_TEXT = 'This agent version does not report details.';

// ---------- Durations ----------

// duration formats a span of milliseconds: "0.4s", "42s", "3m 5s", "2h 10m".
// Tenths are kept under ten seconds, where upgrade steps usually differ.
export function duration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return '-';
  if (ms < 10_000) return `${(ms / 1000).toFixed(1)}s`;
  const sec = Math.floor(ms / 1000);
  if (sec < 60) return `${sec}s`;
  const min = Math.floor(sec / 60);
  if (min < 60) return sec % 60 ? `${min}m ${sec % 60}s` : `${min}m`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return min % 60 ? `${hr}h ${min % 60}m` : `${hr}h`;
  const days = Math.floor(hr / 24);
  return hr % 24 ? `${days}d ${hr % 24}h` : `${days}d`;
}

// age is a coarse "how long ago" for badges and one-line messages: "45s",
// "12m", "3h", "2d". '' when the timestamp is missing or unparseable.
export function age(iso: string | null | undefined, now: number): string {
  if (!iso) return '';
  const ms = now - Date.parse(iso);
  if (Number.isNaN(ms)) return '';
  const sec = Math.max(0, Math.floor(ms / 1000));
  if (sec < 60) return `${sec}s`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr}h`;
  return `${Math.floor(hr / 24)}d`;
}

// between is the time from one timestamp to another, null when either is
// missing or unparseable.
export function between(from: string | null | undefined, to: string | null | undefined): number | null {
  if (!from || !to) return null;
  const ms = Date.parse(to) - Date.parse(from);
  return Number.isNaN(ms) ? null : ms;
}

// stepDurations gives, for each event, the milliseconds since the event
// before it. The first is null, as is any step with an unparseable time.
export function stepDurations(events: Pick<UpgradeEvent, 'ts'>[]): (number | null)[] {
  return events.map((e, i) => {
    if (i === 0) return null;
    const ms = between(events[i - 1].ts, e.ts);
    return ms === null ? null : Math.max(0, ms);
  });
}

// clockTime is the local wall-clock time of a step.
export function clockTime(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '-' : d.toLocaleTimeString();
}

// ---------- Dispatch ----------

// One run of the dispatch line. device, when set, names the device page the
// run links to.
export interface DispatchPart {
  text: string;
  bold?: boolean;
  device?: string;
}

// What the dispatch line needs to know about the device it is shown on.
export interface DispatchContext {
  target: string; // the device's desired agent version
  os: string;
  arch: string;
  now: number; // ms since the epoch
}

// dispatchParts builds the line that says what became of the last upgrade
// evaluation. A code this build does not know shows the server's message.
export function dispatchParts(d: UpgradeDispatch, ctx: DispatchContext): DispatchPart[] {
  const v = ctx.target || 'the target version';
  switch (d.code) {
    case 'sent':
      return [{ text: 'Upgrade request sent.' }];
    case 'in_flight': {
      const facts = [d.blocking_state, age(d.blocking_since, ctx.now)].filter(Boolean).join(', ');
      return [
        { text: 'Not dispatched: an upgrade on ' },
        { text: d.blocking_device || 'another device', bold: true, device: d.blocking_device || undefined },
        { text: ` is in progress${facts ? ` (${facts})` : ''}. One upgrade runs at a time.` },
      ];
    }
    case 'waiting':
      return [
        { text: 'Not dispatched yet: the upgrade that was blocking this one has ended. Press ' },
        { text: 'Upgrade now', bold: true },
        { text: '.' },
      ];
    case 'prev_failed':
      return [
        { text: `Not dispatched: an earlier upgrade to ${v} failed on this device. Fix the cause below, then press ` },
        { text: 'Retry', bold: true },
        { text: '.' },
      ];
    case 'no_release':
      return [{ text: `Not dispatched: no ${v} release has been ingested for ${ctx.os}/${ctx.arch}.` }];
    case 'not_connected':
      return [{ text: `Not dispatched: the device is offline. It will be offered ${v} when it next connects.` }];
    case 'send_failed':
      return [{ text: `Not dispatched: the request could not be sent.${d.message ? ` ${d.message}` : ''}` }];
    default:
      return [{ text: d.message || d.code }];
  }
}

// dispatchText is the dispatch line as plain text.
export function dispatchText(d: UpgradeDispatch, ctx: DispatchContext): string {
  return dispatchParts(d, ctx).map((p) => p.text).join('');
}
