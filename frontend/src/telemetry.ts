export type Event = {
  action: string
  outcome: 'success' | 'failure' | 'started'
  error?: { kind: string }
  session?: { step: string }
  user?: { cohort: string }
  client?: { app_version: string }
  duration_ms?: number
}

// Construct a new allowlisted object; never serialize callers' arbitrary properties.
export async function track(event: Event): Promise<void> {
  const payload: Event = {
    action: event.action,
    outcome: event.outcome,
    client: { app_version: '1.0.0' },
    ...(event.error && { error: { kind: event.error.kind } }),
    ...(event.session && { session: { step: event.session.step } }),
    ...(event.user && { user: { cohort: event.user.cohort } }),
    ...(event.duration_ms !== undefined && { duration_ms: Math.round(event.duration_ms) }),
  }
  try {
    const response = await fetch('/api/events', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
      keepalive: true,
    })
    if (!response.ok) console.warn('[telemetry] delivery_http_failure')
  } catch {
    // Telemetry must never prevent a payment or recursively report its own failure.
    console.warn('[telemetry] delivery_network_failure')
  }
}
