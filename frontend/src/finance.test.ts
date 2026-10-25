import { describe, expect, it } from 'vite-plus/test'
import { money } from './finance'

describe('integer minor-unit display', () => {
  it('formats each supported currency with two decimal minor units', () => {
    expect(money(2500, 'USD')).toBe('$25.00')
    expect(money(105, 'EUR')).toBe('€1.05')
    expect(money(1, 'GBP')).toBe('£0.01')
    expect(money(0)).toBe('$0.00')
  })
  it('preserves cents at the maximum safe contract amount', () => {
    expect(money(Number.MAX_SAFE_INTEGER)).toBe('$90,071,992,547,409.91')
  })
  it('does not present unsafe or fractional values as valid money', () => {
    expect(money(Number.MAX_SAFE_INTEGER + 1)).toBe('—')
    expect(money(1.5)).toBe('—')
    expect(money(-1)).toBe('—')
  })
})
