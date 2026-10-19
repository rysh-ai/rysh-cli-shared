// SPDX-License-Identifier: Apache-2.0

package secretnat

import (
	"encoding/json"
	"strings"
	"testing"
)

// Synthetic Google OAuth access tokens. These are NOT real tokens: they only
// carry the shape — the "ya29." prefix and a long base64url-ish tail. The
// second one has the dotted shape that metadata-server / service-account
// tokens use ("ya29.c.<…>"), which is why the detector's class admits '.'.
const (
	gcpUserToken = "ya29.a0SyntheticTestTokenONLY_not-a-real-credential_0123456789abcdefXYZ"
	gcpSAToken   = "ya29.c.SyntheticServiceAccount_TEST-only.0123456789abcdefghijKLMNOP_qrstuv-wxyz"
)

// (a) A bare token in prose — `gcloud auth print-access-token` output pasted
// into a message, or an HTTP trace — must be replaced, format-preserving,
// and must not swallow the sentence's trailing full stop.
func TestGCPOAuthBareTokenInProse(t *testing.T) {
	m := newTestManager(t, Options{Enabled: true})
	s := m.Session("pane-1")

	in := "gcloud printed " + gcpUserToken + ". Use it for the KMS call."
	out := s.Sanitize(in)
	if strings.Contains(out, gcpUserToken) {
		t.Fatalf("real ya29 token leaked: %q", out)
	}
	want := "gcloud printed ya29.SNAT000001. Use it for the KMS call."
	if out != want {
		t.Fatalf("Sanitize =\n  %q\nwant\n  %q", out, want)
	}
	if got := s.Restore(out); got != in {
		t.Fatalf("Restore = %q, want %q", got, in)
	}
}

// (a') The dotted service-account shape is one token, not several.
func TestGCPOAuthDottedTokenIsOneSpan(t *testing.T) {
	reg := mustRegistry(t)
	got := one(t, reg, "metadata server returned "+gcpSAToken, "gcp-oauth")
	if got != gcpSAToken {
		t.Fatalf("span = %q, want the whole dotted token %q", got, gcpSAToken)
	}
}

// (b) Inside an OAuth token-endpoint JSON response, via the JSON sanitize
// path (string values are walked one by one, so there is no key context).
func TestGCPOAuthTokenInJSON(t *testing.T) {
	m := newTestManager(t, Options{Enabled: true})
	s := m.Session("pane-1")

	in := json.RawMessage(`{"access_token":"` + gcpUserToken + `","expires_in":3599,"token_type":"Bearer"}`)
	out := s.SanitizeJSON(in)
	if strings.Contains(string(out), gcpUserToken) {
		t.Fatalf("JSON sanitize leaked the ya29 token: %s", out)
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("sanitized JSON invalid: %v", err)
	}
	if v["access_token"] != "ya29.SNAT000001" {
		t.Fatalf("access_token = %v, want ya29.SNAT000001", v["access_token"])
	}
	var back map[string]any
	if err := json.Unmarshal(s.RestoreJSON(out), &back); err != nil {
		t.Fatalf("restored JSON invalid: %v", err)
	}
	if back["access_token"] != gcpUserToken {
		t.Fatalf("restore mismatch: %v", back["access_token"])
	}
}

// (c) On an env line the key name also trips envkv (…TOKEN=). Both claim the
// identical span; gcp-oauth must win (higher confidence) so the value is
// tokenised exactly once and keeps its "ya29." shape.
func TestGCPOAuthEnvLineSingleToken(t *testing.T) {
	reg := mustRegistry(t)
	line := "GOOGLE_OAUTH_ACCESS_TOKEN=" + gcpUserToken
	if got := one(t, reg, line, "gcp-oauth"); got != gcpUserToken {
		t.Fatalf("span = %q, want %q", got, gcpUserToken)
	}

	m := newTestManager(t, Options{Enabled: true})
	s := m.Session("pane-1")
	out := s.Sanitize(line)
	if want := "GOOGLE_OAUTH_ACCESS_TOKEN=ya29.SNAT000001"; out != want {
		t.Fatalf("Sanitize = %q, want %q", out, want)
	}
	if n := strings.Count(out, "SNAT"); n != 1 {
		t.Fatalf("double tokenisation: %d SNAT markers in %q", n, out)
	}
}

// After "Bearer " the bearer detector also claims the value; gcp-oauth wins
// for the same reason, so the LLM still sees a Google token, not a generic one.
func TestGCPOAuthBeatsBearer(t *testing.T) {
	reg := mustRegistry(t)
	text := `curl -H "Authorization: Bearer ` + gcpUserToken + `" https://cloudkms.googleapis.com/v1/x`
	if got := one(t, reg, text, "gcp-oauth"); got != gcpUserToken {
		t.Fatalf("span = %q, want %q", got, gcpUserToken)
	}
}

// Negative: no dot after ya29, a tail under 20 chars, or ya29 glued to a
// preceding word is not a token. ("ya29.test" is a real fixture in rysh-cli's
// googlechat_test.go and must stay untouched.)
func TestGCPOAuthNegative(t *testing.T) {
	reg := mustRegistry(t)
	negatives := []string{
		"the ya29 prefix marks a Google access token",
		"ya29a0SyntheticTestTokenONLYnotarealcredential0123456789",
		"ya29.test",
		"ya29.short-tail_1234",
		"xya29.a0SyntheticTestTokenONLY_not-a-real-credential_0123",
	}
	for _, text := range negatives {
		if ms := reg.DetectAll(text); len(ms) != 0 {
			t.Errorf("DetectAll(%q) = %v, want none", text, ms)
		}
	}
}
