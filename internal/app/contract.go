package app

import "encoding/json"

var Currencies = []string{"USD", "EUR", "GBP"}
var Providers = []string{"bank", "card"}
var Scenarios = []string{"normal", "decline", "slow", "risk", "stale_quote"}
var Roles = []string{"customer", "analyst", "admin"}

const MaxSafeInteger int64 = 9007199254740991

// Contract is shared by the HTTP endpoint and the build-time contract command.
func Contract() []byte {
	str := func(values []string) map[string]any {
		return map[string]any{"type": "string", "enum": values}
	}
	obj := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	}
	amount := map[string]any{"type": "integer", "minimum": 1, "maximum": MaxSafeInteger}
	payment := obj(map[string]any{
		"id": map[string]any{"type": "string"}, "amount": amount,
		"currency": str(Currencies), "provider": str(Providers), "scenario": str(Scenarios),
		"status":    str([]string{"settled", "declined", "review", "expired"}),
		"createdAt": map[string]any{"type": "string", "format": "date-time"},
	}, "id", "amount", "currency", "provider", "status", "scenario", "createdAt")
	input := obj(map[string]any{
		"amount": amount, "currency": str(Currencies), "provider": str(Providers), "scenario": str(Scenarios),
		"idempotencyKey": map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
	}, "amount", "currency", "provider", "scenario", "idempotencyKey")
	data, _ := json.MarshalIndent(map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$id":     "https://ledger.local/contract.json",
		"$defs": map[string]any{
			"payment": payment, "paymentInput": input,
			"session": obj(map[string]any{"role": str(append([]string{"guest"}, Roles...))}, "role"),
			"payments": obj(map[string]any{
				"payments": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/payment"}},
				"balance": map[string]any{
					"type": "integer", "minimum": 0, "maximum": MaxSafeInteger,
					"description": "Legacy USD-only settled balance in minor units. Other currencies are reported in balances.",
				},
				"balances": map[string]any{
					"type":                 "object",
					"description":          "Separate settled minor-unit balances keyed by currency; currencies are never combined.",
					"additionalProperties": map[string]any{"type": "integer", "minimum": 0, "maximum": MaxSafeInteger},
				},
			}, "payments", "balance", "balances"),
		},
	}, "", "  ")
	return data
}
