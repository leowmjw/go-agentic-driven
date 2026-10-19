import { useCallback, useEffect, useRef, useState } from 'react'
import type { FormEvent, ReactNode } from 'react'
import {
  Link,
  Outlet,
  RouterProvider,
  createRootRoute,
  createRoute,
  createRouter,
  useRouterState,
} from '@tanstack/react-router'
import { api, ApiError } from './api'
import type { Currency, Payment, PaymentInput, Payments, Role, Scenario } from './api'
import { track } from './telemetry'
import { money } from './finance'

function Icon({ name, size = 20 }: { name: string; size?: number }) {
  const paths: Record<string, ReactNode> = {
    grid: (
      <>
        <rect x="3" y="3" width="7" height="7" rx="1.5" />
        <rect x="14" y="3" width="7" height="7" rx="1.5" />
        <rect x="3" y="14" width="7" height="7" rx="1.5" />
        <rect x="14" y="14" width="7" height="7" rx="1.5" />
      </>
    ),
    arrow: (
      <>
        <path d="M5 12h14M13 6l6 6-6 6" />
      </>
    ),
    activity: <path d="M3 12h4l3-8 4 16 3-8h4" />,
    wallet: (
      <>
        <rect x="3" y="5" width="18" height="15" rx="3" />
        <path d="M3 9h18M16 14h5" />
      </>
    ),
    shield: (
      <>
        <path d="M12 3 4 6v6c0 4 8 9 8 9s8-5 8-9V6z" />
        <path d="m9 12 2 2 4-4" />
      </>
    ),
    plus: <path d="M12 5v14M5 12h14" />,
    card: (
      <>
        <rect x="2" y="5" width="20" height="14" rx="3" />
        <path d="M2 10h20M6 15h3" />
      </>
    ),
    bank: (
      <>
        <path d="m3 8 9-5 9 5H3ZM5 10v8M10 10v8M15 10v8M20 10v8M3 21h18" />
      </>
    ),
    check: <path d="m5 12 4 4L19 6" />,
    close: <path d="m6 6 12 12M6 18 18 6" />,
    refresh: (
      <>
        <path d="M20 7v5h-5M4 17v-5h5" />
        <path d="M6 7a7 7 0 0 1 12-2l2 3M4 16l2 3a7 7 0 0 0 12-2" />
      </>
    ),
    logout: (
      <>
        <path d="M9 4H4v16h5M14 8l4 4-4 4M8 12h12" />
      </>
    ),
    search: (
      <>
        <circle cx="10.5" cy="10.5" r="6.5" />
        <path d="m16 16 4 4" />
      </>
    ),
  }
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.7"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      {paths[name] ?? paths.grid}
    </svg>
  )
}

const scenarioLabels: Record<Scenario, string> = {
  normal: 'Successful payment',
  decline: 'Card declined',
  slow: 'Slow provider',
  risk: 'Risk review',
  stale_quote: 'Expired quote',
}
const goodStatus = (status: string) =>
  ['succeeded', 'completed', 'paid', 'settled', 'success'].includes(status)

function Application() {
  const path = useRouterState({ select: (state) => state.location.pathname })
  const [role, setRole] = useState<Role>('guest')
  const [sessionReady, setSessionReady] = useState(false)
  const [data, setData] = useState<Payments>({ payments: [], balance: 0, balances: {} })
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [loginOpen, setLoginOpen] = useState(false)
  const [paymentOpen, setPaymentOpen] = useState(false)
  const [selectedRole, setSelectedRole] = useState<Role>('customer')
  const [busy, setBusy] = useState(false)
  const [query, setQuery] = useState('')
  const [filter, setFilter] = useState('all')
  const [seed, setSeed] = useState('fresh')
  const loadVersion = useRef(0)
  const canPay = role === 'customer' || role === 'admin'

  const loadPayments = useCallback(async () => {
    const version = ++loadVersion.current
    setLoading(true)
    try {
      const payments = await api.payments()
      if (loadVersion.current === version) setData(payments)
    } finally {
      if (loadVersion.current === version) setLoading(false)
    }
  }, [])

  useEffect(() => {
    let active = true
    api
      .session()
      .then(async (session) => {
        if (!active) return
        setRole(session.role)
        setSessionReady(true)
        if (session.role !== 'guest') await loadPayments()
        else setLoading(false)
      })
      .catch((err: unknown) => {
        if (active) {
          setError(message(err))
          setSessionReady(true)
          setLoading(false)
        }
      })
    return () => {
      active = false
    }
  }, [loadPayments])

  useEffect(() => {
    void track({
      action: 'route.change',
      outcome: 'success',
      session: { step: path === '/activity' ? 'activity' : 'overview' },
    })
  }, [path])

  const act = (action: string) => {
    void track({
      action,
      outcome: 'started',
      user: { cohort: role },
      session: { step: 'dashboard' },
    })
    setError('')
    setNotice('')
  }
  async function signIn(event: FormEvent) {
    event.preventDefault()
    act('login.submit')
    setBusy(true)
    try {
      await api.login(selectedRole)
      setRole(selectedRole)
      setLoginOpen(false)
      await loadPayments()
      setNotice(`You're signed in as ${selectedRole}.`)
      void track({ action: 'login.submit', outcome: 'success', user: { cohort: selectedRole } })
    } catch (err) {
      setError(message(err))
    } finally {
      setBusy(false)
    }
  }
  async function logout() {
    act('logout.click')
    setBusy(true)
    try {
      await api.logout()
      loadVersion.current++
      setRole('guest')
      setData({ payments: [], balance: 0, balances: {} })
      setLoading(false)
      setNotice('You have been signed out.')
      void track({ action: 'logout', outcome: 'success' })
    } catch (err) {
      setError(message(err))
    } finally {
      setBusy(false)
    }
  }
  async function refresh() {
    act('payments.refresh')
    try {
      await loadPayments()
      setNotice('Your payment activity is up to date.')
    } catch (err) {
      setError(message(err))
    }
  }
  async function seedData() {
    act('demo.seed')
    setBusy(true)
    try {
      await api.seed(seed)
      await loadPayments()
      setNotice('Demo environment updated.')
    } catch (err) {
      setError(message(err))
    } finally {
      setBusy(false)
    }
  }
  const complete = data.payments.filter((payment) => goodStatus(payment.status))
  const pending = data.payments.filter((payment) =>
    ['pending', 'processing', 'review', 'risk_review', 'requires_review'].includes(payment.status),
  )
  const rate = data.payments.length ? Math.round((complete.length / data.payments.length) * 100) : 0
  const visible = data.payments
    .filter(
      (payment) =>
        (filter === 'all' ||
          (filter === 'successful' ? goodStatus(payment.status) : !goodStatus(payment.status))) &&
        `${payment.id} ${payment.currency} ${payment.provider} ${payment.status}`
          .toLowerCase()
          .includes(query.toLowerCase()),
    )
    .sort((left, right) => Date.parse(right.createdAt) - Date.parse(left.createdAt))

  return (
    <div className="app">
      <aside className="sidebar">
        <Link to="/" className="brand" onClick={() => act('navigation.brand')}>
          <span className="brand-mark">
            n<span>↗</span>
          </span>{' '}
          northstar<span className="brand-dot">.</span>
        </Link>
        <div className="workspace">
          <span className="workspace-icon">N</span>
          <div>
            Northstar workspace<small>Sandbox environment</small>
          </div>
          <span className="chevron">⌄</span>
        </div>
        <div className="nav-label">WORKSPACE</div>
        <nav aria-label="Main navigation">
          <Link
            to="/"
            className={path === '/' ? 'nav-item active' : 'nav-item'}
            onClick={() => act('navigation.overview')}
          >
            <Icon name="grid" />
            Overview
          </Link>
          <Link
            to="/activity"
            className={path === '/activity' ? 'nav-item active' : 'nav-item'}
            onClick={() => act('navigation.activity')}
          >
            <Icon name="activity" />
            Payment activity<span className="nav-count">{data.payments.length}</span>
          </Link>
          <a href="/admin" className="nav-item" onClick={() => act('navigation.admin')}>
            <Icon name="shield" />
            Admin console<small>↗</small>
          </a>
        </nav>
        <div className="sidebar-bottom">
          <div className="sandbox-card">
            <span className="sandbox-spark">✳</span>
            <strong>A safe space to explore</strong>
            <p>
              Test payments. Try edge cases.
              <br />
              No real money moves here.
            </p>
            <span className="sandbox-tag">
              <span /> Sandbox mode
            </span>
          </div>
          <div className="profile">
            <span className="avatar">
              {role === 'guest' ? 'G' : role.slice(0, 1).toUpperCase()}
            </span>
            <div>
              <strong>
                {role === 'guest'
                  ? 'Guest account'
                  : `${role[0].toUpperCase()}${role.slice(1)} account`}
              </strong>
              <small>{role === 'analyst' ? 'Read-only access' : 'Demo workspace'}</small>
            </div>
            {role !== 'guest' && (
              <button
                className="icon-button"
                disabled={busy}
                onClick={() => void logout()}
                aria-label="Sign out"
              >
                <Icon name="logout" size={18} />
              </button>
            )}
          </div>
        </div>
      </aside>

      <div className="main-wrap">
        <header className="topbar">
          <div className="breadcrumb">
            Workspace <span>/</span>{' '}
            <strong>{path === '/activity' ? 'Payment activity' : 'Overview'}</strong>
          </div>
          <div className="topbar-right">
            <span className="environment">
              <span />
              Sandbox
            </span>
            <span className="divider" />
            <span className="today">
              {new Date().toLocaleDateString('en-US', {
                month: 'short',
                day: 'numeric',
                year: 'numeric',
              })}
            </span>
            <span className="avatar small">N</span>
          </div>
        </header>
        <main>
          <section className="page-heading">
            <div>
              <div className="eyebrow">YOUR MONEY, IN MOTION</div>
              <h1>
                {path === '/activity' ? 'Payment activity' : 'Overview'}
                <span className="heading-dot">.</span>
              </h1>
              <p>A clear view of every payment. A confident next move.</p>
            </div>
            <button
              className="primary"
              disabled={!sessionReady || busy || role === 'analyst'}
              title={role === 'analyst' ? 'Analysts have read-only access' : undefined}
              onClick={() => {
                act('payment.open')
                if (canPay) setPaymentOpen(true)
                else if (role === 'guest') setLoginOpen(true)
                else
                  setNotice(
                    'Analysts have read-only access. Sign in as a customer to create payments.',
                  )
              }}
            >
              <Icon name="plus" size={18} />
              New payment
            </button>
          </section>
          {error && (
            <div className="banner error" role="alert">
              <Icon name="shield" />
              <span>{error}</span>
              <button
                className="icon-button"
                aria-label="Dismiss error"
                onClick={() => {
                  act('error.dismiss')
                  setError('')
                }}
              >
                <Icon name="close" size={16} />
              </button>
            </div>
          )}
          {notice && (
            <div className="banner notice" role="status">
              <Icon name="check" />
              <span>{notice}</span>
              <button
                className="icon-button"
                aria-label="Dismiss notification"
                onClick={() => {
                  act('notice.dismiss')
                  setNotice('')
                }}
              >
                <Icon name="close" size={16} />
              </button>
            </div>
          )}
          {role === 'guest' && sessionReady && (
            <div className="welcome-banner">
              <div className="welcome-symbol">
                <Icon name="wallet" size={24} />
              </div>
              <div>
                <strong>Your payment playground is ready.</strong>
                <p>Choose a demo role to explore payments, insights, and real-world scenarios.</p>
              </div>
              <button
                className="text-button"
                onClick={() => {
                  act('login.open')
                  setLoginOpen(true)
                }}
              >
                Enter workspace <Icon name="arrow" size={18} />
              </button>
            </div>
          )}
          {role === 'analyst' && (
            <div className="banner readonly">
              <Icon name="shield" />
              Analyst workspace · You can explore and monitor payments. Creating payments is
              disabled.
            </div>
          )}
          <section className="metrics" aria-label="Payment summary">
            <Metric
              label="Settled balance"
              value={money(data.balance)}
              icon="wallet"
              note="Sandbox balance · USD"
              featured
            />
            <Metric
              label="Total payments"
              value={String(data.payments.length).padStart(2, '0')}
              icon="activity"
              note="All payment attempts"
            />
            <Metric
              label="Success rate"
              value={`${rate}%`}
              icon="check"
              note={
                data.payments.length
                  ? `${complete.length} successful payments`
                  : 'Ready for your first payment'
              }
            />
            <Metric
              label="In progress"
              value={String(pending.length).padStart(2, '0')}
              icon="refresh"
              note="Processing & under review"
            />
          </section>
          <section className="currency-balances" aria-label="Separate settled balances by currency">
            <span>
              Every currency, accounted for.<small>Balances are never combined.</small>
            </span>
            <div>
              {(['USD', 'EUR', 'GBP'] as const).map((currency) => (
                <span className="currency-total" key={currency}>
                  <small>{currency}</small>
                  <strong>
                    {money(
                      data.balances[currency] ?? (currency === 'USD' ? data.balance : 0),
                      currency,
                    )}
                  </strong>
                </span>
              ))}
            </div>
          </section>
          <div className="content-grid">
            <section className="panel activity-panel">
              <div className="panel-heading">
                <div>
                  <h2>{path === '/activity' ? 'All payments' : 'Recent payments'}</h2>
                  <p>Your latest transactions, at a glance.</p>
                </div>
                <button
                  className="icon-button refresh-button"
                  onClick={() => void refresh()}
                  disabled={loading || role === 'guest'}
                  aria-label="Refresh payments"
                >
                  <Icon name="refresh" size={18} />
                </button>
              </div>
              <div className="table-toolbar">
                <div className="tabs" aria-label="Payment filter">
                  {[
                    ['all', 'All payments'],
                    ['successful', 'Successful'],
                    ['attention', 'Other'],
                  ].map(([value, label]) => (
                    <button
                      key={value}
                      className={filter === value ? 'tab selected' : 'tab'}
                      onClick={() => {
                        act(`payments.filter.${value}`)
                        setFilter(value)
                      }}
                    >
                      {label}
                    </button>
                  ))}
                </div>
                <label className="search">
                  <Icon name="search" size={16} />
                  <input
                    aria-label="Search payments"
                    placeholder="Search payments…"
                    value={query}
                    onChange={(event) => {
                      setQuery(event.target.value)
                    }}
                    onBlur={() => {
                      if (query) act('payments.search')
                    }}
                  />
                </label>
              </div>
              <div className="table-scroll">
                <table>
                  <thead>
                    <tr>
                      <th>TRANSACTION</th>
                      <th>AMOUNT</th>
                      <th>STATUS</th>
                      <th>DATE</th>
                      <th aria-label="Details" />
                    </tr>
                  </thead>
                  <tbody>
                    {loading ? (
                      <tr>
                        <td colSpan={5}>
                          <div className="empty-state">
                            <span className="spinner" />
                            <strong>Loading your payments…</strong>
                          </div>
                        </td>
                      </tr>
                    ) : visible.length === 0 ? (
                      <tr>
                        <td colSpan={5}>
                          <div className="empty-state">
                            <span className="empty-icon">
                              <Icon name="activity" size={28} />
                            </span>
                            <strong>
                              {role === 'guest'
                                ? 'Great things start with one payment'
                                : query || filter !== 'all'
                                  ? 'No matching payments'
                                  : 'Your first payment starts here'}
                            </strong>
                            <p>
                              {role === 'guest'
                                ? 'Enter the workspace to bring your dashboard to life.'
                                : query || filter !== 'all'
                                  ? 'Try a different search or filter.'
                                  : 'Create a sandbox payment and watch its journey.'}
                            </p>
                            {canPay && (
                              <button
                                className="text-button"
                                onClick={() => {
                                  act('payment.empty.open')
                                  setPaymentOpen(true)
                                }}
                              >
                                Create a payment <Icon name="arrow" size={16} />
                              </button>
                            )}
                          </div>
                        </td>
                      </tr>
                    ) : (
                      visible.map((payment) => (
                        <PaymentRow
                          key={payment.id}
                          payment={payment}
                          onSelect={() => {
                            act('payment.details')
                            setNotice(
                              `${scenarioLabels[payment.scenario]} · ${payment.status} · ${payment.provider === 'bank' ? 'Bank transfer' : 'Card payment'}`,
                            )
                          }}
                        />
                      ))
                    )}
                  </tbody>
                </table>
              </div>
              <div className="table-footer">
                <span>
                  {visible.length} of {data.payments.length} payments
                </span>
                <span>
                  <span className="live-dot" /> Live sandbox data
                </span>
              </div>
            </section>
            <aside className="insights">
              <section className="panel quick-panel">
                <div className="section-icon">
                  <Icon name="shield" size={23} />
                </div>
                <span className="mini-label">BUILT FOR CONFIDENCE</span>
                <h2>
                  Real scenarios.
                  <br />
                  Zero real-world risk.
                </h2>
                <p>See how your payments behave before they meet the real world.</p>
                <div className="scenario-preview">
                  <span className="scenario-dot green" /> Successful payments{' '}
                  <Icon name="check" size={14} />
                </div>
                <div className="scenario-preview">
                  <span className="scenario-dot orange" /> Provider delays <span>↗</span>
                </div>
                <div className="scenario-preview">
                  <span className="scenario-dot purple" /> Risk & decline testing <span>↗</span>
                </div>
                <div className="quick-footer">
                  <Icon name="shield" size={15} /> Every transaction is simulated
                </div>
              </section>
              <section className="help-card">
                <div className="help-title">
                  <span>✦</span>
                  <strong>A little more clarity.</strong>
                </div>
                <p>Amounts are stored in minor units. A payment of 1,000 equals $10.00 USD.</p>
                <span className="help-foot">Precise by design.</span>
              </section>
            </aside>
          </div>
          {role === 'admin' && (
            <section className="panel admin-controls">
              <div>
                <h2>Demo controls</h2>
                <p>Reset this sandbox with a fresh, busy, or incident scenario.</p>
              </div>
              <select
                aria-label="Demo environment"
                value={seed}
                onChange={(event) => {
                  act('demo.scenario')
                  setSeed(event.target.value)
                }}
              >
                <option value="fresh">Fresh start</option>
                <option value="busy">Busy workspace</option>
                <option value="incident">Provider incident</option>
              </select>
              <button className="secondary" disabled={busy} onClick={() => void seedData()}>
                Load scenario
              </button>
            </section>
          )}
          <footer className="footer">
            <span className="footer-brand">
              northstar<span>↗</span>
            </span>
            <span>Thoughtfully simple. Dependably precise.</span>
            <span>
              <span className="live-dot" /> Sandbox environment
            </span>
          </footer>
        </main>
      </div>
      {loginOpen && (
        <Modal
          title="Make yourself at home."
          subtitle="One workspace. Three perspectives."
          onClose={() => {
            act('login.cancel')
            setLoginOpen(false)
          }}
          busy={busy}
        >
          <form onSubmit={(event) => void signIn(event)}>
            <div className="role-options">
              {(['customer', 'analyst', 'admin'] as const).map((item) => (
                <label
                  className={selectedRole === item ? 'role-option chosen' : 'role-option'}
                  key={item}
                >
                  <input
                    type="radio"
                    name="role"
                    value={item}
                    checked={selectedRole === item}
                    onChange={() => {
                      act('login.role')
                      setSelectedRole(item)
                    }}
                  />
                  <Icon
                    name={
                      item === 'customer' ? 'wallet' : item === 'analyst' ? 'activity' : 'shield'
                    }
                  />
                  <div>
                    <strong>{item[0].toUpperCase() + item.slice(1)}</strong>
                    <p>
                      {item === 'customer'
                        ? 'Create payments and explore scenarios'
                        : item === 'analyst'
                          ? 'Monitor payment activity · read only'
                          : 'Manage payments and seed demo data'}
                    </p>
                  </div>
                </label>
              ))}
            </div>
            <p className="form-hint">
              Demo access only. Your role is saved in a secure session cookie.
            </p>
            {error && (
              <div className="form-error" role="alert">
                {error}
              </div>
            )}
            <button className="primary full" disabled={busy}>
              {busy ? 'Entering workspace…' : 'Enter workspace'}
              <Icon name="arrow" size={18} />
            </button>
          </form>
        </Modal>
      )}
      {paymentOpen && (
        <PaymentModal
          onClose={() => {
            act('payment.cancel')
            setPaymentOpen(false)
          }}
          onSubmit={async (input) => {
            act('payment.submit')
            const started = performance.now()
            const payment = await api.pay(input)
            void track({
              action: 'payment.submit',
              outcome: 'success',
              duration_ms: performance.now() - started,
              user: { cohort: role },
            })
            setPaymentOpen(false)
            setNotice(
              `Payment ${goodStatus(payment.status) ? 'successful' : payment.status.replaceAll('_', ' ')}. ${scenarioLabels[payment.scenario]}.`,
            )
            await loadPayments().catch((err: unknown) => setError(message(err)))
          }}
        />
      )}
      <Outlet />
    </div>
  )
}

function message(error: unknown) {
  return error instanceof ApiError ? error.message : 'Something went wrong. Please try again.'
}
function Metric({
  label,
  value,
  icon,
  note,
  featured = false,
}: {
  label: string
  value: string
  icon: string
  note: string
  featured?: boolean
}) {
  return (
    <article className={`metric ${featured ? 'featured' : ''}`}>
      <div className="metric-top">
        <span>{label}</span>
        <span className="metric-icon">
          <Icon name={icon} size={18} />
        </span>
      </div>
      <strong>{value}</strong>
      <div className="metric-note">
        {featured && <span className="balance-dot" />}
        {note}
      </div>
    </article>
  )
}
function PaymentRow({ payment, onSelect }: { payment: Payment; onSelect: () => void }) {
  const good = goodStatus(payment.status)
  const bad = ['failed', 'declined', 'rejected', 'expired'].includes(payment.status)
  const date = new Date(payment.createdAt)
  return (
    <tr>
      <td>
        <div className="transaction">
          <span className={`transaction-icon ${payment.provider}`}>
            <Icon name={payment.provider} size={18} />
          </span>
          <div>
            <strong>{payment.provider === 'card' ? 'Card payment' : 'Bank transfer'}</strong>
            <small>{payment.id.slice(0, 16)}</small>
          </div>
        </div>
      </td>
      <td>
        <strong className="amount">{money(payment.amount, payment.currency)}</strong>
        <small className="currency">{payment.currency}</small>
      </td>
      <td>
        <span className={`status ${good ? 'success' : bad ? 'failed' : 'pending'}`}>
          <span />
          {payment.status.replaceAll('_', ' ')}
        </span>
      </td>
      <td className="date-cell">
        {Number.isNaN(date.valueOf())
          ? '—'
          : date.toLocaleDateString('en-US', { month: 'short', day: 'numeric' })}
        <small>
          {Number.isNaN(date.valueOf())
            ? ''
            : date.toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit' })}
        </small>
      </td>
      <td>
        <button className="icon-button" onClick={onSelect} aria-label="View payment scenario">
          <Icon name="arrow" size={17} />
        </button>
      </td>
    </tr>
  )
}
function Modal({
  title,
  subtitle,
  children,
  onClose,
  busy = false,
}: {
  title: string
  subtitle: string
  children: ReactNode
  onClose: () => void
  busy?: boolean
}) {
  const ref = useRef<HTMLDialogElement>(null)
  useEffect(() => {
    ref.current?.showModal()
  }, [])
  return (
    <dialog
      ref={ref}
      className="modal"
      onCancel={(event) => {
        event.preventDefault()
        if (!busy) onClose()
      }}
    >
      <div className="modal-heading">
        <span className="modal-mark">n↗</span>
        <button className="icon-button" aria-label="Close dialog" disabled={busy} onClick={onClose}>
          <Icon name="close" />
        </button>
      </div>
      <h2>{title}</h2>
      <p className="modal-subtitle">{subtitle}</p>
      {children}
    </dialog>
  )
}
function PaymentModal({
  onClose,
  onSubmit,
}: {
  onClose: () => void
  onSubmit: (input: PaymentInput) => Promise<void>
}) {
  const [amount, setAmount] = useState('2500')
  const [currency, setCurrency] = useState<Currency>('USD')
  const [provider, setProvider] = useState<'card' | 'bank'>('card')
  const [scenario, setScenario] = useState<Scenario>('normal')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [key, setKey] = useState(() => crypto.randomUUID())
  function edited() {
    setKey(crypto.randomUUID())
    setError('')
    void track({ action: 'payment.edit', outcome: 'success', session: { step: 'payment.form' } })
  }
  async function submit(event: FormEvent) {
    event.preventDefault()
    if (!/^[1-9]\d*$/.test(amount) || !Number.isSafeInteger(Number(amount))) {
      setError('Enter a positive whole number in minor units.')
      void track({ action: 'payment.validate', outcome: 'failure', error: { kind: 'contract' } })
      return
    }
    setBusy(true)
    setError('')
    try {
      // Retain this key on an uncertain response; a retry cannot duplicate the payment.
      await onSubmit({ amount: Number(amount), currency, provider, scenario, idempotencyKey: key })
    } catch (err) {
      setError(message(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal
      title="A new payment."
      subtitle="Move money in a world without risk."
      onClose={onClose}
      busy={busy}
    >
      <form
        onSubmit={(event) => void submit(event)}
        onInvalid={() => {
          void track({
            action: 'payment.validate',
            outcome: 'failure',
            error: { kind: 'contract' },
          })
        }}
      >
        <fieldset disabled={busy}>
          <div className="form-row">
            <label className="form-field">
              Amount in minor units
              <input
                autoFocus
                inputMode="numeric"
                value={amount}
                onChange={(event) => {
                  edited()
                  setAmount(event.target.value)
                }}
                required
                aria-describedby="amount-help"
              />
            </label>
            <label className="form-field currency-select">
              Currency
              <select
                value={currency}
                onChange={(event) => {
                  edited()
                  setCurrency(event.target.value as Currency)
                }}
              >
                <option>USD</option>
                <option>EUR</option>
                <option>GBP</option>
              </select>
            </label>
          </div>
          <p className="amount-preview" id="amount-help">
            {Number.isSafeInteger(Number(amount))
              ? money(Number(amount), currency)
              : 'Enter a whole number'}{' '}
            <span>· 100 minor units = 1 {currency}</span>
          </p>
          <label className="form-field">
            Payment provider
            <select
              value={provider}
              onChange={(event) => {
                edited()
                setProvider(event.target.value as 'card' | 'bank')
              }}
            >
              <option value="card">Card payment</option>
              <option value="bank">Bank transfer</option>
            </select>
          </label>
          <label className="form-field">
            Simulation scenario
            <select
              value={scenario}
              onChange={(event) => {
                edited()
                setScenario(event.target.value as Scenario)
              }}
            >
              {Object.entries(scenarioLabels).map(([value, label]) => (
                <option key={value} value={value}>
                  {label}
                </option>
              ))}
            </select>
          </label>
          <div className="simulation-note">
            <Icon name="shield" size={17} />
            <span>No card details needed. No real funds transferred.</span>
          </div>
        </fieldset>
        {error && (
          <div className="form-error" role="alert">
            {error}
          </div>
        )}
        <button className="primary full" disabled={busy}>
          {busy ? (
            <>
              <span className="spinner" />
              Processing payment…
            </>
          ) : (
            <>
              Simulate payment
              <Icon name="arrow" size={18} />
            </>
          )}
        </button>
        <p className="form-hint centered">
          Protected against duplicate payments with an idempotency key.
        </p>
      </form>
    </Modal>
  )
}

const rootRoute = createRootRoute({ component: Application })
const overviewRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/',
  component: () => null,
})
const activityRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/activity',
  component: () => null,
})
const router = createRouter({ routeTree: rootRoute.addChildren([overviewRoute, activityRoute]) })
declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}
export default function App() {
  return <RouterProvider router={router} />
}
