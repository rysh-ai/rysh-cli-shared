package agentic

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
)

// Phase 3 item N — fine-grained tool permissions.
//
// The existing approval flow is binary: a tool either always requires approval
// (configured per-tool via RequiresApproval) or it doesn't. The user can hand
// out "approve always" decisions that cache an auto-approval key. That's
// coarse: there's no way to say "auto-allow file_read everywhere", or
// "deny file_write outside the workspace", or "ask for bash unless it's a
// read-only command".
//
// PermissionPolicy plugs into OrchestratorActor.executeTool BEFORE the
// existing approval check and produces one of three decisions:
//
//	PermAllow  → skip the approval prompt entirely.
//	PermDeny   → refuse the tool call with a structured error so the model
//	             can adapt without surprising the user.
//	PermAsk    → fall through to the existing approval flow (default).
//
// Rules match by tool name (exact, case-sensitive) and an optional glob over
// the "file_path" / "command" / "url" / "pattern" field in the tool's
// params. The first matching rule wins; an empty Policy yields PermAsk for
// every call.
//
// The policy is intentionally simple: no boolean expressions, no rule
// composition, no per-user grants. Add complexity only when a concrete
// scenario demands it.

// PermDecision is one of the three policy outcomes.
type PermDecision int

const (
	PermAsk   PermDecision = iota // default: fall through to the existing approval flow
	PermAllow                     // bypass the approval prompt, run the tool
	PermDeny                      // refuse the tool call with a structured error
)

// PermissionRule is a single match-and-decide entry. Tool must match exactly;
// Match is a doublestar-friendly glob over the tool's primary path / command
// argument (interpreted via PathMatch below). An empty Match means "any
// argument" so a tool-only rule applies broadly.
type PermissionRule struct {
	Tool     string       // tool name (exact); "" matches any tool
	Match    string       // glob over the primary path/command; "" matches any
	Decision PermDecision // allow / deny / ask
}

// PermissionPolicy is an ordered list of rules. First match wins; if no
// rule matches, the default decision is PermAsk.
type PermissionPolicy struct {
	Rules []PermissionRule
}

// NewPermissionPolicy constructs a policy from a slice of rules. The caller's
// slice is COPIED so later mutations of the source don't surprise the
// orchestrator.
func NewPermissionPolicy(rules []PermissionRule) *PermissionPolicy {
	cp := make([]PermissionRule, len(rules))
	copy(cp, rules)
	return &PermissionPolicy{Rules: cp}
}

// Decide returns the decision for a tool call. params is the raw JSON the
// model emitted; the policy extracts a primary string field (see
// permissionPath) to glob against. Empty / nil policy returns PermAsk.
func (p *PermissionPolicy) Decide(toolName string, params json.RawMessage) PermDecision {
	if p == nil || len(p.Rules) == 0 {
		return PermAsk
	}
	target := permissionPath(toolName, params)
	for _, r := range p.Rules {
		if r.Tool != "" && r.Tool != toolName {
			continue
		}
		if r.Match == "" {
			return r.Decision
		}
		if matchGlob(r.Match, target) {
			return r.Decision
		}
	}
	return PermAsk
}

// permissionPath returns the string the policy globs against for a given
// tool. Choice of field is by tool name:
//
//	bash, bash_background → "command"
//	web_fetch             → "url"
//	web_search, grep      → "pattern"
//	default               → "file_path"
//
// Empty when the field is absent or malformed.
func permissionPath(toolName string, params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	var probe struct {
		FilePath string `json:"file_path"`
		Command  string `json:"command"`
		URL      string `json:"url"`
		Pattern  string `json:"pattern"`
	}
	if err := json.Unmarshal(params, &probe); err != nil {
		return ""
	}
	switch toolName {
	case "bash", "bash_background":
		return probe.Command
	case "web_fetch":
		return probe.URL
	case "web_search", "grep":
		return probe.Pattern
	}
	return probe.FilePath
}

// matchGlob is a tiny glob implementation: supports * (any sequence within a
// path component) and ** (any sequence including separators). Plain prefixes
// without wildcards behave like substring matches anchored on the boundary
// so the common "allow bash for `ls`" rule can use `ls *` literally.
//
// We intentionally don't pull in a heavyweight glob library — the rule
// surface is small and predictable. doublestar semantics are limited to the
// patterns most users write.
func matchGlob(pattern, value string) bool {
	if pattern == "" {
		return true
	}
	if pattern == value {
		return true
	}
	// ** anywhere → split-and-match like path.Match but allow / through *.
	if strings.Contains(pattern, "**") {
		// Convert to a regex-like check using strings.Split.
		return matchDoublestar(pattern, value)
	}
	// path.Match respects / boundaries; the doc says ? matches single rune
	// and * matches any sequence within a path component.
	matched, err := filepath.Match(pattern, value)
	if err == nil && matched {
		return true
	}
	// Convenience: treat "prefix *" as a starts-with check on whitespace
	// boundaries so `bash` rules like `git status *` work intuitively.
	if strings.HasSuffix(pattern, " *") {
		prefix := strings.TrimSuffix(pattern, " *")
		if value == prefix || strings.HasPrefix(value, prefix+" ") {
			return true
		}
	}
	if strings.HasSuffix(pattern, "*") && !strings.ContainsAny(pattern, "?[") {
		prefix := strings.TrimSuffix(pattern, "*")
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

// matchDoublestar handles patterns containing "**". Implementation: convert
// the glob to an anchored regex and run it. The conversion is small and
// covers the cases users actually write:
//
//	**       → .*          (any sequence, including path separators)
//	*        → [^/]*       (any sequence WITHIN a path component)
//	?        → [^/]        (single non-separator)
//	**/      → optional sequence ending in /, OR empty
//	          → handled as: "(?:.*/)?" so `**/.env*` matches `.env` AND `a/b/.env`
//	other    → regex-quoted
func matchDoublestar(pattern, value string) bool {
	re, err := globToRegexp(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(value)
}

// globToRegexp compiles a glob pattern (with **, *, ?) into an anchored Go
// regexp. Special-cases `**/` as `(?:.*/)?` so patterns like `**/.env*`
// match both bare names (`.env`) and nested paths (`a/b/.env`).
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	i := 0
	for i < len(pattern) {
		switch {
		case strings.HasPrefix(pattern[i:], "**/"):
			// "Any number of path components, or none." This makes
			// `**/.env*` match `.env` directly.
			b.WriteString("(?:.*/)?")
			i += 3
		case strings.HasPrefix(pattern[i:], "**"):
			// Bare ** matches any chars (incl. /).
			b.WriteString(".*")
			i += 2
		case pattern[i] == '*':
			// Single * matches within a path component (no /).
			b.WriteString("[^/]*")
			i++
		case pattern[i] == '?':
			b.WriteString("[^/]")
			i++
		default:
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
			i++
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// _ = filepath.Match keeps the dependency referenced; filepath remains in use
// from matchGlob's non-doublestar branch.
var _ = filepath.Match
