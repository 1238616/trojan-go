package log

import (
	"fmt"
	"strings"
)

// formatKV formats a message with key-value pairs in "msg key=val key=val"
// format. This is a lightweight structured logging adapter that works
// with Go 1.19 (no log/slog). When the project bumps to Go 1.21+,
// these helpers can be mechanically replaced with slog calls.
//
// kvs must have an even number of elements: key1, val1, key2, val2, ...
func formatKV(msg string, kvs []interface{}) string {
	if len(kvs) == 0 {
		return msg
	}
	var b strings.Builder
	b.WriteString(msg)
	for i := 0; i+1 < len(kvs); i += 2 {
		b.WriteByte(' ')
		b.WriteString(fmt.Sprint(kvs[i]))
		b.WriteByte('=')
		b.WriteString(fmt.Sprint(kvs[i+1]))
	}
	return b.String()
}

// DebugKV logs a debug message with structured key-value pairs.
func DebugKV(msg string, kvs ...interface{}) {
	Debug(formatKV(msg, kvs))
}

// InfoKV logs an info message with structured key-value pairs.
func InfoKV(msg string, kvs ...interface{}) {
	Info(formatKV(msg, kvs))
}

// WarnKV logs a warning message with structured key-value pairs.
func WarnKV(msg string, kvs ...interface{}) {
	Warn(formatKV(msg, kvs))
}

// ErrorKV logs an error message with structured key-value pairs.
func ErrorKV(msg string, kvs ...interface{}) {
	Error(formatKV(msg, kvs))
}
