// SPDX-License-Identifier: Apache-2.0

package msg

import (
	"encoding/json"
	"testing"
	"time"
)

// root-order-044 #8: the org-wide USD spend cap rides on the lease contract.

func TestSpendCapPeriodBounds(t *testing.T) {
	cases := []struct {
		name       string
		period     string
		now        time.Time
		start, end time.Time
	}{
		{"month mid", SpendCapPeriodMonth,
			time.Date(2026, 10, 19, 13, 5, 0, 0, time.UTC),
			time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
		{"month last second", SpendCapPeriodMonth,
			time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC),
			time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"month first second", SpendCapPeriodMonth,
			time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)},
		{"day", SpendCapPeriodDay,
			time.Date(2026, 10, 19, 23, 59, 59, 0, time.UTC),
			time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)},
		// A non-UTC clock must still give UTC bounds: 01:30 on Nov 1 in UTC+3 is
		// still October in UTC.
		{"month non-UTC input", SpendCapPeriodMonth,
			time.Date(2026, 11, 1, 1, 30, 0, 0, time.FixedZone("UTC+3", 3*3600)),
			time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start, end, ok := SpendCapPeriodBounds(c.period, c.now)
			if !ok {
				t.Fatalf("period %q refused", c.period)
			}
			if !start.Equal(c.start) || !end.Equal(c.end) {
				t.Fatalf("got [%v, %v), want [%v, %v)", start, end, c.start, c.end)
			}
			if start.Location() != time.UTC || end.Location() != time.UTC {
				t.Fatalf("bounds not in UTC: %v %v", start.Location(), end.Location())
			}
		})
	}
}

func TestSpendCapPeriodBoundsRefusesUnknown(t *testing.T) {
	for _, p := range []string{"", "week", "Month", "year", " month"} {
		if _, _, ok := SpendCapPeriodBounds(p, time.Now()); ok {
			t.Errorf("period %q accepted, want refused", p)
		}
		if ValidSpendCapPeriod(p) {
			t.Errorf("ValidSpendCapPeriod(%q) = true", p)
		}
	}
	for _, p := range []string{SpendCapPeriodDay, SpendCapPeriodMonth} {
		if !ValidSpendCapPeriod(p) {
			t.Errorf("ValidSpendCapPeriod(%q) = false", p)
		}
	}
}

func TestLeaseReplySpendCapRoundTrip(t *testing.T) {
	in := GatewayLeaseReply{
		GrantedTokens:       0,
		Refusal:             GatewayRefusalSpendCap,
		SpendCapMicroUSD:    50_000_000,
		SpentPeriodMicroUSD: 50_000_001,
		SpendCapPeriod:      SpendCapPeriodMonth,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out GatewayLeaseReply
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Refusal != in.Refusal || out.SpendCapMicroUSD != in.SpendCapMicroUSD ||
		out.SpentPeriodMicroUSD != in.SpentPeriodMicroUSD || out.SpendCapPeriod != in.SpendCapPeriod {
		t.Fatalf("round trip lost fields: %+v", out)
	}
}

// An old server's reply has none of the new fields: it must decode to "no cap,
// no reason", which the client reads as today's behaviour.
func TestLeaseReplyOldShapeDecodes(t *testing.T) {
	old := `{"granted_tokens":0,"expires_at":"2026-10-19T00:00:00Z","remaining_today":0,"ceiling_tokens":100,"active_daemons":1,"max_lease_tokens":10}`
	var out GatewayLeaseReply
	if err := json.Unmarshal([]byte(old), &out); err != nil {
		t.Fatal(err)
	}
	if out.Refusal != "" || out.SpendCapMicroUSD != 0 || out.SpentPeriodMicroUSD != 0 || out.SpendCapPeriod != "" {
		t.Fatalf("old reply decoded new fields: %+v", out)
	}
}

// A reply with no cap must not grow new keys on the wire, so an old client's
// view of a no-cap server is byte-for-byte what it was.
func TestLeaseReplyNoCapOmitsNewKeys(t *testing.T) {
	b, err := json.Marshal(GatewayLeaseReply{GrantedTokens: 5})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"refusal", "spend_cap_micro_usd", "spent_period_micro_usd", "spend_cap_period"} {
		if _, ok := m[k]; ok {
			t.Errorf("key %q present on a no-cap reply", k)
		}
	}
}

func TestLeaseRequestSpentSinceMicroUSD(t *testing.T) {
	b, err := json.Marshal(GatewayLeaseRequest{DaemonID: "d", SpentSinceMicroUSD: 7})
	if err != nil {
		t.Fatal(err)
	}
	var out GatewayLeaseRequest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.SpentSinceMicroUSD != 7 {
		t.Fatalf("SpentSinceMicroUSD = %d", out.SpentSinceMicroUSD)
	}
	b, _ = json.Marshal(GatewayLeaseRequest{DaemonID: "d"})
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if _, ok := m["spent_since_micro_usd"]; ok {
		t.Error("spent_since_micro_usd present when zero")
	}
}

func TestSpendCapDocRoundTrip(t *testing.T) {
	in := GatewaySpendCap{Period: SpendCapPeriodDay, CapMicroUSD: 1_000_000, SpentMicroUSD: 3}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out GatewaySpendCap
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("got %+v want %+v", out, in)
	}
}
