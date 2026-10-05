import { useState, type MouseEvent } from 'react'

interface Props {
  label: string
  // Bucket start times (unix seconds) and values, index-aligned. A null value
  // is a gap: it is not drawn and breaks the line.
  t: number[]
  values: (number | null)[]
  // The x domain, unix seconds; points are placed by time so offline gaps show.
  start: number
  end: number
  // Consecutive points further apart than two buckets are not joined.
  bucketS: number
  color?: string
  // Shown instead of the plot when there is nothing to draw.
  empty?: string
  // The y-axis maximum and value formatter; a 0-100% scale by default.
  max?: number
  format?: (v: number) => string
  // An optional dashed reference line (e.g. a capacity), index-aligned with t;
  // null where unknown.
  reference?: (number | null)[]
  referenceLabel?: string
}

const W = 600
const H = 120

function stamp(s: number): string {
  return new Date(s * 1000).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
}

function pct(v: number): string {
  return `${v.toFixed(v < 10 ? 1 : 0)}%`
}

// segmentsOf turns index-aligned times and values into polyline point lists,
// breaking at nulls and at gaps longer than two buckets.
function segmentsOf(t: number[], values: (number | null)[], bucketS: number, x: (s: number) => number, y: (v: number) => number): string[] {
  const segments: string[] = []
  let cur: string[] = []
  let prev = -1
  values.forEach((v, i) => {
    if (v === null) return
    if (cur.length > 0 && (prev !== i - 1 || t[i] - t[prev] > 2 * bucketS)) {
      segments.push(cur.join(' '))
      cur = []
    }
    cur.push(`${x(t[i]).toFixed(1)},${y(v).toFixed(1)}`)
    prev = i
  })
  if (cur.length > 0) segments.push(cur.join(' '))
  return segments
}

// TimeSeriesChart is a line chart (0-100% unless max/format say otherwise)
// with a y-axis frame, min/max labels, time-axis endpoints, an optional dashed
// reference line and a hover readout. The SVG stretches to its container; text
// is HTML so it does not distort. color takes a chart token.
export function TimeSeriesChart({
  label, t, values, start, end, bucketS, color = 'var(--color-chart-1)', empty = 'No data in this range',
  max: yMax = 100, format = pct, reference, referenceLabel = 'total',
}: Props) {
  const [hover, setHover] = useState<number | null>(null)
  const span = Math.max(end - start, 1)
  const x = (s: number) => ((s - start) / span) * W
  const y = (v: number) => H - (Math.max(0, Math.min(v, yMax)) / yMax) * H

  const segments = segmentsOf(t, values, bucketS, x, y)
  const refSegments = reference ? segmentsOf(t, reference, bucketS, x, y) : []

  const present = values.filter((v): v is number => v !== null)
  const has = present.length > 0
  const min = has ? Math.min(...present) : 0
  const max = has ? Math.max(...present) : 0
  const last = has ? present[present.length - 1] : 0
  const lastRef = reference?.findLast((v) => v !== null) ?? null

  const onMove = (e: MouseEvent<HTMLDivElement>) => {
    if (!has) return
    const r = e.currentTarget.getBoundingClientRect()
    const at = start + ((e.clientX - r.left) / r.width) * span
    let best = -1
    for (let i = 0; i < t.length; i++) {
      if (values[i] !== null && (best < 0 || Math.abs(t[i] - at) < Math.abs(t[best] - at))) best = i
    }
    setHover(best < 0 ? null : best)
  }
  const hoverValue = hover !== null ? values[hover] : null
  const hoverRef = hover !== null ? reference?.[hover] ?? null : null

  return (
    <figure className="rounded-lg border border-border bg-surface p-3">
      <figcaption className="mb-2 flex items-baseline justify-between text-sm">
        <span className="font-medium text-fg">{label}</span>
        {has && (
          <span className="text-xs text-fg-muted">
            now {format(last)} · min {format(min)} · max {format(max)}
            {lastRef !== null && <> · <span className="text-fg-subtle">- - {referenceLabel} {format(lastRef)}</span></>}
          </span>
        )}
      </figcaption>
      <div className="flex gap-2">
        <div className="flex h-32 w-9 flex-col justify-between text-right text-[10px] leading-none text-fg-subtle">
          <span>{format(yMax)}</span>
          <span>{format(yMax / 2)}</span>
          <span>{format(0)}</span>
        </div>
        <div className="relative h-32 flex-1" onMouseMove={onMove} onMouseLeave={() => setHover(null)}>
          <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" className="h-full w-full" role="img" aria-label={`${label} chart`}>
            <line x1="0" y1={H / 2} x2={W} y2={H / 2} stroke="var(--color-chart-grid)" strokeDasharray="4 4" vectorEffect="non-scaling-stroke" />
            <rect x="0" y="0" width={W} height={H} fill="none" stroke="var(--color-chart-axis)" strokeWidth="1" vectorEffect="non-scaling-stroke" />
            {refSegments.map((pts, i) => (
              <polyline key={`ref${i}`} data-reference points={pts} fill="none" stroke="var(--color-chart-axis)" strokeWidth="1.5" strokeDasharray="6 4" vectorEffect="non-scaling-stroke" />
            ))}
            {segments.map((pts, i) =>
              pts.includes(' ')
                ? <polyline key={i} points={pts} fill="none" stroke={color} strokeWidth="2" strokeLinejoin="round" vectorEffect="non-scaling-stroke" />
                : <circle key={i} cx={pts.split(',')[0]} cy={pts.split(',')[1]} r="1.5" fill={color} />,
            )}
            {hover !== null && hoverValue !== null && (
              <line x1={x(t[hover])} y1="0" x2={x(t[hover])} y2={H} stroke="var(--color-chart-axis)" vectorEffect="non-scaling-stroke" />
            )}
          </svg>
          {!has && (
            <div className="absolute inset-0 flex items-center justify-center text-sm text-fg-subtle">{empty}</div>
          )}
          {hover !== null && hoverValue !== null && (
            <div
              className="pointer-events-none absolute top-1 whitespace-nowrap rounded border border-border bg-surface px-2 py-1 text-xs text-fg shadow-sm"
              style={x(t[hover]) > W / 2 ? { right: `${100 - (x(t[hover]) / W) * 100}%`, marginRight: 6 } : { left: `${(x(t[hover]) / W) * 100}%`, marginLeft: 6 }}
            >
              {format(hoverValue)}{hoverRef !== null && ` / ${format(hoverRef)}`} · <span className="text-fg-muted">{stamp(t[hover])}</span>
            </div>
          )}
        </div>
      </div>
      <div className="mt-1 flex justify-between pl-11 text-[10px] text-fg-subtle">
        <span>{stamp(start)}</span>
        <span>{stamp(end)}</span>
      </div>
    </figure>
  )
}
