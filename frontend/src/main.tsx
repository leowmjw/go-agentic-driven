import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './index.css'
import App from './App.tsx'
import { track } from './telemetry'

window.addEventListener('error', () => {
  void track({ action: 'client.error', outcome: 'failure', error: { kind: 'runtime' } })
})
window.addEventListener('unhandledrejection', () => {
  void track({ action: 'client.error', outcome: 'failure', error: { kind: 'unhandled_rejection' } })
})

createRoot(document.getElementById('root')!, {
  onUncaughtError: () => {
    void track({ action: 'client.error', outcome: 'failure', error: { kind: 'render' } })
  },
  onRecoverableError: () => {
    void track({
      action: 'client.error',
      outcome: 'failure',
      error: { kind: 'recoverable_render' },
    })
  },
}).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
