package secretnat

import (
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Options configures a Manager. Zero value = disabled.
type Options struct {
	// Enabled is the session-wide default; individual sessions may override
	// (##snat on|off on a pane).
	Enabled bool
	// Mode selects semantic (type-preserving) or private tokens.
	Mode Mode
	// RestoreDisplay controls whether displayed LLM output has tokens
	// restored to real values. Default false: the pane display buffer is
	// persisted and forwarded to listeners, so restoring would re-leak.
	RestoreDisplay bool
	// MappingTTL expires idle per-conversation mappings; 0 = conversation
	// lifetime (mappings die when the session closes or the process exits).
	MappingTTL time.Duration
	// DisabledDetectors removes built-in detectors by name.
	DisabledDetectors []string
	// CustomDetectors adds user-defined regex detectors.
	CustomDetectors []CustomDetector
}

// SessionStats is the value-free per-session metrics view.
type SessionStats struct {
	Detected    int            // total value→token replacements (both tiers)
	Restored    int            // total token→value replacements
	Mappings    int            // distinct detected-tier mappings alive
	PerDetector map[string]int // hit counts per detector
	Entries     []MappingEntry // token/detector/hits — never values
	Override    *bool          // per-session enable override (nil = default)
}

// ManagerStats is the value-free aggregate metrics view (##snat status).
type ManagerStats struct {
	Enabled        bool
	Mode           Mode
	RestoreDisplay bool
	KnownSecrets   int
	Sessions       int
	Detected       int
	Restored       int
	PerDetector    map[string]int
}

// SessionHandle is the interface the agentic layer consumes. All methods
// are safe on a nil *Session and cheap no-ops when disabled, so callers can
// thread it unconditionally.
type SessionHandle interface {
	Enabled() bool
	Sanitize(text string) string
	SanitizeJSON(raw json.RawMessage) json.RawMessage
	Restore(text string) string
	RestoreJSON(raw json.RawMessage) json.RawMessage
	RestoreDisplay() bool
	NewStreamRestorer() *StreamRestorer
	Stats() SessionStats
}

// Manager owns the process-wide SecretNAT state: options, detector registry,
// the known-secret snapshot, and one Session per conversation (pane / agent /
// humanoid). Safe for concurrent use.
type Manager struct {
	mu       sync.RWMutex
	opts     Options
	registry *Registry
	known    *KnownSet
	sessions map[string]*Session
}

// NewManager builds a Manager from options. Custom detector patterns are
// compiled eagerly; an invalid pattern fails construction.
func NewManager(opts Options) (*Manager, error) {
	reg, err := NewDefaultRegistry(opts.DisabledDetectors, opts.CustomDetectors)
	if err != nil {
		return nil, err
	}
	if opts.Mode != ModePrivate {
		opts.Mode = ModeSemantic
	}
	return &Manager{
		opts:     opts,
		registry: reg,
		known:    NewKnownSet(nil),
		sessions: make(map[string]*Session),
	}, nil
}

// Session returns the session for convID, creating it on first use. Each
// conversation's mapping table is isolated; mappings never leak across
// sessions. Session creation opportunistically sweeps TTL-expired sessions.
func (m *Manager) Session(convID string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepExpiredLocked()
	if s, ok := m.sessions[convID]; ok {
		return s
	}
	s := &Session{mgr: m, id: convID, table: NewMappingTable()}
	m.sessions[convID] = s
	return s
}

// CloseSession drops convID's mapping table (pane closed). Real values in
// the table become unreachable immediately.
func (m *Manager) CloseSession(convID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, convID)
}

// SweepExpired drops sessions idle longer than MappingTTL. No-op when TTL
// is 0. Returns the number of sessions dropped.
func (m *Manager) SweepExpired() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sweepExpiredLocked()
}

// sweepExpiredLocked implements SweepExpired; the caller holds m.mu.
func (m *Manager) sweepExpiredLocked() int {
	if m.opts.MappingTTL <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-m.opts.MappingTTL)
	n := 0
	for id, s := range m.sessions {
		if s.table.LastUsed().Before(cutoff) {
			delete(m.sessions, id)
			n++
		}
	}
	if n > 0 {
		slog.Info("secretnat: expired idle sessions", "count", n)
	}
	return n
}

// UpdateKnownSecrets atomically swaps the known-tier snapshot. Call from the
// secret store's owner on every mutation (values are held only in memory,
// inside the snapshot).
func (m *Manager) UpdateKnownSecrets(secrets []KnownSecret) {
	ks := NewKnownSet(secrets)
	m.mu.Lock()
	m.known = ks
	m.mu.Unlock()
	slog.Debug("secretnat: known-secret set updated", "count", ks.Size())
}

// SetEnabled flips the session-wide default (##snat on|off --session).
func (m *Manager) SetEnabled(v bool) {
	m.mu.Lock()
	m.opts.Enabled = v
	m.mu.Unlock()
}

// Enabled returns the session-wide default.
func (m *Manager) Enabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.opts.Enabled
}

// SetMode switches token style for subsequently minted tokens.
func (m *Manager) SetMode(mode Mode) {
	if mode != ModePrivate {
		mode = ModeSemantic
	}
	m.mu.Lock()
	m.opts.Mode = mode
	m.mu.Unlock()
}

// Mode returns the current token mode.
func (m *Manager) Mode() Mode {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.opts.Mode
}

// RestoreDisplay reports whether displayed output should be restored.
func (m *Manager) RestoreDisplay() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.opts.RestoreDisplay
}

// DetectorNames lists active detectors (##snat status).
func (m *Manager) DetectorNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.registry.Names()
}

// Stats aggregates value-free counters across sessions.
func (m *Manager) Stats() ManagerStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := ManagerStats{
		Enabled:        m.opts.Enabled,
		Mode:           m.opts.Mode,
		RestoreDisplay: m.opts.RestoreDisplay,
		KnownSecrets:   m.known.Size(),
		Sessions:       len(m.sessions),
		PerDetector:    make(map[string]int),
	}
	for _, s := range m.sessions {
		ss := s.Stats()
		st.Detected += ss.Detected
		st.Restored += ss.Restored
		for k, v := range ss.PerDetector {
			st.PerDetector[k] += v
		}
	}
	return st
}

// snapshot returns the pieces a session needs for one translation pass.
func (m *Manager) snapshot() (bool, *KnownSet, *Registry, *Generator, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.opts.Enabled, m.known, m.registry, NewGenerator(m.opts.Mode), m.opts.RestoreDisplay
}

// ---------------------------------------------------------------------------
// Session
// ---------------------------------------------------------------------------

// Session is one conversation's translation context: the manager's shared
// detector/known state plus a private mapping table. All methods are safe on
// a nil receiver (no-ops), for callers that thread the handle
// unconditionally.
type Session struct {
	mgr *Manager
	id  string

	mu       sync.Mutex
	override *bool // nil = follow manager default (##snat on|off per pane)
	detected int
	restored int

	table *MappingTable
}

var _ SessionHandle = (*Session)(nil)

// ID returns the conversation id this session serves.
func (s *Session) ID() string {
	if s == nil {
		return ""
	}
	return s.id
}

// SetOverride sets (or clears, with nil) the per-session enable override.
func (s *Session) SetOverride(v *bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.override = v
	s.mu.Unlock()
}

// Enabled reports whether translation is active for this session: the
// per-session override when set, else the manager default.
func (s *Session) Enabled() bool {
	if s == nil || s.mgr == nil {
		return false
	}
	s.mu.Lock()
	ov := s.override
	s.mu.Unlock()
	if ov != nil {
		return *ov
	}
	return s.mgr.Enabled()
}

// RestoreDisplay reports whether displayed output should be restored.
func (s *Session) RestoreDisplay() bool {
	if s == nil || s.mgr == nil {
		return false
	}
	return s.mgr.RestoreDisplay()
}

// Sanitize translates secrets in text to synthetic tokens. Identity when
// disabled.
func (s *Session) Sanitize(text string) string {
	if !s.Enabled() || text == "" {
		return text
	}
	_, known, reg, gen, _ := s.mgr.snapshot()
	out, n := Sanitize(text, known, reg, s.table, gen)
	if n > 0 {
		s.mu.Lock()
		s.detected += n
		s.mu.Unlock()
		slog.Debug("secretnat: sanitized", "session", s.id, "replacements", n)
	}
	return out
}

// SanitizeJSON translates secrets in every string leaf of raw.
func (s *Session) SanitizeJSON(raw json.RawMessage) json.RawMessage {
	if !s.Enabled() || len(raw) == 0 {
		return raw
	}
	return transformJSON(raw, func(v string) string { return s.Sanitize(v) })
}

// Restore replaces synthetic tokens in text with real values. Identity when
// disabled.
func (s *Session) Restore(text string) string {
	if !s.Enabled() || text == "" {
		return text
	}
	_, known, _, _, _ := s.mgr.snapshot()
	out, n := Restore(text, known, s.table)
	if n > 0 {
		s.mu.Lock()
		s.restored += n
		s.mu.Unlock()
		slog.Debug("secretnat: restored", "session", s.id, "replacements", n)
	}
	return out
}

// RestoreJSON restores tokens in every string leaf of raw.
func (s *Session) RestoreJSON(raw json.RawMessage) json.RawMessage {
	if !s.Enabled() || len(raw) == 0 {
		return raw
	}
	return transformJSON(raw, func(v string) string { return s.Restore(v) })
}

// NewStreamRestorer returns a restorer bound to this session's live token
// set (the mapping table can grow mid-stream).
func (s *Session) NewStreamRestorer() *StreamRestorer {
	if s == nil {
		return NewStreamRestorer(func(t string) string { return t }, func() []string { return nil })
	}
	return NewStreamRestorer(
		func(t string) string { return s.Restore(t) },
		func() []string {
			if !s.Enabled() {
				return nil
			}
			_, known, _, _, _ := s.mgr.snapshot()
			return append(s.table.Tokens(), known.Tokens()...)
		},
	)
}

// Reveal resolves a token back to its real value LOCALLY — a detected-tier
// mapping token (e.g. "sk_live_SNAT000001"), a known-tier "${NAME}" reference,
// or a bare registered NAME. Returns (value, tier, ok). This is the explicit
// "##snat get" escape hatch: the value is printed to the owner's own pane and
// is NEVER placed on the outbound path. source is "detected" or "known".
func (s *Session) Reveal(token string) (value, source string, ok bool) {
	if s == nil || s.mgr == nil {
		return "", "", false
	}
	token = strings.TrimSpace(token)
	_, known, _, _, _ := s.mgr.snapshot()
	// Known-tier "${NAME}" reference (or a bare NAME).
	name := token
	if strings.HasPrefix(token, "${") && strings.HasSuffix(token, "}") {
		name = token[2 : len(token)-1]
	}
	if v, kok := known.ValueFor(name); kok {
		return v, "known", true
	}
	// Detected-tier minted token.
	if v, tok := s.table.RevealToken(token); tok {
		return v, "detected", true
	}
	return "", "", false
}

// Stats returns the session's value-free counters.
func (s *Session) Stats() SessionStats {
	if s == nil {
		return SessionStats{}
	}
	s.mu.Lock()
	st := SessionStats{Detected: s.detected, Restored: s.restored, Override: s.override}
	s.mu.Unlock()
	st.Mappings = s.table.Size()
	st.PerDetector = s.table.PerDetector()
	st.Entries = s.table.Entries()
	return st
}
