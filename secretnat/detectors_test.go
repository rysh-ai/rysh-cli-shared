// SPDX-License-Identifier: Apache-2.0

package secretnat

import (
	"strings"
	"testing"
)

func mustRegistry(t *testing.T) *Registry {
	t.Helper()
	reg, err := NewDefaultRegistry(nil, nil)
	if err != nil {
		t.Fatalf("NewDefaultRegistry: %v", err)
	}
	return reg
}

// one detects exactly one match of the wanted type and returns its value.
func one(t *testing.T, reg *Registry, text, wantType string) string {
	t.Helper()
	ms := reg.DetectAll(text)
	if len(ms) != 1 {
		t.Fatalf("DetectAll(%q) = %d matches (%v), want 1", text, len(ms), ms)
	}
	if ms[0].Type != wantType {
		t.Fatalf("DetectAll(%q) type = %s, want %s", text, ms[0].Type, wantType)
	}
	return text[ms[0].Start:ms[0].End]
}

func TestBuiltinDetectorsPositive(t *testing.T) {
	reg := mustRegistry(t)
	cases := []struct {
		name     string
		text     string
		wantType string
		wantVal  string
	}{
		{"anthropic", `key = "sk-ant-api03-AbCdEfGh12345678901234"`, "anthropic", "sk-ant-api03-AbCdEfGh12345678901234"},
		{"stripe-live", `stripe.api_key = "sk_live_4eC39HqLyjWDarjtT1zdp7dc"`, "stripe", "sk_live_4eC39HqLyjWDarjtT1zdp7dc"},
		{"stripe-test-pk", `pk_test_TYooMQauvdEDq54NiTphI7jx`, "stripe", "pk_test_TYooMQauvdEDq54NiTphI7jx"},
		{"github-classic", "token: ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789", "github", "ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"},
		{"github-pat", "github_pat_11ABCDEFG0abcdefghijklmnop", "github", "github_pat_11ABCDEFG0abcdefghijklmnop"},
		{"slack", "xoxb-123456789012-abcdefghijklmn", "slack", "xoxb-123456789012-abcdefghijklmn"},
		{"google", "AIzaSyA1234567890abcdefghijklmnopqrstuv", "google", "AIzaSyA1234567890abcdefghijklmnopqrstuv"},
		{"aws-id", "aws key AKIAIOSFODNN7EXAMPLE here", "aws", "AKIAIOSFODNN7EXAMPLE"},
		{"aws-secret", "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "aws-secret", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"},
		{"openai", `OpenAI(api_key="sk-abcdefghijklmnopqrst")`, "openai", "sk-abcdefghijklmnopqrst"},
		{"openai-proj", "sk-proj-abcdefghijk123456789", "openai", "sk-proj-abcdefghijk123456789"},
		{"jwt", "auth eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk", "jwt", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk"},
		{"dburl", "postgres://admin:hunter2pass@db.internal:5432/app", "dburl", "hunter2pass"},
		{"dburl-mongo", "mongodb+srv://svc:S3cretPW@cluster0.mongodb.net", "dburl", "S3cretPW"},
		{"bearer", `curl -H "Authorization: Bearer abc123def456ghi789jkl012"`, "bearer", "abc123def456ghi789jkl012"},
		{"envkv", "export DB_PASSWORD=supersecret99", "envkv", "supersecret99"},
		{"envkv-yaml", "api_key: myverysecretvalue", "envkv", "myverysecretvalue"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := one(t, reg, tc.text, tc.wantType)
			if got != tc.wantVal {
				t.Fatalf("span = %q, want %q", got, tc.wantVal)
			}
		})
	}
}

func TestPEMDetector(t *testing.T) {
	reg := mustRegistry(t)
	pem := "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA7\nanotherline==\n-----END RSA PRIVATE KEY-----"
	text := "here is a key:\n" + pem + "\ndone"
	got := one(t, reg, text, "pem")
	if got != pem {
		t.Fatalf("pem span = %q, want full block", got)
	}
}

func TestBuiltinDetectorsNegative(t *testing.T) {
	reg := mustRegistry(t)
	negatives := []string{
		"just some ordinary prose with no secrets at all",
		"HOST=db.internal.example.com",                       // env key not sensitive
		"PASSWORD=$DB_PASS",                                  // variable reference
		"TOKEN=${GITHUB_TOKEN}",                              // interpolation placeholder
		"sk_live_short",                                      // too short for stripe
		"the word task is not a secret, nor is skate",        // no prefixes
		"Bearer tokens should be rotated regularly",          // prose after Bearer
		"https://example.com/path",                           // url without credentials
		"eyJhbGciOiJIUzI1NiJ9",                               // single JWT segment
		"git commit 4eC39HqLyjWDarjtT1zdp7dcTYooMQauvdEDq54", // bare high-entropy string, no context
	}
	for _, text := range negatives {
		if ms := reg.DetectAll(text); len(ms) != 0 {
			t.Errorf("DetectAll(%q) = %v, want none", text, ms)
		}
	}
}

func TestDetectAllOverlapLeftmostLongest(t *testing.T) {
	reg := mustRegistry(t)
	// "sk-ant-…" must resolve to anthropic, never openai, even though both
	// could claim overlapping spans.
	text := "sk-ant-api03-AbCdEfGh12345678901234"
	ms := reg.DetectAll(text)
	if len(ms) != 1 || ms[0].Type != "anthropic" {
		t.Fatalf("DetectAll = %v, want single anthropic match", ms)
	}
}

func TestDetectAllMultipleSecrets(t *testing.T) {
	reg := mustRegistry(t)
	text := `stripe = "sk_live_4eC39HqLyjWDarjtT1zdp7dc"
gh = "ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
export API_TOKEN=abcdef1234567890`
	ms := reg.DetectAll(text)
	if len(ms) != 3 {
		t.Fatalf("DetectAll = %d matches (%v), want 3", len(ms), ms)
	}
	for i := 1; i < len(ms); i++ {
		if ms[i].Start < ms[i-1].End {
			t.Fatalf("matches overlap: %v", ms)
		}
	}
}

func TestDisabledAndCustomDetectors(t *testing.T) {
	reg, err := NewDefaultRegistry([]string{"stripe"}, []CustomDetector{
		{Name: "acme", Pattern: `\bacme_[A-Za-z0-9]{16}\b`, Prefix: "acme_"},
	})
	if err != nil {
		t.Fatalf("NewDefaultRegistry: %v", err)
	}
	if ms := reg.DetectAll("sk_live_4eC39HqLyjWDarjtT1zdp7dc"); len(ms) != 0 {
		t.Fatalf("disabled stripe still detects: %v", ms)
	}
	got := one(t, reg, "key acme_ABCDEF0123456789 end", "acme")
	if got != "acme_ABCDEF0123456789" {
		t.Fatalf("custom span = %q", got)
	}
	if _, err := NewDefaultRegistry(nil, []CustomDetector{{Name: "bad", Pattern: "("}}); err == nil {
		t.Fatal("invalid custom pattern must fail construction")
	}
}

func TestValidateSkipsTokensAndPlaceholders(t *testing.T) {
	reg := mustRegistry(t)
	// Synthetic-looking values must never be (re-)detected: idempotence.
	for _, text := range []string{
		"sk_live_SNAT000001",
		"key = eyJSNAT000001.SNATPAYLOAD.SNATSIGNATURE",
		"PASSWORD=SECRET_TOKEN_001",
	} {
		for _, m := range reg.DetectAll(text) {
			v := text[m.Start:m.End]
			if strings.Contains(v, "SNAT") || strings.Contains(v, "SECRET_TOKEN_") {
				t.Errorf("detector %s re-matched synthetic %q", m.Type, v)
			}
		}
	}
}
