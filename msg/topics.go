// SPDX-License-Identifier: Apache-2.0

package msg

import "strings"

// sessionPrefix is prepended to all NATS subjects to namespace topics per session.
// Set once at startup via SetSessionPrefix().
var sessionPrefix = "rysh"

// SetSessionPrefix configures the NATS subject prefix to the session name.
// Must be called before any actors or bridges are started.
func SetSessionPrefix(session string) {
	if session == "" {
		session = "default"
	}
	sessionPrefix = session
}

// SessionPrefix returns the current NATS subject prefix.
func SessionPrefix() string {
	return sessionPrefix
}

// T constructs a NATS subject prefixed with the session name.
//
//	T("ws", "inbox")                    → "{session}.ws.inbox"
//	T("tab", tabID, "inbox")           → "{session}.tab.{tabID}.inbox"
//	T("pane", paneID, "output")        → "{session}.pane.{paneID}.output"
//	T("pane", "*", "approval", "request") → "{session}.pane.*.approval.request"
func T(parts ...string) string {
	return sessionPrefix + "." + strings.Join(parts, ".")
}
