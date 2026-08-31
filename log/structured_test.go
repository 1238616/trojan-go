package log

import (
	"io"
	"testing"
)

// stubLogger is a no-op Logger with a configurable threshold, used to
// verify level gating without touching the process-global logger state
// more than the test requires.
type stubLogger struct {
	level LogLevel
}

func (l *stubLogger) SetLogLevel(level LogLevel)    { l.level = level }
func (l *stubLogger) GetLevel() LogLevel            { return l.level }
func (l *stubLogger) SetOutput(io.Writer)           {}
func (l *stubLogger) Fatal(v ...interface{})        {}
func (l *stubLogger) Fatalf(string, ...interface{}) {}
func (l *stubLogger) Error(v ...interface{})        {}
func (l *stubLogger) Errorf(string, ...interface{}) {}
func (l *stubLogger) Warn(v ...interface{})         {}
func (l *stubLogger) Warnf(string, ...interface{})  {}
func (l *stubLogger) Info(v ...interface{})         {}
func (l *stubLogger) Infof(string, ...interface{})  {}
func (l *stubLogger) Debug(v ...interface{})        {}
func (l *stubLogger) Debugf(string, ...interface{}) {}
func (l *stubLogger) Trace(v ...interface{})        {}
func (l *stubLogger) Tracef(string, ...interface{}) {}

// withLogger swaps the global logger for the duration of fn and restores
// the previous one afterwards.
func withLogger(l Logger, fn func()) {
	prev := logger
	logger = l
	defer func() { logger = prev }()
	fn()
}

// TestKVFilteredZeroAllocs is the acceptance test from issue #3: when
// the level filters a KV message out, no string formatting may happen —
// zero allocations, not just fewer. The arguments mirror the relay hot
// path's DebugKV("conn relay ends", …) call.
func TestKVFilteredZeroAllocs(t *testing.T) {
	withLogger(&stubLogger{level: InfoLevel}, func() {
		allocs := testing.AllocsPerRun(100, func() {
			DebugKV("conn relay ends",
				"conn_id", "conn-123",
				"target", "example.com:443",
				"exit", "local",
				"reason", "eof")
		})
		if allocs != 0 {
			t.Fatalf("filtered DebugKV allocated %.2f times per call, want 0", allocs)
		}
	})

	// Error level filters Info/Warn/Debug alike.
	withLogger(&stubLogger{level: ErrorLevel}, func() {
		allocs := testing.AllocsPerRun(100, func() {
			InfoKV("cluster: relay decision", "target", "example.com:443", "peer", "node-2")
			DebugKV("freedom dial ok", "target", "example.com:443", "dur_ms", "12")
		})
		if allocs != 0 {
			t.Fatalf("filtered InfoKV/DebugKV allocated %.2f times per call, want 0", allocs)
		}
	})
}

// TestKVEmittedStillFormats guards the other direction: a message whose
// level passes the threshold must still be formatted and delivered.
func TestKVEmittedStillFormats(t *testing.T) {
	if got := formatKV("m", []interface{}{"k", "v", "n", 2}); got != "m k=v n=2" {
		t.Fatalf("formatKV = %q", got)
	}
	if got := formatKV("m", nil); got != "m" {
		t.Fatalf("formatKV(no kvs) = %q", got)
	}
}

func TestLevelEnabled(t *testing.T) {
	withLogger(&stubLogger{level: InfoLevel}, func() {
		if LevelEnabled(AllLevel) {
			t.Fatal("Debug must be disabled at Info level")
		}
		if !LevelEnabled(InfoLevel) || !LevelEnabled(ErrorLevel) {
			t.Fatal("Info/Error must be enabled at Info level")
		}
	})
	// The default EmptyLogger emits nothing, so everything is filtered.
	if LevelEnabled(ErrorLevel) {
		t.Fatal("EmptyLogger must report everything as filtered")
	}
}

// BenchmarkKVFiltered measures the hot-path cost when the level filters
// the message — the case issue #3 optimizes. Expect 0 allocs/op.
func BenchmarkKVFiltered(b *testing.B) {
	withLogger(&stubLogger{level: InfoLevel}, func() {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			DebugKV("conn relay ends",
				"conn_id", "conn-123",
				"target", "example.com:443",
				"exit", "local",
				"reason", "eof")
		}
	})
}

// BenchmarkKVEmitted is the comparison case: the level passes and the
// message is actually formatted.
func BenchmarkKVEmitted(b *testing.B) {
	withLogger(&stubLogger{level: AllLevel}, func() {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			DebugKV("conn relay ends",
				"conn_id", "conn-123",
				"target", "example.com:443",
				"exit", "local",
				"reason", "eof")
		}
	})
}
