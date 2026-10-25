import type { Currency } from './api'

export function money(amount: number, currency: Currency = 'USD'): string {
  if (!Number.isSafeInteger(amount) || amount < 0) return '—'
  const minor = BigInt(amount)
  // Formatting whole units as BigInt avoids losing cents near MAX_SAFE_INTEGER.
  const fraction = (minor % 100n).toString().padStart(2, '0')
  return new Intl.NumberFormat('en-US', { style: 'currency', currency })
    .formatToParts(minor / 100n)
    .map((part) => (part.type === 'fraction' ? fraction : part.value))
    .join('')
}
