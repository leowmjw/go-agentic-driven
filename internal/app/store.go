package app

import (
	"context"
	"errors"
	"sync"
	"time"
)

type Payment struct {
	ID        string    `json:"id"`
	Amount    int64     `json:"amount"`
	Currency  string    `json:"currency"`
	Provider  string    `json:"provider"`
	Status    string    `json:"status"`
	Scenario  string    `json:"scenario"`
	CreatedAt time.Time `json:"createdAt"`
}

type PaymentInput struct {
	Amount         int64  `json:"amount"`
	Currency       string `json:"currency"`
	Provider       string `json:"provider"`
	Scenario       string `json:"scenario"`
	IdempotencyKey string `json:"idempotencyKey"`
}

type entry struct {
	input   PaymentInput
	payment Payment
}

type store struct {
	mu       sync.Mutex
	payments []Payment
	keys     map[string]entry
	balances map[string]int64
}

// snapshot reports a single currency's settled balance, USD unless requested.
func (s *store) snapshot(currency ...string) ([]Payment, int64) {
	selected := "USD"
	if len(currency) != 0 {
		selected = currency[0]
	}
	payments, balance, _ := s.snapshotAll(selected)
	return payments, balance
}

func (s *store) snapshotAll(currency string) ([]Payment, int64, map[string]int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	balances := make(map[string]int64, len(Currencies))
	for _, code := range Currencies {
		balances[code] = s.balances[code]
	}
	return append([]Payment{}, s.payments...), s.balances[currency], balances
}

var errConflict = errors.New("idempotency key already used for a different payment")
var errBalance = errors.New("ledger safe integer limit reached")

func (s *store) create(in PaymentInput, now time.Time, id string) (Payment, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.keys[in.IdempotencyKey]; ok {
		if existing.input != in {
			return Payment{}, false, errConflict
		}
		return existing.payment, true, nil
	}
	status := "settled"
	switch in.Scenario {
	case "decline":
		status = "declined"
	case "risk":
		status = "review"
	case "stale_quote":
		status = "expired"
	}
	if status == "settled" && in.Amount > MaxSafeInteger-s.balances[in.Currency] {
		return Payment{}, false, errBalance
	}
	p := Payment{ID: id, Amount: in.Amount, Currency: in.Currency, Provider: in.Provider, Scenario: in.Scenario, Status: status, CreatedAt: now.UTC()}
	s.payments = append(s.payments, p)
	s.keys[in.IdempotencyKey] = entry{in, p}
	if status == "settled" {
		if s.balances == nil {
			s.balances = make(map[string]int64)
		}
		s.balances[in.Currency] += in.Amount
	}
	return p, false, nil
}

func delay(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
