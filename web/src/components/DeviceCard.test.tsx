import { describe, it, expect } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { MemoryRouter } from 'react-router'
import { DeviceCard } from './DeviceCard'
import type { Device, Metrics } from '../api'

const metrics: Metrics = {
  cpu_percent: 12.5, mem_total_bytes: 16e9, mem_used_bytes: 6e9,
  mem_used_percent: 37.5, disk_total_bytes: 5e11, disk_used_bytes: 1e11,
  disk_used_percent: 20, uptime_s: 86400, load1: 0.4, load5: 0.3, load15: 0.2,
}

// Disconnected, so the card renders without fetching sparkline history.
const device: Device = {
  name: 'linux-server', state: 'online', connected: false, os: 'linux', arch: 'amd64',
  agent_version: '0.4.0', protocol_version: 1, desired_agent_version: '',
  capabilities: [], commands: [], mac: '', normally_off: false, schedule: null,
  last_seen: '', last_disconnect_at: '', incompatible: false, terminal_enabled: false,
  reject_reason: '', metrics,
}

function card(m: Metrics): string {
  return renderToStaticMarkup(<MemoryRouter><DeviceCard device={{ ...device, metrics: m }} /></MemoryRouter>)
}

describe('DeviceCard temperature', () => {
  it('shows a Temp cell with the sensor in its tooltip', () => {
    expect(card({ ...metrics, temp_c: 54, temp_sensor: 'k10temp/Tctl' }))
      .toContain('<div title="k10temp/Tctl">Temp: <span>54°C</span></div>')
  })

  it('colours warn and crit readings', () => {
    expect(card({ ...metrics, temp_c: 85 })).toContain('<span class="text-warning">85°C</span>')
    expect(card({ ...metrics, temp_c: 97 })).toContain('<span class="text-danger font-medium">97°C</span>')
  })

  it('renders no Temp cell when the device reports none', () => {
    expect(card(metrics)).not.toContain('Temp')
  })
})
