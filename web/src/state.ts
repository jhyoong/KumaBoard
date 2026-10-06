// State management for KumaBoard frontend.

import type { Device, Metrics, Run, RunOutput } from './api';

// How much of each output stream is kept and shown: the last 64 KiB, the
// same bound the agent and server apply. Counted in UTF-16 units here.
export const MAX_OUTPUT = 64 * 1024;

// Inserted where output was dropped, so a gap never reads as continuous.
export const SKIP_MARKER = '\n[... output skipped ...]\n';

// Streamed output of one run still in flight, newest last.
export interface LiveOutput {
  chunks: RunOutput[];
  // Older chunks were dropped here to stay within MAX_OUTPUT.
  dropped: boolean;
}

export interface State {
  devices: Record<string, Device>;
  runs: Record<string, Run[]>;
  // By run ID. Entries exist only while a run is in flight.
  output: Record<string, LiveOutput>;
}

export const emptyState: State = { devices: {}, runs: {}, output: {} };

// SSE event union
export type SSEEvent =
  | { type: 'device'; device: Device }
  | { type: 'metrics'; device: string; metrics: Metrics }
  | { type: 'run'; run: Run }
  | { type: 'run_output'; output: RunOutput };

// isTerminal reports whether a run status will not change again.
export function isTerminal(status: string): boolean {
  return status !== 'running' && status !== 'dispatched';
}

function tail(s: string): string {
  return s.length > MAX_OUTPUT ? s.slice(s.length - MAX_OUTPUT) : s;
}

// liveRun overlays streamed chunks onto a run that is still in flight, so
// the row reads like tail -f. A finished run is returned as is: its stored
// output is authoritative. Chunks the run's tails already include (seq up to
// output_seq) are not applied twice.
export function liveRun(run: Run, out: LiveOutput | undefined): Run {
  if (!out || isTerminal(run.status)) return run;
  const base = run.output_seq ?? 0;
  let stdout = run.stdout_tail;
  let stderr = run.stderr_tail;
  let truncated = run.truncated || out.dropped;
  let first = true;
  for (const c of out.chunks) {
    if (c.seq <= base) continue;
    // A hole between what the page loaded and the first chunk it received.
    const gap = c.skipped || (first && !out.dropped && c.seq !== base + 1);
    first = false;
    if (gap) truncated = true;
    const text = (gap ? SKIP_MARKER : '') + c.data;
    if (c.stream === 'stderr') stderr += text;
    else stdout += text;
  }
  if (stdout.length > MAX_OUTPUT || stderr.length > MAX_OUTPUT) truncated = true;
  return { ...run, stdout_tail: tail(stdout), stderr_tail: tail(stderr), truncated };
}

function appendOutput(prev: LiveOutput | undefined, o: RunOutput): LiveOutput | undefined {
  const chunks = prev?.chunks ?? [];
  const last = chunks.length > 0 ? chunks[chunks.length - 1].seq : undefined;
  // A replay after a reconnect, or an out-of-order event.
  if (last !== undefined && o.seq <= last) return prev;
  // Events lost in between: say so rather than joining the two ends.
  const chunk = last !== undefined && o.seq !== last + 1 ? { ...o, skipped: true } : o;
  const next = [...chunks, chunk];
  let dropped = prev?.dropped ?? false;
  let size = 0;
  for (const c of next) if (c.stream === chunk.stream) size += c.data.length;
  // Trim this stream to the newest MAX_OUTPUT, always keeping the new chunk.
  for (let i = 0; i < next.length - 1 && size > MAX_OUTPUT;) {
    if (next[i].stream !== chunk.stream) {
      i++;
      continue;
    }
    size -= next[i].data.length;
    next.splice(i, 1);
    dropped = true;
  }
  return { chunks: next, dropped };
}

export function reduce(state: State, event: SSEEvent): State {
  switch (event.type) {
    case 'device': {
      const d = event.device;
      return {
        ...state,
        devices: { ...state.devices, [d.name]: d },
      };
    }
    case 'metrics': {
      const existing = state.devices[event.device];
      if (!existing) return state;
      return {
        ...state,
        devices: {
          ...state.devices,
          [event.device]: { ...existing, metrics: event.metrics },
        },
      };
    }
    case 'run': {
      const r = event.run;
      const list = state.runs[r.device] ?? [];
      const idx = list.findIndex((x) => x.id === r.id);
      let next: Run[];
      if (idx >= 0) {
        next = [...list];
        next[idx] = r;
      } else {
        next = [r, ...list];
      }
      let output = state.output;
      if (isTerminal(r.status) && r.id in output) {
        // The stored result replaces what was streamed.
        output = { ...output };
        delete output[r.id];
      }
      return {
        ...state,
        runs: { ...state.runs, [r.device]: next },
        output,
      };
    }
    case 'run_output': {
      const o = event.output;
      // A chunk that trails the result of a finished run is dropped.
      const known = state.runs[o.device]?.find((r) => r.id === o.run_id);
      if (known && isTerminal(known.status)) return state;
      const next = appendOutput(state.output[o.run_id], o);
      if (!next || next === state.output[o.run_id]) return state;
      return { ...state, output: { ...state.output, [o.run_id]: next } };
    }
  }
}

// loadDevices replaces the device set with a full snapshot (initial load,
// resync, poll). Devices already shown keep their position so a refresh never
// reshuffles the list; new ones append in snapshot order, missing ones drop.
export function loadDevices(state: State, devices: Device[]): State {
  const incoming = new Map(devices.map((d) => [d.name, d]));
  const next: Record<string, Device> = {};
  for (const name of Object.keys(state.devices)) {
    const d = incoming.get(name);
    if (d) next[name] = d;
  }
  for (const d of devices) {
    if (!(d.name in next)) next[d.name] = d;
  }
  return { ...state, devices: next };
}
