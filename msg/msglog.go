package msg

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// MessageLogger logs actor-to-actor communication to a file.
// Enabled by setting log_messages = true in the logging config section
// (or RYSH_LOG_MESSAGES=true environment variable as override).
// Logs are written to .rysh/actor-comms.log in the working directory.
//
// Each log line contains: readable datetime, direction (SEND/RECV/REQ/REPLY),
// source file:line, actor name, NATS subject, message TypeTag, payload size,
// and an optional excerpt.
type MessageLogger struct {
	w       io.Writer
	enabled bool
}

var (
	globalMsgLog *MessageLogger
	msgLogOnce   sync.Once
)

// InitMsgLog initializes the singleton MessageLogger from configuration.
// It must be called once from main before any actors are spawned.
// If logMessages is true, the logger creates .rysh/ in the current working
// directory and writes to .rysh/actor-comms.log.
func InitMsgLog(logMessages bool) {
	msgLogOnce.Do(func() {
		globalMsgLog = initLogger(logMessages)
	})
}

// MsgLog returns the singleton MessageLogger. If InitMsgLog was not called,
// it returns a disabled logger.
func MsgLog() *MessageLogger {
	msgLogOnce.Do(func() {
		// Fallback: no one called InitMsgLog — return a disabled logger.
		globalMsgLog = &MessageLogger{enabled: false}
	})
	return globalMsgLog
}

func initLogger(enabled bool) *MessageLogger {
	if !enabled {
		return &MessageLogger{enabled: false}
	}

	// Create .rysh/ directory in the working directory.
	dir := ".rysh"
	if err := os.MkdirAll(dir, 0755); err != nil {
		return &MessageLogger{enabled: false}
	}

	logPath := filepath.Join(dir, "actor-comms.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return &MessageLogger{enabled: false}
	}

	ml := &MessageLogger{w: f, enabled: true}
	ml.writeLine("INFO", "system", "---", "=== message logging started === log_path=%s", logPath)
	return ml
}

// Enabled returns true if message logging is active.
func (ml *MessageLogger) Enabled() bool {
	return ml.enabled && ml.w != nil
}

// LogSend logs an outgoing fire-and-forget message.
func (ml *MessageLogger) LogSend(subject, typeTag string, payloadSize int, actor string) {
	if !ml.Enabled() {
		return
	}
	ml.writeLine("SEND", actor, caller(2), "subject=%s type=%s bytes=%d", subject, typeTag, payloadSize)
}

// LogRequest logs an outgoing request (request-reply).
func (ml *MessageLogger) LogRequest(subject, typeTag string, payloadSize int, timeout time.Duration, actor string) {
	if !ml.Enabled() {
		return
	}
	ml.writeLine("REQ", actor, caller(2), "subject=%s type=%s bytes=%d timeout=%s", subject, typeTag, payloadSize, timeout)
}

// LogReply logs a reply to a request-reply exchange.
func (ml *MessageLogger) LogReply(replyTo, typeTag string, payloadSize int, actor string) {
	if !ml.Enabled() {
		return
	}
	ml.writeLine("REPLY", actor, caller(2), "replyTo=%s type=%s bytes=%d", replyTo, typeTag, payloadSize)
}

// LogRecv logs a message received by a bridge and delivered to an actor.
func (ml *MessageLogger) LogRecv(subject, typeTag string, actor string, isRequest bool) {
	if !ml.Enabled() {
		return
	}
	dir := "RECV"
	if isRequest {
		dir = "RECV-REQ"
	}
	ml.writeLine(dir, actor, caller(2), "subject=%s type=%s", subject, typeTag)
}

// LogError logs a message-related error.
func (ml *MessageLogger) LogError(context, subject string, err error, actor string) {
	if !ml.Enabled() {
		return
	}
	ml.writeLine("ERROR", actor, caller(2), "ctx=%s subject=%s err=%v", context, subject, err)
}

// LogPayload logs message content for detailed debugging.
// Truncates large payloads to maxLen bytes.
func (ml *MessageLogger) LogPayload(direction, subject, typeTag string, payload []byte, actor string) {
	if !ml.Enabled() {
		return
	}
	excerpt := string(payload)
	const maxLen = 500
	if len(excerpt) > maxLen {
		excerpt = excerpt[:maxLen] + fmt.Sprintf("... (%d bytes total)", len(payload))
	}
	ml.writeLine("PAYLOAD-"+direction, actor, caller(2), "subject=%s type=%s data=%s", subject, typeTag, excerpt)
}

// writeLine formats and writes a single log line with readable datetime.
// Format: [2006-01-02 15:04:05.000] DIRECTION  actor=NAME  file=source.go:42  details...
func (ml *MessageLogger) writeLine(direction, actor, file, format string, args ...interface{}) {
	ts := time.Now().Format("2006-01-02 15:04:05.000")
	details := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("[%s] %-10s actor=%-30s file=%-30s %s\n", ts, direction, actor, file, details)
	_, _ = fmt.Fprint(ml.w, line)
}

// caller returns the file:line of the caller at the given skip depth.
// skip=1 means the caller of the function that called caller().
func caller(skip int) string {
	_, file, line, ok := runtime.Caller(skip)
	if !ok {
		return "???:0"
	}
	// Trim to just package/file.go for readability.
	parts := strings.Split(file, "/")
	if len(parts) >= 2 {
		file = parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
	return fmt.Sprintf("%s:%d", file, line)
}
