// Helpers for the command buttons on the device page.

import type { CommandDef, Device, Run } from './api';

// Cancel arrived with agent protocol 2; older agents have no way to stop a run.
export const CANCEL_MIN_PROTOCOL = 2;

// canStop reports whether a Stop button makes sense for this device.
export function canStop(device: Pick<Device, 'connected' | 'protocol_version'>): boolean {
  return device.connected && device.protocol_version >= CANCEL_MIN_PROTOCOL;
}

// confirmMessage is the question asked before a confirm: true command starts.
export function confirmMessage(device: string, cmd: CommandDef): string {
  const what = cmd.description ? `\n\n${cmd.description}` : '';
  return `Run "${cmd.name}" on ${device}?${what}`;
}

// confirmRun gates a command start. Commands without confirm go straight
// through; the rest ask first. This guards against misclicks only: it runs in
// the browser and is not a security control.
export function confirmRun(
  device: string, cmd: CommandDef, ask: (message: string) => boolean = (m) => window.confirm(m),
): boolean {
  return !cmd.confirm || ask(confirmMessage(device, cmd));
}

// runningRun finds the in-flight run of a command, if any. The agent runs a
// command name at most once at a time.
export function runningRun(runs: Run[], command: string): Run | undefined {
  return runs.find((r) => r.command === command && r.status === 'running');
}

// mergeRuns combines runs seen live with the fetched history, newest first.
// The live copy wins, except that a run still in flight keeps the output tail
// the fetch delivered: a live "run" event carries none until the run ends.
export function mergeRuns(live: Run[], fetched: Run[]): Run[] {
  const byId = new Map<string, Run>();
  for (const r of fetched) byId.set(r.id, r);
  for (const r of live) {
    const f = byId.get(r.id);
    const keepTail = f?.output_seq !== undefined && r.output_seq === undefined
      && (r.status === 'running' || r.status === 'dispatched');
    byId.set(r.id, keepTail
      ? { ...r, stdout_tail: f.stdout_tail, stderr_tail: f.stderr_tail, truncated: f.truncated, output_seq: f.output_seq }
      : r);
  }
  return [...byId.values()].sort((a, b) => b.requested_at.localeCompare(a.requested_at));
}
