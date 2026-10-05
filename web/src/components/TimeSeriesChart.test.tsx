import { describe, it, expect } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { TimeSeriesChart } from './TimeSeriesChart'
import { bytesScale, historySeries } from '../history'
import { bytes } from '../format'
import type { HistorySample } from '../api'

const GB = 1024 ** 3
const range = { start: 0, end: 3600, bucketS: 900 }

// gpuMemChart renders the GPU memory chart exactly as the device detail page
// wires it from history samples.
function gpuMemChart(samples: HistorySample[]): string {
  const s = historySeries(samples)
  return renderToStaticMarkup(
    <TimeSeriesChart
      label="GPU memory"
      t={s.t}
      {...range}
      values={s.gpuMem?.used ?? []}
      reference={s.gpuMem?.total}
      max={s.gpuMem ? bytesScale(s.gpuMem.used, s.gpuMem.total) : 100}
      format={bytes}
      empty="No GPU reported in this range"
    />,
  )
}

const count = (html: string, re: RegExp) => (html.match(re) ?? []).length

describe('GPU memory chart', () => {
  it('plots used bytes on a byte axis with a dashed total line', () => {
    const html = gpuMemChart([
      { t: 0, cpu: 0, mem_pct: 0, gpu_mem_used_bytes: 2 * GB, gpu_mem_total_bytes: 8 * GB },
      { t: 900, cpu: 0, mem_pct: 0, gpu_mem_used_bytes: 4 * GB, gpu_mem_total_bytes: 8 * GB },
    ])
    expect(html).toContain('8.0 GB') // y-axis max is the total
    expect(html).toContain('now 4.0 GB · min 2.0 GB · max 4.0 GB')
    expect(html).toContain('total 8.0 GB')
    expect(count(html, /data-reference="true"[^>]*stroke-dasharray="6 4"/g)).toBe(1)
    // Used: two points at 25% and 50% of the height (120).
    expect(html).toMatch(/points="0\.0,90\.0 150\.0,60\.0"/)
    expect(html).not.toContain('No GPU reported')
  })

  it('handles an absent total (Apple unified memory) without a reference line', () => {
    const html = gpuMemChart([
      { t: 0, cpu: 0, mem_pct: 0, gpu_mem_used_bytes: 1 * GB },
      { t: 900, cpu: 0, mem_pct: 0, gpu_mem_used_bytes: 2 * GB },
    ])
    expect(html).not.toContain('data-reference')
    expect(html).not.toContain('total')
    expect(html).toContain('2.5 GB') // peak plus headroom
  })

  it('breaks the line at samples without GPU memory instead of plotting zeros', () => {
    const html = gpuMemChart([
      { t: 0, cpu: 0, mem_pct: 0, gpu_mem_used_bytes: 2 * GB },
      { t: 900, cpu: 0, mem_pct: 0, gpu_mem_used_bytes: 2 * GB },
      { t: 1800, cpu: 0, mem_pct: 0 },
      { t: 2700, cpu: 0, mem_pct: 0, gpu_mem_used_bytes: 2 * GB },
      { t: 3600, cpu: 0, mem_pct: 0, gpu_mem_used_bytes: 2 * GB },
    ])
    expect(count(html, /<polyline/g)).toBe(2)
    expect(html).not.toContain(',120.0') // nothing drawn at zero
    expect(html).toContain('min 2.0 GB')
  })

  it('shows the empty state when the device reports no GPU', () => {
    const html = gpuMemChart([{ t: 0, cpu: 1, mem_pct: 1 }])
    expect(html).toContain('No GPU reported in this range')
    expect(html).not.toContain('<polyline')
  })

  it('keeps percentage charts unchanged', () => {
    const html = renderToStaticMarkup(<TimeSeriesChart label="CPU %" t={[0, 900]} values={[10, 50]} {...range} />)
    expect(html).toContain('100%')
    expect(html).toContain('now 50% · min 10% · max 50%')
  })
})
