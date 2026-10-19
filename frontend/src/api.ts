import { z } from 'zod'
import contract from './contract.json'
import { track } from './telemetry'

export type Role = 'guest' | 'customer' | 'analyst' | 'admin'
export type Currency = 'USD' | 'EUR' | 'GBP'
export type Scenario = 'normal' | 'decline' | 'slow' | 'risk' | 'stale_quote'
export type Payment = {
  id: string
  amount: number
  currency: Currency
  provider: 'card' | 'bank'
  status: string
  scenario: Scenario
  createdAt: string
}
export type PaymentInput = {
  amount: number
  currency: Currency
  provider: 'card' | 'bank'
  scenario: Scenario
  idempotencyKey: string
}
export type Payments = {
  payments: Payment[]
  balance: number
  balances: Record<string, number>
}

// Include the complete document's definitions so nested local $refs resolve.
function schema(name: keyof typeof contract.$defs) {
  // JSON imports widen schema keywords to string; this assertion is only schema metadata.
  const jsonSchema = {
    ...contract.$defs[name],
    $defs: contract.$defs,
  } as z.core.JSONSchema.JSONSchema
  return z.fromJSONSchema(jsonSchema)
}
export const schemas = {
  session: schema('session'),
  payments: schema('payments'),
  payment: schema('payment'),
  paymentInput: schema('paymentInput'),
}

export class ApiError extends Error {
  kind: string
  constructor(message: string, kind: string) {
    super(message)
    this.kind = kind
  }
}

export async function request(path: string, output?: z.ZodType, body?: unknown): Promise<unknown> {
  const start = performance.now()
  try {
    const response = await fetch(`/api/${path}`, {
      method: body === undefined ? 'GET' : 'POST',
      credentials: 'same-origin',
      headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    })
    if (!response.ok) {
      // Backend text can include arbitrary data; display and report only safe local messages.
      throw new ApiError(
        response.status === 401 || response.status === 403
          ? 'Your role does not have access. Please sign in again.'
          : 'The request could not be completed. Please try again.',
        response.status === 401 || response.status === 403 ? 'authorization' : 'http',
      )
    }
    if (response.status === 204) {
      if (output) {
        throw new ApiError('The server returned an empty response. Please refresh.', 'contract')
      }
      return undefined
    }
    let data: unknown
    try {
      data = await response.json()
    } catch {
      throw new ApiError('The server returned an unreadable response. Please refresh.', 'contract')
    }
    if (!output) return data
    const result = output.safeParse(data)
    if (!result.success) {
      throw new ApiError('The server returned an unexpected response. Please refresh.', 'contract')
    }
    return result.data
  } catch (error) {
    const safe =
      error instanceof ApiError
        ? error
        : new ApiError('Unable to connect. Check your connection and try again.', 'network')
    void track({
      action: 'api.request',
      outcome: 'failure',
      error: { kind: safe.kind },
      duration_ms: performance.now() - start,
    })
    throw safe
  }
}

// These assertions describe the generated contract's TS projection, not inferred Zod types.
export const api = {
  session: () => request('session', schemas.session) as Promise<{ role: Role }>,
  payments: () => request('payments', schemas.payments) as Promise<Payments>,
  login: (role: Role) => request('login', schemas.session, { role }),
  logout: () => request('logout', schemas.session, {}),
  seed: (scenario: string) => request('seed', schemas.payments, { scenario }),
  pay: async (input: PaymentInput): Promise<Payment> => {
    const parsed = schemas.paymentInput.safeParse(input)
    if (!parsed.success) {
      void track({ action: 'payment.validate', outcome: 'failure', error: { kind: 'contract' } })
      throw new ApiError('Check the payment amount and selected options.', 'contract')
    }
    return request('payments', schemas.payment, parsed.data) as Promise<Payment>
  },
}
