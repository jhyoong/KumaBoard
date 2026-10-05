import { describe, it, expect } from 'vitest';
import { gpuCardLines, gpuDetailRows, gpuMemory, gpuSeries, gpuSummary } from './gpu';
import type { GPU, Metrics } from './api';

const base: Metrics = {
  cpu_percent: 1, mem_total_bytes: 1e9, mem_used_bytes: 5e8,
  mem_used_percent: 50, disk_total_bytes: 1e10, disk_used_bytes: 5e9,
  disk_used_percent: 50, uptime_s: 3600, load1: 1, load5: 1, load15: 1,
};

const amd: GPU = {
  index: 0, name: 'AMD Radeon RX 6600', vendor: 'amd', util_percent: 37,
  mem_used_bytes: 2254857830, mem_total_bytes: 8573157376, temp_c: 61, power_w: 118,
};
const apple: GPU = { index: 0, name: 'Apple M2 Max', vendor: 'apple', util_percent: 12, mem_used_bytes: 1234567890 };

describe('gpuSummary', () => {
  it('renders every reported field', () => {
    expect(gpuSummary(amd)).toBe('37% · 2.1 GB / 8.0 GB · 61°C · 118 W');
  });

  it('omits absent fields instead of showing zero', () => {
    // Apple: unified memory, no total, no temp/power.
    expect(gpuSummary(apple)).toBe('12% · 1.1 GB');
    // Intel iGPU on its first tick: nothing yet.
    expect(gpuSummary({ index: 1, name: 'Intel GPU', vendor: 'intel' })).toBe('no data');
  });

  it('shows asleep for a suspended GPU, keeping memory when reported', () => {
    expect(gpuSummary({ index: 0, name: 'x', vendor: 'amd', util_percent: 0, suspended: true })).toBe('asleep');
    expect(gpuSummary({ ...amd, suspended: true })).toBe('asleep · 2.1 GB / 8.0 GB');
  });

  it('shows a memory placeholder when usage is reported without memory', () => {
    expect(gpuSummary({ index: 0, name: 'x', vendor: 'intel', util_percent: 5 })).toBe('5% · mem n/a');
    expect(gpuSummary({ index: 0, name: 'x', vendor: 'intel', util_percent: 5, temp_c: 40 })).toBe('5% · mem n/a · 40°C');
  });

  it('shows memory alone when usage is absent', () => {
    expect(gpuSummary({ index: 0, name: 'x', vendor: 'amd', mem_used_bytes: 2254857830 })).toBe('2.1 GB');
  });
});

describe('gpuMemory', () => {
  it('renders used / total, used alone, or nothing', () => {
    expect(gpuMemory(amd)).toBe('2.1 GB / 8.0 GB');
    expect(gpuMemory(apple)).toBe('1.1 GB');
    expect(gpuMemory({ index: 0, name: 'x', vendor: 'intel', util_percent: 5 })).toBeUndefined();
  });
});

describe('gpuDetailRows', () => {
  it('shows usage and memory with percentage', () => {
    expect(gpuDetailRows(amd)).toEqual([
      ['Utilization', '37%'],
      ['VRAM', '2.1 GB / 8.0 GB (26.3%)'],
      ['Temperature', '61°C'],
      ['Power', '118 W'],
    ]);
  });

  it('shows memory without percentage when there is no total', () => {
    expect(gpuDetailRows(apple)).toEqual([['Utilization', '12%'], ['VRAM', '1.1 GB']]);
  });

  it('shows a VRAM placeholder when usage is reported without memory', () => {
    expect(gpuDetailRows({ index: 0, name: 'x', vendor: 'intel', util_percent: 5 }))
      .toEqual([['Utilization', '5%'], ['VRAM', 'n/a']]);
  });

  it('keeps memory for a suspended GPU but hides stale readings', () => {
    expect(gpuDetailRows({ ...amd, suspended: true })).toEqual([['VRAM', '2.1 GB / 8.0 GB (26.3%)']]);
    expect(gpuDetailRows({ index: 0, name: 'x', vendor: 'amd', util_percent: 0, suspended: true })).toEqual([]);
  });

  it('is empty when nothing is reported', () => {
    expect(gpuDetailRows({ index: 1, name: 'Intel GPU', vendor: 'intel' })).toEqual([]);
  });
});

describe('gpuCardLines', () => {
  it('is empty for pre-GPU agents and GPU-less devices', () => {
    expect(gpuCardLines(undefined)).toEqual([]);
    expect(gpuCardLines([])).toEqual([]);
  });

  it('labels one GPU plainly and two by index', () => {
    expect(gpuCardLines([amd])).toEqual(['GPU: 37% · 2.1 GB / 8.0 GB · 61°C · 118 W']);
    const lines = gpuCardLines([amd, { index: 1, name: 'Intel', vendor: 'intel', suspended: true }]);
    expect(lines).toEqual(['GPU0: 37% · 2.1 GB / 8.0 GB · 61°C · 118 W', 'GPU1: asleep']);
  });

  it('shows usage and memory together for one or two GPUs', () => {
    expect(gpuCardLines([apple])).toEqual(['GPU: 12% · 1.1 GB']);
    expect(gpuCardLines([{ index: 0, name: 'x', vendor: 'intel', util_percent: 5 }])).toEqual(['GPU: 5% · mem n/a']);
    expect(gpuCardLines([amd, { ...amd, index: 1, suspended: true }]))
      .toEqual(['GPU0: 37% · 2.1 GB / 8.0 GB · 61°C · 118 W', 'GPU1: asleep · 2.1 GB / 8.0 GB']);
  });

  it('rolls up more than two GPUs to the max utilisation and summed VRAM', () => {
    const gpus: GPU[] = [
      { ...amd, index: 0, util_percent: 10 },
      { ...amd, index: 1, util_percent: 80 },
      { ...amd, index: 2, util_percent: 95, suspended: true },
    ];
    expect(gpuCardLines(gpus)).toEqual(['GPUs: 3 · max 80%', 'VRAM: 6.3 GB / 24.0 GB']);
  });

  it('rolls up VRAM as used only when any GPU lacks a total, or n/a when none report memory', () => {
    const intel: GPU = { index: 2, name: 'Intel', vendor: 'intel', util_percent: 3 };
    expect(gpuCardLines([amd, { ...apple, index: 1 }, intel])).toEqual(['GPUs: 3 · max 37%', 'VRAM: 3.2 GB']);
    expect(gpuCardLines([intel, { ...intel, index: 3 }, { ...intel, index: 4 }])).toEqual(['GPUs: 3 · max 3%', 'VRAM: n/a']);
  });
});

describe('gpuSeries', () => {
  it('has no series when the latest sample has no gpus', () => {
    expect(gpuSeries([])).toEqual([]);
    expect(gpuSeries([base, base])).toEqual([]);
  });

  it('tracks each GPU by index, filling gaps with 0', () => {
    const history: Metrics[] = [
      base, // from before the agent gained the gpu capability
      { ...base, gpus: [{ ...amd, util_percent: 20 }] },
      { ...base, gpus: [{ ...amd, util_percent: undefined }] }, // first-tick style gap
      { ...base, gpus: [{ ...amd, util_percent: 40 }] },
    ];
    expect(gpuSeries(history)).toEqual([{ index: 0, name: 'AMD Radeon RX 6600', values: [0, 20, 0, 40] }]);
  });
});
