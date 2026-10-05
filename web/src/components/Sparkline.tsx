// color is any CSS colour; pass a chart token (var(--color-chart-N)) so the
// line re-themes with light/dark. unit follows the latest-value readout, and
// valueClass colours it (e.g. a temperature threshold class).
export function Sparkline({ values, max = 100, label, color = 'var(--color-chart-1)', unit = '%', valueClass }: {
  values: number[]; max?: number; label: string; color?: string; unit?: string; valueClass?: string
}) {
  const w = 160
  const h = 32
  if (values.length < 2) return <div className="text-xs text-fg-subtle">{label}: collecting</div>
  const step = w / (values.length - 1)
  const pts = values.map((v, i) => `${(i * step).toFixed(1)},${(h - (Math.min(v, max) / max) * h).toFixed(1)}`).join(' ')
  return (
    <div className="flex items-center gap-2 text-xs text-fg-muted">
      <span className="w-14">{label}</span>
      <svg width={w} height={h} aria-label={label}>
        <line x1="0" y1={h - 0.5} x2={w} y2={h - 0.5} stroke="var(--color-chart-grid)" strokeWidth="1" />
        <polyline points={pts} fill="none" stroke={color} strokeWidth="1.5" />
      </svg>
      <span className={valueClass || undefined}>{values[values.length - 1].toFixed(0)}{unit}</span>
    </div>
  )
}
