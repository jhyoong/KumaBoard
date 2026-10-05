// Shaping for metrics history: the 1h sparklines on device cards and the
// 24h/7d/30d charts on the device detail page.

import type { HistorySample, Metrics } from './api';

// Match the server's storage resolutions (store.MetricsBucket and
// store.MetricsRollupBucket).
export const BUCKET_S = 30;
export const ROLLUP_BUCKET_S = 900;
export const WINDOW_S = 3600;

export type HistoryWindow = '1h' | '24h' | '7d' | '30d';

// Range is a ?window= value with its span and the bucket size it is served at.
export interface Range {
  window: HistoryWindow;
  windowS: number;
  bucketS: number;
}

const DAY_S = 86400;

export const RANGES: Record<HistoryWindow, Range> = {
  '1h': { window: '1h', windowS: WINDOW_S, bucketS: BUCKET_S },
  '24h': { window: '24h', windowS: DAY_S, bucketS: ROLLUP_BUCKET_S },
  '7d': { window: '7d', windowS: 7 * DAY_S, bucketS: ROLLUP_BUCKET_S },
  '30d': { window: '30d', windowS: 30 * DAY_S, bucketS: ROLLUP_BUCKET_S },
};

// gpuPercent mirrors the server rollup: the busiest GPU, suspended counting as
// idle, adapters with no utilisation skipped. Undefined when none contribute.
export function gpuPercent(m: Metrics): number | undefined {
  let out: number | undefined;
  for (const g of m.gpus ?? []) {
    const v = g.suspended ? 0 : g.util_percent;
    if (v !== undefined && (out === undefined || v > out)) out = v;
  }
  return out;
}

// gpuMemBytes mirrors the server's GPU memory rollup: used memory summed over
// the GPUs reporting it, and the summed total only when each of them reports
// one. used is undefined when no GPU reports memory.
export function gpuMemBytes(m: Metrics): { used?: number; total?: number } {
  const withMem = (m.gpus ?? []).filter((g) => g.mem_used_bytes !== undefined);
  if (withMem.length === 0) return {};
  const used = withMem.reduce((sum, g) => sum + (g.mem_used_bytes ?? 0), 0);
  if (!withMem.every((g) => g.mem_total_bytes !== undefined)) return { used };
  return { used, total: withMem.reduce((sum, g) => sum + (g.mem_total_bytes ?? 0), 0) };
}

// mergeLive folds the latest live sample into the fetched history so the tail
// tracks SSE between refetches. It replaces the sample for the current bucket
// of the range (or appends one) and drops anything older than the window.
export function mergeLive(samples: HistorySample[], m: Metrics | null, nowS: number, range: Range = RANGES['1h']): HistorySample[] {
  const cutoff = nowS - range.windowS;
  const kept = samples.filter((s) => s.t >= cutoff);
  if (!m) return kept;
  const t = nowS - (nowS % range.bucketS);
  const live: HistorySample = {
    t,
    cpu: m.cpu_percent,
    mem_pct: m.mem_total_bytes > 0 ? (m.mem_used_bytes / m.mem_total_bytes) * 100 : 0,
  };
  const gpu = gpuPercent(m);
  if (gpu !== undefined) live.gpu_pct = gpu;
  const gpuMem = gpuMemBytes(m);
  if (gpuMem.used !== undefined) live.gpu_mem_used_bytes = gpuMem.used;
  if (gpuMem.total !== undefined) live.gpu_mem_total_bytes = gpuMem.total;
  live.disk_pct = m.disk_used_percent;
  if (m.temp_c !== undefined) live.temp_c = m.temp_c;
  const before = kept.filter((s) => s.t < t);
  return [...before, live];
}

// historySeries splits samples into chart series. gpu is null when no sample
// in the window reported a GPU; gaps in a GPU series count as 0. disk is 0 for
// samples from servers that predate disk history. gpuMem is null when no
// sample reported GPU memory; otherwise samples without it are null (a gap,
// never a fake zero), and total is null wherever the total is unknown. temp is
// null when no sample reported a temperature; otherwise samples without one
// are null gaps.
export interface HistorySeries {
  t: number[];
  cpu: number[];
  mem: number[];
  disk: number[];
  gpu: number[] | null;
  gpuMem: { used: (number | null)[]; total: (number | null)[] } | null;
  temp: (number | null)[] | null;
}

export function historySeries(samples: HistorySample[]): HistorySeries {
  const hasGPU = samples.some((s) => s.gpu_pct !== undefined);
  const hasGPUMem = samples.some((s) => s.gpu_mem_used_bytes !== undefined);
  const hasTemp = samples.some((s) => s.temp_c !== undefined);
  return {
    t: samples.map((s) => s.t),
    cpu: samples.map((s) => s.cpu),
    mem: samples.map((s) => s.mem_pct),
    disk: samples.map((s) => s.disk_pct ?? 0),
    gpu: hasGPU ? samples.map((s) => s.gpu_pct ?? 0) : null,
    gpuMem: hasGPUMem
      ? {
        used: samples.map((s) => s.gpu_mem_used_bytes ?? null),
        total: samples.map((s) => (s.gpu_mem_used_bytes !== undefined ? s.gpu_mem_total_bytes ?? null : null)),
      }
      : null,
    temp: hasTemp ? samples.map((s) => s.temp_c ?? null) : null,
  };
}

// bytesScale is the y-axis maximum for a byte chart: the largest total when
// one is known, else the peak used value with 25% headroom. At least 1.
export function bytesScale(used: (number | null)[], total: (number | null)[]): number {
  const totals = total.filter((v): v is number => v !== null);
  if (totals.length > 0) return Math.max(1, ...totals, ...used.filter((v): v is number => v !== null));
  const peak = Math.max(0, ...used.filter((v): v is number => v !== null));
  return peak > 0 ? peak * 1.25 : 1;
}
