package connmonitor

import "sync"

var (
	globalMonitor *Monitor
	once          sync.Once
)

// Global returns the singleton connection monitor instance.
func Global() *Monitor {
	once.Do(func() {
		globalMonitor = NewMonitor()
	})
	return globalMonitor
}
