import { describe, it, expect } from 'vitest'
import { wakePath } from './api'

describe('wakePath', () => {
  it('targets the per-device wake route, not /api/wake', () => {
    expect(wakePath('macos-desktop')).toBe('/api/devices/macos-desktop/wake')
  })

  it('escapes the device name', () => {
    expect(wakePath('a/b c')).toBe('/api/devices/a%2Fb%20c/wake')
  })
})
