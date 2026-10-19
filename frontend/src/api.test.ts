import { afterEach, beforeEach, describe, expect, it, vi } from 'vite-plus/test'
import { api, ApiError, request, schemas } from './api'
import { track } from './telemetry'

const payment = {
  id: 'payment_123',
  amount: 2500,
  currency: 'USD',
  provider: 'card',
  status: 'settled',
  scenario: 'normal',
  createdAt: '2026-10-19T12:00:00Z',
}
const input = {
  amount: 2500,
  currency: 'USD' as const,
  provider: 'card' as const,
  scenario: 'normal' as const,
  idempotencyKey: '14dff33d-19b5-4ba8-b380-802f3697c6cf',
}
beforeEach(() => vi.spyOn(console, 'warn').mockImplementation(() => undefined))
afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('generated contract validation', () => {
  it('resolves nested payment references in the payments response', () => {
    expect(
      schemas.payments.safeParse({ payments: [payment], balance: 10000, balances: { USD: 10000 } })
        .success,
    ).toBe(true)
    expect(
      schemas.payments.safeParse({
        payments: [{ ...payment, amount: '2500' }],
        balance: 10000,
        balances: { USD: 10000 },
      }).success,
    ).toBe(false)
  })
  it('rejects fractional amounts and unsupported options', () => {
    expect(schemas.paymentInput.safeParse(input).success).toBe(true)
    expect(schemas.paymentInput.safeParse({ ...input, amount: 1.5 }).success).toBe(false)
    expect(schemas.paymentInput.safeParse({ ...input, amount: -5 }).success).toBe(false)
    expect(schemas.paymentInput.safeParse({ ...input, currency: 'BTC' }).success).toBe(false)
    expect(schemas.paymentInput.safeParse({ ...input, provider: 'unknown' }).success).toBe(false)
    expect(
      schemas.paymentInput.safeParse({ ...input, amount: Number.MAX_SAFE_INTEGER + 1 }).success,
    ).toBe(false)
    expect(schemas.paymentInput.safeParse({ ...input, extra: 'unexpected' }).success).toBe(false)
  })
  it('validates payment status and timestamp formats', () => {
    expect(schemas.payment.safeParse({ ...payment, status: 'unknown' }).success).toBe(false)
    expect(schemas.payment.safeParse({ ...payment, createdAt: 'not-a-date' }).success).toBe(false)
  })
  it('accepts guest and read-only analyst sessions', () => {
    expect(schemas.session.safeParse({ role: 'guest' }).success).toBe(true)
    expect(schemas.session.safeParse({ role: 'analyst' }).success).toBe(true)
    expect(schemas.session.safeParse({ role: 'root' }).success).toBe(false)
  })
})

describe('API safety', () => {
  it('sends cookie credentials and an integer minor-unit payment', async () => {
    const fetcher = vi.fn().mockResolvedValue(new Response(JSON.stringify(payment)))
    vi.stubGlobal('fetch', fetcher)
    expect(await api.pay(input)).toEqual(payment)
    expect(fetcher).toHaveBeenCalledWith(
      '/api/payments',
      expect.objectContaining({
        method: 'POST',
        credentials: 'same-origin',
      }),
    )
    expect(JSON.parse(fetcher.mock.calls[0][1].body)).toEqual(input)
  })
  it('reports invalid server contracts without leaking response data', async () => {
    const fetcher = vi
      .fn()
      .mockImplementation((url: string) =>
        Promise.resolve(
          new Response(JSON.stringify(url === '/api/session' ? { role: 'sensitive-secret' } : {})),
        ),
      )
    vi.stubGlobal('fetch', fetcher)
    await expect(api.session()).rejects.toMatchObject({ kind: 'contract' })
    const event = fetcher.mock.calls.find(([url]) => url === '/api/events')
    expect(JSON.parse(event?.[1].body)).toMatchObject({
      action: 'api.request',
      outcome: 'failure',
      error: { kind: 'contract' },
    })
    expect(event?.[1].body).not.toContain('sensitive-secret')
  })
  it('rejects locally invalid input without posting a payment', async () => {
    const fetcher = vi.fn().mockResolvedValue(new Response('{}'))
    vi.stubGlobal('fetch', fetcher)
    await expect(api.pay({ ...input, amount: 0 })).rejects.toBeInstanceOf(ApiError)
    expect(fetcher.mock.calls.every(([url]) => url === '/api/events')).toBe(true)
  })
  it('reports authorization and network failures as safe errors', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(new Response('private details', { status: 403 })),
    )
    await expect(api.payments()).rejects.toMatchObject({ kind: 'authorization' })
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('private network data')))
    await expect(api.payments()).rejects.toMatchObject({ kind: 'network' })
  })
  it('supports no-content logout responses', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(null, { status: 204 })))
    await expect(request('logout', undefined, {})).resolves.toBeUndefined()
  })
  it('rejects no-content responses when a generated schema requires a body', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(null, { status: 204 })))
    await expect(api.session()).rejects.toMatchObject({ kind: 'contract' })
  })
  it('treats malformed JSON as a contract failure rather than exposing parser data', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('<private error page>')))
    await expect(api.session()).rejects.toMatchObject({ kind: 'contract' })
  })
})

describe('telemetry isolation', () => {
  it('serializes only allowlisted properties', async () => {
    const fetcher = vi.fn().mockResolvedValue(new Response(null, { status: 204 }))
    vi.stubGlobal('fetch', fetcher)
    const callerData = {
      action: 'payment.submit',
      outcome: 'success' as const,
      duration_ms: 12.3,
      amount: 2500,
      cardNumber: 'never-send-this',
      error: {
        kind: 'contract',
        message: 'private',
      },
    }
    await track(callerData)
    const payload = JSON.parse(fetcher.mock.calls[0][1].body)
    expect(payload).toEqual({
      action: 'payment.submit',
      outcome: 'success',
      client: { app_version: '1.0.0' },
      duration_ms: 12,
      error: { kind: 'contract' },
    })
  })
  it('keeps network failure nonfatal and warns without recursion', async () => {
    const fetcher = vi.fn().mockRejectedValue(new Error('offline'))
    vi.stubGlobal('fetch', fetcher)
    await expect(
      track({ action: 'navigation.overview', outcome: 'success' }),
    ).resolves.toBeUndefined()
    expect(fetcher).toHaveBeenCalledTimes(1)
    expect(console.warn).toHaveBeenCalledWith('[telemetry] delivery_network_failure')
  })
  it('warns with a fixed category on non-2xx responses without recursion', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValue(new Response('private rejection details', { status: 400 }))
    vi.stubGlobal('fetch', fetcher)
    await expect(
      track({ action: 'navigation.overview', outcome: 'success' }),
    ).resolves.toBeUndefined()
    expect(fetcher).toHaveBeenCalledTimes(1)
    expect(console.warn).toHaveBeenCalledExactlyOnceWith('[telemetry] delivery_http_failure')
  })
})
