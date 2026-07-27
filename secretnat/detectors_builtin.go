package secretnat

import (
	"fmt"
	"regexp"
	"strings"
)

// builtinDetectors returns the built-in detector set in fixed order.
// Ordering is most-specific-first: it is the deterministic tiebreak when two
// detectors claim the same span (leftmost-longest resolution happens first).
//
// All patterns are RE2 (linear time). Each detector's span is the secret
// VALUE only — contextual detectors (aws-secret, dburl, bearer, envkv)
// capture the value in a group so keys/usernames stay untouched.
func builtinDetectors() []Detector {
	return []Detector{
		// Anthropic API keys. Must precede openai ("sk-ant-…" vs "sk-…").
		&regexDetector{
			name:        "anthropic",
			re:          regexp.MustCompile(`\b(sk-ant-)[A-Za-z0-9_\-]{20,}`),
			prefixGroup: 1,
			prefix:      "sk-ant-",
			confidence:  0.97,
		},
		// Stripe secret/publishable/restricted keys, live and test.
		&regexDetector{
			name:        "stripe",
			re:          regexp.MustCompile(`\b((?:sk|pk|rk)_(?:live|test)_)[A-Za-z0-9]{16,}\b`),
			prefixGroup: 1,
			prefix:      "sk_live_",
			confidence:  0.97,
		},
		// GitHub classic + fine-grained tokens.
		&regexDetector{
			name:        "github",
			re:          regexp.MustCompile(`\b(gh[opusr]_)[A-Za-z0-9]{36,}\b`),
			prefixGroup: 1,
			prefix:      "ghp_",
			confidence:  0.97,
		},
		&regexDetector{
			name:        "github",
			re:          regexp.MustCompile(`\b(github_pat_)[A-Za-z0-9_]{22,}\b`),
			prefixGroup: 1,
			prefix:      "github_pat_",
			confidence:  0.97,
		},
		// Slack bot/user/app/refresh tokens.
		&regexDetector{
			name:        "slack",
			re:          regexp.MustCompile(`\b(xox[baprs]-)[A-Za-z0-9-]{10,}\b`),
			prefixGroup: 1,
			prefix:      "xoxb-",
			confidence:  0.95,
		},
		// Google API keys.
		&regexDetector{
			name:        "google",
			re:          regexp.MustCompile(`\b(AIza)[0-9A-Za-z_\-]{35}\b`),
			prefixGroup: 1,
			prefix:      "AIza",
			confidence:  0.95,
		},
		// AWS access key IDs.
		&regexDetector{
			name:        "aws",
			re:          regexp.MustCompile(`\b(AKIA)[0-9A-Z]{16}\b`),
			prefixGroup: 1,
			prefix:      "AKIA",
			confidence:  0.95,
		},
		// AWS secret access keys assigned to the conventional variable name.
		&regexDetector{
			name:       "aws-secret",
			re:         regexp.MustCompile(`(?i)aws_secret_access_key["']?[ \t]*[:=][ \t]*["']?([A-Za-z0-9/+=]{30,})`),
			group:      1,
			confidence: 0.9,
		},
		// PEM-encoded private key blocks (RSA / EC / OPENSSH / PKCS#8 …).
		// The whole block is the secret; the synthetic replacement preserves
		// the BEGIN/END framing so the LLM still recognises a private key.
		&regexDetector{
			name:       "pem",
			re:         regexp.MustCompile(`(?s)-----BEGIN (?:[A-Z]+ )*PRIVATE KEY-----.*?-----END (?:[A-Z]+ )*PRIVATE KEY-----`),
			confidence: 0.99,
			synthetic: func(seq int) string {
				return fmt.Sprintf("-----BEGIN PRIVATE KEY-----\nSNAT%06d\n-----END PRIVATE KEY-----", seq)
			},
		},
		// OpenAI keys — after anthropic so "sk-ant-…" never lands here.
		&regexDetector{
			name:        "openai",
			re:          regexp.MustCompile(`\b(sk-(?:proj-)?)[A-Za-z0-9]{20,}\b`),
			prefixGroup: 1,
			prefix:      "sk-",
			confidence:  0.85,
		},
		// JWTs: three base64url segments. The synthetic keeps the
		// three-segment shape so downstream parsing logic stays plausible.
		&regexDetector{
			name:       "jwt",
			re:         regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\b`),
			prefix:     "eyJ",
			confidence: 0.9,
			synthetic: func(seq int) string {
				return fmt.Sprintf("eyJSNAT%06d.SNATPAYLOAD.SNATSIGNATURE", seq)
			},
		},
		// Database connection URLs — only the password component.
		&regexDetector{
			name:       "dburl",
			re:         regexp.MustCompile(`(?i)\b(?:postgres(?:ql)?|mysql|mongodb(?:\+srv)?|rediss?|amqp)://[^:/\s@]+:([^@\s]{4,})@`),
			group:      1,
			confidence: 0.85,
		},
		// Bearer tokens in headers / curl invocations.
		&regexDetector{
			name:       "bearer",
			re:         regexp.MustCompile(`\bBearer[ \t]+([A-Za-z0-9\-._~+/]{20,}=*)`),
			group:      1,
			confidence: 0.6,
		},
		// Env-style assignments whose KEY name signals a secret
		// (KEY=value, KEY: value, export KEY=value; case-insensitive).
		// The keyword list mirrors the pre-existing one-way redactors
		// (rysh-cli file_browse.go redactLine / env_read.go isSensitiveEnv).
		&regexDetector{
			name:       "envkv",
			re:         regexp.MustCompile(`(?im)^[ \t]*(?:export[ \t]+)?[A-Z0-9_]*(?:SECRET|TOKEN|PASSWORD|PASSWD|API_?KEY|ACCESS_KEY|PRIVATE_KEY|CREDENTIALS?)[A-Z0-9_]*[ \t]*[=:][ \t]*["']?([^"'\s;,]{8,})`),
			group:      1,
			confidence: 0.7,
			validate: func(v string) bool {
				// Skip variable references and interpolations — they are
				// placeholders, not secret material.
				return !strings.HasPrefix(v, "$") && !strings.HasPrefix(v, "%")
			},
		},
	}
}
