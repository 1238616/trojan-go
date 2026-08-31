package trojan

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/p4gefau1t/trojan-go/common"
	"github.com/p4gefau1t/trojan-go/config"
	"github.com/p4gefau1t/trojan-go/statistic/connmonitor"
	"github.com/p4gefau1t/trojan-go/statistic/memory"
)

// fullReadConn is a net.Conn stub whose Read always fills the caller's
// buffer, modelling a saturated TCP stream. Only Read is exercised by the
// benchmarks.
type fullReadConn struct {
	buf []byte
}

func (c *fullReadConn) Read(p []byte) (int, error)       { return copy(p, c.buf), nil }
func (c *fullReadConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *fullReadConn) Close() error                     { return nil }
func (c *fullReadConn) LocalAddr() net.Addr              { return benchAddr{} }
func (c *fullReadConn) RemoteAddr() net.Addr             { return benchAddr{} }
func (c *fullReadConn) SetDeadline(time.Time) error      { return nil }
func (c *fullReadConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fullReadConn) SetWriteDeadline(time.Time) error { return nil }

type benchAddr struct{}

func (benchAddr) Network() string { return "tcp" }
func (benchAddr) String() string  { return "127.0.0.1:0" }

// newBenchUser creates a memory authenticator with one user and returns it.
func newBenchUser(b *testing.B) *memory.User {
	b.Helper()
	ctx := config.WithConfig(context.Background(), memory.Name,
		&memory.Config{Passwords: []string{"bench-password"}})
	auth, err := memory.NewAuthenticator(ctx)
	if err != nil {
		b.Fatal(err)
	}
	_, user := auth.AuthUser(common.SHA224String("bench-password"))
	if user == nil {
		b.Fatal("bench user not found")
	}
	return user.(*memory.User)
}

// BenchmarkInboundConnRead measures the per-Read accounting cost of a
// trojan inbound connection on the relay hot path (issue #9). Every Read
// of a saturated stream bumps the user-billing counters (and, when
// enabled, the per-user dashboard counters); the benchmark isolates that
// accounting overhead on top of the stubbed transport Read.
//
// Two chunk sizes are covered: 1500 B models a small-chunk / high-PPS
// stream where per-Read accounting dominates, and 32 KiB matches the
// default relay buffer where the payload memcpy shares the cost.
//
// The parallel variants share ONE user across all goroutines, modelling
// many connections of the same account contending on the same shared
// counter cache lines — the production worst case.
func BenchmarkInboundConnRead(b *testing.B) {
	user := newBenchUser(b)
	hash := user.Hash()

	cases := []struct {
		name    string
		metrics bool
	}{
		{"billing-only", false},
		{"billing+per-user-metrics", true},
	}
	for _, chunk := range []int{1500, 32 * 1024} {
		payload := make([]byte, chunk)
		for _, tc := range cases {
			name := fmt.Sprintf("%s-%dB", tc.name, chunk)
			b.Run(name, func(b *testing.B) {
				c := &InboundConn{Conn: &fullReadConn{buf: payload}, user: user, hash: hash}
				if tc.metrics {
					c.userMetrics = connmonitor.GlobalMetrics().GetUser(hash)
				}
				buf := make([]byte, chunk)
				b.SetBytes(int64(chunk))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := c.Read(buf); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run(name+"-parallel", func(b *testing.B) {
				c := &InboundConn{Conn: &fullReadConn{buf: payload}, user: user, hash: hash}
				if tc.metrics {
					c.userMetrics = connmonitor.GlobalMetrics().GetUser(hash)
				}
				b.SetBytes(int64(chunk))
				b.ReportAllocs()
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					local := make([]byte, chunk)
					for pb.Next() {
						if _, err := c.Read(local); err != nil {
							b.Fatal(err)
						}
					}
				})
			})
		}
	}
}
