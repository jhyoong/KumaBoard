import { describe, it, expect } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { Sparkline } from './Sparkline'

describe('Sparkline', () => {
  it('reads out a percentage by default', () => {
    expect(renderToStaticMarkup(<Sparkline label="CPU 1h" values={[10, 12.4]} />)).toContain('<span>12%</span>')
  })

  it('takes a unit and a readout class', () => {
    const html = renderToStaticMarkup(<Sparkline label="Temp 1h" values={[50, 96]} max={110} unit="°C" valueClass="text-danger" />)
    expect(html).toContain('<span class="text-danger">96°C</span>')
  })
})
