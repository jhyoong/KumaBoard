// State management for KumaBoard frontend.

import type { Device, Metrics, Run } from './api';

export interface State {
  devices: Record<string, Device>;
  runs: Record<string, Run[]>;
}

export const emptyState: State = { devices: {}, runs: {} };

// SSE event union
export type SSEEvent =
  | { type: 'device'; device: Device }
  | { type: 'metrics'; device: string; metrics: Metrics }
  | { type: 'run'; run: Run };

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
      return {
        ...state,
        runs: { ...state.runs, [r.device]: next },
      };
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
