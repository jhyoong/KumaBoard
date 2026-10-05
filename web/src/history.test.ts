import { describe, it, expect } from 'vitest';
import { RANGES, bytesScale, gpuMemBytes, gpuPercent, historySeries, mergeLive } from './history';
import type { HistorySample, Metrics } from './api';

const base: Metrics = {
  cpu_percent: 12, mem_total_bytes: 1000, mem_used_bytes: 250,
  mem_used_percent: 25, disk_total_bytes: 1e10, disk_used_bytes: 5e9,
  disk_used_percent: 50, uptime_s: 3600, load1: 1, load5: 1, load15: 1,
};

const now = 1_800_000_015; // 15s into a bucket
const bucket = 1_800_000_000;

describe('gpuPercent', () => {
  it('is undefined without reporting GPUs', () => {
    expect(gpuPercent(base)).toBeUndefined();
    expect(gpuPercent({ ...base, gpus: [{ index: 0, name: 'x', vendor: 'intel' }] })).toBeUndefined();
  });

  it('takes the busiest GPU and treats suspended as idle', () => {
    expect(gpuPercent({ ...base, gpus: [
      { index: 0, name: 'a', vendor: 'amd', util_percent: 30 },
      { index: 1, name: 'b', vendor: 'amd', util_percent: 80, suspended: true },
    ] })).toBe(30);
    expect(gpuPercent({ ...base, gpus: [{ index: 0, name: 'a', vendor: 'amd', suspended: true }] })).toBe(0);
  });
});

describe('mergeLive', () => {
  const history: HistorySample[] = [
    { t: bucket - 3600, cpu: 1, mem_pct: 1 }, // falls out of the window
    { t: bucket - 60, cpu: 2, mem_pct: 2 },
    { t: bucket - 30, cpu: 3, mem_pct: 3 },
    { t: bucket, cpu: 4, mem_pct: 4 },
  ];

  it('replaces the current bucket with the live sample', () => {
    expect(mergeLive(history, base, now)).toEqual([
      { t: bucket - 60, cpu: 2, mem_pct: 2 },
      { t: bucket - 30, cpu: 3, mem_pct: 3 },
      { t: bucket, cpu: 12, mem_pct: 25, disk_pct: 50 },
    ]);
  });

  it('appends when the current bucket is new, carrying GPU when present', () => {
    const m = { ...base, gpus: [{ index: 0, name: 'a', vendor: 'amd', util_percent: 40 }] };
    const out = mergeLive(history, m, now + 30);
    expect(out[out.length - 1]).toEqual({ t: bucket + 30, cpu: 12, mem_pct: 25, gpu_pct: 40, disk_pct: 50 });
    expect(out).toHaveLength(4);
  });

  it('only trims the window without a live sample', () => {
    expect(mergeLive(history, null, now)).toHaveLength(3);
  });
});

describe('mergeLive across ranges', () => {
  // 1_800_000_000 is a multiple of 900; now is 7.5 minutes into that bucket.
  const rb = 1_800_000_000;
  const at = rb + 450;
  const rolled: HistorySample[] = [
    { t: rb - 8 * 86400, cpu: 1, mem_pct: 1, disk_pct: 10 }, // outside 7d, inside 30d
    { t: rb - 2 * 86400, cpu: 2, mem_pct: 2, disk_pct: 20 }, // outside 24h
    { t: rb - 900, cpu: 3, mem_pct: 3, disk_pct: 30 },
    { t: rb, cpu: 4, mem_pct: 4, disk_pct: 40 }, // partial bucket in progress
  ];

  it('trims to each window', () => {
    expect(mergeLive(rolled, null, at, RANGES['24h']).map((s) => s.t)).toEqual([rb - 900, rb]);
    expect(mergeLive(rolled, null, at, RANGES['7d'])).toHaveLength(3);
    expect(mergeLive(rolled, null, at, RANGES['30d'])).toHaveLength(4);
  });

  it('folds live into the 15-minute bucket in progress', () => {
    const out = mergeLive(rolled, base, at, RANGES['24h']);
    expect(out).toEqual([
      { t: rb - 900, cpu: 3, mem_pct: 3, disk_pct: 30 },
      { t: rb, cpu: 12, mem_pct: 25, disk_pct: 50 },
    ]);
  });

  it('appends a new 15-minute bucket once the previous one closes', () => {
    const out = mergeLive(rolled, base, rb + 900 + 5, RANGES['24h']);
    expect(out.map((s) => s.t)).toEqual([rb - 900, rb, rb + 900]);
    // A 30s step within the same rollup bucket does not append.
    expect(mergeLive(rolled, base, at + 30, RANGES['24h'])).toHaveLength(2);
  });

  it('defaults to the 1h range', () => {
    expect(mergeLive(rolled, null, at)).toEqual(mergeLive(rolled, null, at, RANGES['1h']));
  });
});

describe('RANGES', () => {
  it('matches the server windows and bucket sizes', () => {
    expect(Object.keys(RANGES)).toEqual(['1h', '24h', '7d', '30d']);
    expect(RANGES['1h']).toMatchObject({ windowS: 3600, bucketS: 30 });
    expect(RANGES['24h']).toMatchObject({ windowS: 86400, bucketS: 900 });
    expect(RANGES['7d']).toMatchObject({ windowS: 604800, bucketS: 900 });
    expect(RANGES['30d']).toMatchObject({ windowS: 2592000, bucketS: 900 });
  });
});

describe('historySeries', () => {
  it('has no GPU series when no sample reported one', () => {
    expect(historySeries([{ t: 0, cpu: 5, mem_pct: 50 }, { t: 30, cpu: 6, mem_pct: 60 }]))
      .toEqual({ t: [0, 30], cpu: [5, 6], mem: [50, 60], disk: [0, 0], gpu: null, gpuMem: null, temp: null });
  });

  it('passes disk_pct through', () => {
    expect(historySeries([{ t: 0, cpu: 5, mem_pct: 50, disk_pct: 71.5 }, { t: 900, cpu: 6, mem_pct: 60, disk_pct: 72 }]).disk)
      .toEqual([71.5, 72]);
  });

  it('fills GPU gaps with 0 once any sample has one', () => {
    expect(historySeries([{ t: 0, cpu: 5, mem_pct: 50 }, { t: 30, cpu: 6, mem_pct: 60, gpu_pct: 70 }]).gpu)
      .toEqual([0, 70]);
  });
});

describe('GPU memory history', () => {
  it('sums GPU memory like the server, total only when every GPU has one', () => {
    expect(gpuMemBytes(base)).toEqual({});
    expect(gpuMemBytes({ ...base, gpus: [{ index: 0, name: 'x', vendor: 'amd', util_percent: 5 }] })).toEqual({});
    expect(gpuMemBytes({ ...base, gpus: [
      { index: 0, name: 'a', vendor: 'nvidia', mem_used_bytes: 2, mem_total_bytes: 8 },
      { index: 1, name: 'b', vendor: 'nvidia', suspended: true, mem_used_bytes: 1, mem_total_bytes: 4 },
    ] })).toEqual({ used: 3, total: 12 });
    // Apple unified memory has no total.
    expect(gpuMemBytes({ ...base, gpus: [{ index: 0, name: 'm', vendor: 'apple', mem_used_bytes: 7 }] })).toEqual({ used: 7 });
  });

  it('folds live GPU memory into the current bucket', () => {
    const got = mergeLive([], { ...base, gpus: [{ index: 0, name: 'a', vendor: 'amd', mem_used_bytes: 2, mem_total_bytes: 8 }] }, now);
    expect(got[0]).toMatchObject({ t: bucket, gpu_mem_used_bytes: 2, gpu_mem_total_bytes: 8 });
    expect(mergeLive([], base, now)[0]).not.toHaveProperty('gpu_mem_used_bytes');
  });

  it('has no GPU memory series when no sample reported it', () => {
    expect(historySeries([{ t: 0, cpu: 5, mem_pct: 50, gpu_pct: 10 }]).gpuMem).toBeNull();
  });

  it('keeps missing samples and unknown totals as gaps, not zeros', () => {
    const samples: HistorySample[] = [
      { t: 0, cpu: 1, mem_pct: 1, gpu_mem_used_bytes: 100, gpu_mem_total_bytes: 800 },
      { t: 900, cpu: 1, mem_pct: 1 },
      { t: 1800, cpu: 1, mem_pct: 1, gpu_mem_used_bytes: 300 },
    ];
    expect(historySeries(samples).gpuMem).toEqual({ used: [100, null, 300], total: [800, null, null] });
  });

  it('scales to the total when known, else to the peak with headroom', () => {
    expect(bytesScale([100, null], [800, null])).toBe(800);
    expect(bytesScale([100, 400], [null, null])).toBe(500);
    expect(bytesScale([null], [null])).toBe(1);
  });
});

describe('temperature history', () => {
  it('folds live temperature into the current bucket only when reported', () => {
    expect(mergeLive([], { ...base, temp_c: 54, temp_sensor: 'k10temp/Tctl' }, now)[0])
      .toEqual({ t: bucket, cpu: 12, mem_pct: 25, disk_pct: 50, temp_c: 54 });
    expect(mergeLive([], base, now)[0]).not.toHaveProperty('temp_c');
  });

  it('has no temperature series when no sample reported one', () => {
    expect(historySeries([{ t: 0, cpu: 5, mem_pct: 50 }, { t: 30, cpu: 6, mem_pct: 60 }]).temp).toBeNull();
  });

  it('keeps samples without a temperature as gaps, not zeros', () => {
    const samples: HistorySample[] = [
      { t: 0, cpu: 1, mem_pct: 1, temp_c: 50 },
      { t: 30, cpu: 1, mem_pct: 1 },
      { t: 60, cpu: 1, mem_pct: 1, temp_c: 61.5 },
    ];
    expect(historySeries(samples).temp).toEqual([50, null, 61.5]);
  });
});
