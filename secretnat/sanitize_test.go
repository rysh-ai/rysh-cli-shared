package secretnat

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const (
	stripeKey = "sk_live_4eC39HqLyjWDarjtT1zdp7dc"
	githubKey = "ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
)

func newTestManager(t *testing.T, opts Options) *Manager {
	t.Helper()
	m, err := NewManager(opts)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func TestSanitizeDetectedTier(t *testing.T) {
	m := newTestManager(t, Options{Enabled: true})
	s := m.Session("pane-1")

	in := `stripe.api_key = "` + stripeKey + `"`
	out := s.Sanitize(in)
	if strings.Contains(out, stripeKey) {
		t.Fatalf("real key leaked: %q", out)
	}
	if !strings.Contains(out, "sk_live_SNAT000001") {
		t.Fatalf("expected semantic token, got %q", out)
	}

	// Determinism: same value → same token.
	out2 := s.Sanitize("again: " + stripeKey)
	if !strings.Contains(out2, "sk_live_SNAT000001") {
		t.Fatalf("token not deterministic: %q", out2)
	}

	// Idempotence: sanitizing sanitized text is a no-op.
	if got := s.Sanitize(out); got != out {
		t.Fatalf("Sanitize not idempotent:\n  once: %q\n twice: %q", out, got)
	}

	// Restore round-trips.
	restored := s.Restore(out)
	if restored != in {
		t.Fatalf("Restore = %q, want %q", restored, in)
	}
}

func TestSanitizeKnownTier(t *testing.T) {
	m := newTestManager(t, Options{Enabled: true})
	m.UpdateKnownSecrets([]KnownSecret{{Name: "STRIPE_KEY", Value: stripeKey}})
	s := m.Session("pane-1")

	out := s.Sanitize("use " + stripeKey + " here")
	if out != "use ${STRIPE_KEY} here" {
		t.Fatalf("known tier: got %q", out)
	}
	if got := s.Restore(out); got != "use "+stripeKey+" here" {
		t.Fatalf("known restore: got %q", got)
	}
	// A ${NAME} the model invents is NOT restored unless registered.
	if got := s.Restore("path is ${HOME}/bin"); got != "path is ${HOME}/bin" {
		t.Fatalf("unregistered ref must stay: %q", got)
	}
}

func TestKnownTierLongestValueFirst(t *testing.T) {
	long := "prefix-secret-and-more"
	short := "prefix-secret"
	ks := NewKnownSet([]KnownSecret{{Name: "SHORT", Value: short}, {Name: "LONG", Value: long}})
	out, n := Sanitize("x "+long+" y "+short, ks, nil, nil, nil)
	if out != "x ${LONG} y ${SHORT}" || n != 2 {
		t.Fatalf("got %q (n=%d)", out, n)
	}
}

func TestPrivateMode(t *testing.T) {
	m := newTestManager(t, Options{Enabled: true, Mode: ModePrivate})
	s := m.Session("pane-1")
	out := s.Sanitize(stripeKey + " and " + githubKey)
	if strings.Contains(out, "sk_live") || strings.Contains(out, "ghp_") {
		t.Fatalf("private mode leaked type: %q", out)
	}
	if !strings.Contains(out, "SECRET_TOKEN_001") || !strings.Contains(out, "SECRET_TOKEN_002") {
		t.Fatalf("want SECRET_TOKEN_ tokens: %q", out)
	}
	if got := s.Restore(out); got != stripeKey+" and "+githubKey {
		t.Fatalf("private restore: %q", got)
	}
}

func TestSessionIsolation(t *testing.T) {
	m := newTestManager(t, Options{Enabled: true})
	a, b := m.Session("pane-a"), m.Session("pane-b")
	tokA := strings.TrimSpace(a.Sanitize(stripeKey))
	// b never saw the value: restoring a's token through b is a no-op.
	if got := b.Restore(tokA); got != tokA {
		t.Fatalf("mapping leaked across sessions: %q → %q", tokA, got)
	}
	if got := a.Restore(tokA); got != stripeKey {
		t.Fatalf("own session restore failed: %q", got)
	}
}

func TestDisabled(t *testing.T) {
	m := newTestManager(t, Options{Enabled: false})
	s := m.Session("pane-1")
	if got := s.Sanitize(stripeKey); got != stripeKey {
		t.Fatalf("disabled manager must be identity, got %q", got)
	}
	// Per-session override turns it on.
	on := true
	s.SetOverride(&on)
	if got := s.Sanitize(stripeKey); got == stripeKey {
		t.Fatal("override on: expected sanitization")
	}
	// Clearing the override falls back to the (disabled) default.
	s.SetOverride(nil)
	if got := s.Sanitize("x " + githubKey); got != "x "+githubKey {
		t.Fatalf("cleared override must follow default, got %q", got)
	}
	// And the inverse: enabled default, per-session off.
	m2 := newTestManager(t, Options{Enabled: true})
	s2 := m2.Session("p")
	off := false
	s2.SetOverride(&off)
	if got := s2.Sanitize(stripeKey); got != stripeKey {
		t.Fatalf("override off must be identity, got %q", got)
	}
}

func TestNilSessionSafety(t *testing.T) {
	var s *Session
	if s.Enabled() {
		t.Fatal("nil session must be disabled")
	}
	if got := s.Sanitize(stripeKey); got != stripeKey {
		t.Fatal("nil Sanitize must be identity")
	}
	if got := s.Restore("x"); got != "x" {
		t.Fatal("nil Restore must be identity")
	}
	if out := s.NewStreamRestorer().Feed("abc"); out != "abc" {
		t.Fatalf("nil stream restorer must pass through, got %q", out)
	}
	_ = s.Stats()
	_ = s.RestoreJSON(json.RawMessage(`{"a":1}`))
	_ = s.SanitizeJSON(json.RawMessage(`{"a":1}`))
}

func TestJSONTransform(t *testing.T) {
	m := newTestManager(t, Options{Enabled: true})
	s := m.Session("pane-1")

	in := json.RawMessage(`{"cmd":"deploy","env":{"STRIPE":"` + stripeKey + `"},"args":["--key","` + stripeKey + `"],"count":42,"big":9007199254740993}`)
	out := s.SanitizeJSON(in)
	if strings.Contains(string(out), stripeKey) {
		t.Fatalf("JSON sanitize leaked: %s", out)
	}
	if !strings.Contains(string(out), "9007199254740993") {
		t.Fatalf("large integer mangled: %s", out)
	}
	back := s.RestoreJSON(out)
	var v struct {
		Env  map[string]string `json:"env"`
		Args []string          `json:"args"`
	}
	if err := json.Unmarshal(back, &v); err != nil {
		t.Fatalf("restored JSON invalid: %v", err)
	}
	if v.Env["STRIPE"] != stripeKey || v.Args[1] != stripeKey {
		t.Fatalf("restore mismatch: %+v", v)
	}

	// Values with quotes/newlines survive the round trip with correct escaping.
	m.UpdateKnownSecrets([]KnownSecret{{Name: "WEIRD", Value: `pa"ss\nwo rd99`}})
	in2, _ := json.Marshal(map[string]string{"p": `pa"ss\nwo rd99`})
	out2 := s.SanitizeJSON(in2)
	if strings.Contains(string(out2), "rd99") {
		t.Fatalf("weird value leaked: %s", out2)
	}
	var v2 map[string]string
	if err := json.Unmarshal(s.RestoreJSON(out2), &v2); err != nil {
		t.Fatalf("restored weird JSON invalid: %v", err)
	}
	if v2["p"] != `pa"ss\nwo rd99` {
		t.Fatalf("weird round trip: %q", v2["p"])
	}

	// Malformed JSON passes through unchanged (never corrupted).
	bad := json.RawMessage(`{"unterminated`)
	if got := s.SanitizeJSON(bad); string(got) != string(bad) {
		t.Fatalf("malformed JSON must pass through: %s", got)
	}
}

func TestMappingTableNeverSerializes(t *testing.T) {
	tab := NewMappingTable()
	tab.TokenFor("value", "test", func(seq int) string { return "TOK" })
	if _, err := json.Marshal(tab); err == nil {
		t.Fatal("MappingTable marshaled without error — persistence guard broken")
	}
	type carrier struct {
		Table *MappingTable `json:"table"`
	}
	if _, err := json.Marshal(carrier{Table: tab}); err == nil {
		t.Fatal("embedded MappingTable marshaled — persistence guard broken")
	}
}

func TestStatsAreValueFree(t *testing.T) {
	m := newTestManager(t, Options{Enabled: true})
	m.UpdateKnownSecrets([]KnownSecret{{Name: "K", Value: "knownvalue99"}})
	s := m.Session("pane-1")
	s.Sanitize(stripeKey + " knownvalue99")
	st := s.Stats()
	if st.Detected != 2 {
		t.Fatalf("Detected = %d, want 2", st.Detected)
	}
	for _, e := range st.Entries {
		if strings.Contains(e.Token, stripeKey) {
			t.Fatal("entry leaked a real value")
		}
	}
	ms := m.Stats()
	if ms.Sessions != 1 || ms.KnownSecrets != 1 || ms.Detected != 2 {
		t.Fatalf("manager stats: %+v", ms)
	}
}

func TestSweepExpired(t *testing.T) {
	m := newTestManager(t, Options{Enabled: true, MappingTTL: time.Nanosecond})
	s := m.Session("pane-1")
	s.Sanitize(stripeKey)
	time.Sleep(2 * time.Millisecond)
	if n := m.SweepExpired(); n != 1 {
		t.Fatalf("SweepExpired = %d, want 1", n)
	}
	// No-TTL manager never sweeps.
	m2 := newTestManager(t, Options{Enabled: true})
	m2.Session("pane-1").Sanitize(stripeKey)
	if n := m2.SweepExpired(); n != 0 {
		t.Fatalf("SweepExpired with TTL=0 = %d, want 0", n)
	}
}

func TestSanitizeLargePromptPerf(t *testing.T) {
	m := newTestManager(t, Options{Enabled: true})
	s := m.Session("pane-1")
	var b strings.Builder
	for b.Len() < 1<<20 { // ~1 MB
		b.WriteString("some ordinary log line with nothing sensitive in it 1234567890\n")
	}
	b.WriteString("export DB_PASSWORD=hunter2secret99\n")
	// The bound guards against pathological (e.g. quadratic) slowdown, not
	// absolute speed: ~0.5s is typical, but the race detector alone costs
	// 10-20x, so a fixed 5s budget flaked under -race. Scale it rather than
	// weaken the plain-build bound.
	budget := 5 * time.Second
	if raceEnabled {
		budget = 60 * time.Second
	}
	start := time.Now()
	out := s.Sanitize(b.String())
	if d := time.Since(start); d > budget {
		t.Fatalf("1MB sanitize took %v (budget %v)", d, budget)
	}
	if strings.Contains(out, "hunter2secret99") {
		t.Fatal("secret survived large-prompt sanitize")
	}
}
