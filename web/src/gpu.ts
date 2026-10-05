// GPU display helpers. Absent fields are omitted, never shown as zero, with
// one exception: a GPU that reports utilisation but no memory shows a "n/a"
// memory placeholder, so usage and memory always appear together.

import type { GPU, Metrics } from './api';
import { bytes } from './format';

const MEM_NA = 'n/a';

// gpuMemory renders "used / total", or just "used" where the platform has no
// total (Apple unified memory). Undefined when memory is not reported.
export function gpuMemory(g: GPU): string | undefined {
  if (g.mem_used_bytes === undefined) return undefined;
  return g.mem_total_bytes !== undefined
    ? `${bytes(g.mem_used_bytes)} / ${bytes(g.mem_total_bytes)}`
    : bytes(g.mem_used_bytes);
}

// gpuSummary renders one adapter, e.g. "37% · 2.1 GB / 8.0 GB · 61°C · 118 W".
// A suspended GPU is "asleep", plus its memory if the agent still reports it.
export function gpuSummary(g: GPU): string {
  const mem = gpuMemory(g);
  if (g.suspended) return mem !== undefined ? `asleep · ${mem}` : 'asleep';
  const parts: string[] = [];
  if (g.util_percent !== undefined) {
    parts.push(`${g.util_percent.toFixed(0)}%`);
    parts.push(mem ?? `mem ${MEM_NA}`);
  } else if (mem !== undefined) {
    parts.push(mem);
  }
  if (g.temp_c !== undefined) parts.push(`${g.temp_c.toFixed(0)}°C`);
  if (g.power_w !== undefined) parts.push(`${g.power_w.toFixed(0)} W`);
  return parts.length > 0 ? parts.join(' · ') : 'no data';
}

// gpuDetailRows are the label/value rows of a GPU card on the device detail
// page. A suspended GPU hides its (stale) utilisation, temperature and power
// but keeps its memory. Empty when there is nothing to show; callers fall back
// to gpuSummary.
export function gpuDetailRows(g: GPU): [string, string][] {
  const rows: [string, string][] = [];
  if (!g.suspended && g.util_percent !== undefined) rows.push(['Utilization', `${g.util_percent.toFixed(0)}%`]);
  if (g.mem_used_bytes !== undefined) {
    const pct = g.mem_total_bytes !== undefined && g.mem_total_bytes > 0
      ? ` (${((g.mem_used_bytes / g.mem_total_bytes) * 100).toFixed(1)}%)`
      : '';
    rows.push(['VRAM', `${gpuMemory(g)}${pct}`]);
  } else if (rows.length > 0) {
    rows.push(['VRAM', MEM_NA]);
  }
  if (!g.suspended) {
    if (g.temp_c !== undefined) rows.push(['Temperature', `${g.temp_c.toFixed(0)}°C`]);
    if (g.power_w !== undefined) rows.push(['Power', `${g.power_w.toFixed(0)} W`]);
  }
  return rows;
}

// gpuCardLines is what DeviceCard shows: one line per GPU for up to two, or for
// more a max-utilisation rollup plus a summed VRAM line. The VRAM total is only
// shown when every GPU reporting memory also reports a total.
export function gpuCardLines(gpus: GPU[] | undefined): string[] {
  if (!gpus || gpus.length === 0) return [];
  if (gpus.length <= 2) {
    const label = (g: GPU) => (gpus.length === 1 ? 'GPU' : `GPU${g.index}`);
    return gpus.map((g) => `${label(g)}: ${gpuSummary(g)}`);
  }
  const utils = gpus.flatMap((g) => (g.util_percent !== undefined && !g.suspended ? [g.util_percent] : []));
  const peak = utils.length > 0 ? ` · max ${Math.max(...utils).toFixed(0)}%` : '';
  const withMem = gpus.filter((g) => g.mem_used_bytes !== undefined);
  let mem = MEM_NA;
  if (withMem.length > 0) {
    const used = withMem.reduce((sum, g) => sum + (g.mem_used_bytes ?? 0), 0);
    mem = withMem.every((g) => g.mem_total_bytes !== undefined)
      ? `${bytes(used)} / ${bytes(withMem.reduce((sum, g) => sum + (g.mem_total_bytes ?? 0), 0))}`
      : bytes(used);
  }
  return [`GPUs: ${gpus.length}${peak}`, `VRAM: ${mem}`];
}

// gpuSeries is the utilisation history for GPUs present in the latest sample.
// Samples where a GPU is missing or reports no utilisation count as 0.
export function gpuSeries(history: Metrics[]): { index: number; name: string; values: number[] }[] {
  const latest = history[history.length - 1]?.gpus ?? [];
  return latest.map((g) => ({
    index: g.index,
    name: g.name,
    values: history.map((m) => m.gpus?.find((x) => x.index === g.index)?.util_percent ?? 0),
  }));
}
