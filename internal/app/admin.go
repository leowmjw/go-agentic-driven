package app

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"time"
)

var dashboard = template.Must(template.New("dashboard").Parse(`<section id="ledger-dashboard">
<h2>Live ledger</h2><p>{{len .Payments}} payments · separate settled balances in minor units:</p><ul>{{range $currency, $balance := .Balances}}<li>{{$currency}} {{$balance}}</li>{{end}}</ul>
<table><thead><tr><th>ID</th><th>Amount</th><th>Currency</th><th>Provider</th><th>Status</th><th>Scenario</th></tr></thead>
<tbody>{{range .Payments}}<tr><td>{{.ID}}</td><td>{{.Amount}}</td><td>{{.Currency}}</td><td>{{.Provider}}</td><td>{{.Status}}</td><td>{{.Scenario}}</td></tr>{{else}}<tr><td colspan="6">No payments yet</td></tr>{{end}}</tbody></table></section>`))

func (a *App) dashboard() string {
	payments, _, balances := a.store.snapshotAll("USD")
	var b bytes.Buffer
	_ = dashboard.Execute(&b, struct {
		Payments []Payment
		Balances map[string]int64
	}{payments, balances})
	return b.String()
}

func (a *App) admin(w http.ResponseWriter, r *http.Request) {
	if role(r) != "admin" {
		fail(w, 403, "forbidden", "admin demo role required")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Ledger administration</title><script type="module" src="/datastar.js"></script><style>body{font:16px system-ui;max-width:1100px;margin:3rem auto;padding:1rem;background:#101522;color:#edf1fa}a{color:#80bfff}table{width:100%%;border-collapse:collapse}th,td{text-align:left;padding:.6rem;border-bottom:1px solid #3a4050}td:first-child{max-width:12rem;overflow:hidden;text-overflow:ellipsis}</style></head><body data-init="@get('/admin/events')" data-on-load="@get('/admin/events')"><h1>Ledger operations</h1><p>Explicit demo role switch — not real authentication. <a href="/">Back to application</a></p>%s</body></html>`, a.dashboard())
}

func (a *App) adminEvents(w http.ResponseWriter, r *http.Request) {
	if role(r) != "admin" {
		fail(w, 403, "forbidden", "admin demo role required")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	for {
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		elements := singleLine(a.dashboard())
		// The npm beta uses merge-fragments/on-load; modern Datastar uses patch-elements/init.
		if _, err := fmt.Fprintf(w, "event: datastar-patch-elements\ndata: elements %s\n\nevent: datastar-merge-fragments\ndata: fragments %s\n\n", elements, elements); err != nil {
			return
		}
		if err := controller.Flush(); err != nil {
			return
		}
		if err := a.deps.Delay(r.Context(), time.Second); err != nil {
			return
		}
	}
}

func singleLine(s string) string {
	var b bytes.Buffer
	for _, c := range s {
		if c == '\n' || c == '\r' {
			b.WriteByte(' ')
		} else {
			b.WriteRune(c)
		}
	}
	return b.String()
}
