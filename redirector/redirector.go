package redirector

import (
	"context"
	"io"
	"net"
	"reflect"
	"time"

	"github.com/p4gefau1t/trojan-go/common"
	"github.com/p4gefau1t/trojan-go/log"
)

const (
	// maxConcurrentRedirections bounds how many fallback relays may run at
	// once. Redirected connections are typically failed-auth traffic; without
	// a cap a slow trickle of them could pin unbounded goroutines and file
	// descriptors (a slowloris-style resource hold).
	maxConcurrentRedirections = 64
	// redirectIdleTimeout tears down a redirection relay when neither
	// direction makes progress for this long.
	redirectIdleTimeout = 60 * time.Second
)

type Dial func(net.Addr) (net.Conn, error)

func defaultDial(addr net.Addr) (net.Conn, error) {
	return net.DialTimeout("tcp", addr.String(), 10*time.Second)
}

type Redirection struct {
	Dial
	RedirectTo  net.Addr
	InboundConn net.Conn
}

type Redirector struct {
	ctx             context.Context
	redirectionChan chan *Redirection
	sem             chan struct{}
}

func (r *Redirector) Redirect(redirection *Redirection) {
	select {
	case r.redirectionChan <- redirection:
		log.Debug("redirect request")
	case <-r.ctx.Done():
		log.Debug("exiting")
	}
}

func (r *Redirector) worker() {
	for {
		select {
		case redirection := <-r.redirectionChan:
			// Concurrency cap: drop (and close) rather than queue unboundedly
			// when the relay is already at capacity.
			select {
			case r.sem <- struct{}{}:
			default:
				log.Warn("redirector at capacity, dropping redirection")
				if !isNilConn(redirection.InboundConn) {
					redirection.InboundConn.Close()
				}
				continue
			}
			handle := func(redirection *Redirection) {
				defer func() { <-r.sem }()
				if isNilConn(redirection.InboundConn) {
					log.Error("nil inbound conn")
					return
				}
				defer redirection.InboundConn.Close()
				if isNilAddr(redirection.RedirectTo) {
					log.Error("nil redirection addr")
					return
				}
				if redirection.Dial == nil {
					redirection.Dial = defaultDial
				}
				log.Warn("redirecting connection from", redirection.InboundConn.RemoteAddr(), "to", redirection.RedirectTo.String())
				outboundConn, err := redirection.Dial(redirection.RedirectTo)
				if err != nil {
					log.Error(common.NewError("failed to redirect to target address").Base(err))
					return
				}
				defer outboundConn.Close()
				errChan := make(chan error, 2)
				// Wrap both sides so a stalled relay is reaped after the idle
				// timeout instead of holding the FDs indefinitely.
				inbound := &idleTimeoutConn{Conn: redirection.InboundConn, idle: redirectIdleTimeout}
				outbound := &idleTimeoutConn{Conn: outboundConn, idle: redirectIdleTimeout}
				copyConn := func(a, b net.Conn) {
					_, err := io.Copy(a, b)
					errChan <- err
				}
				go copyConn(outbound, inbound)
				go copyConn(inbound, outbound)
				select {
				case err := <-errChan:
					if err != nil {
						log.Error(common.NewError("failed to redirect").Base(err))
					}
					log.Info("redirection done")
				case <-r.ctx.Done():
					log.Debug("exiting")
					return
				}
			}
			go handle(redirection)
		case <-r.ctx.Done():
			log.Debug("shutting down redirector")
			return
		}
	}
}

// idleTimeoutConn refreshes its deadlines on activity so a relay with no
// progress in either direction is closed after the idle timeout.
type idleTimeoutConn struct {
	net.Conn
	idle time.Duration
}

func (c *idleTimeoutConn) Read(p []byte) (int, error) {
	c.Conn.SetReadDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(p)
}

func (c *idleTimeoutConn) Write(p []byte) (int, error) {
	c.Conn.SetWriteDeadline(time.Now().Add(c.idle))
	return c.Conn.Write(p)
}

// isNilConn / isNilAddr report whether the interface is nil or holds a nil
// pointer. reflect.Value.IsNil panics for non-nilable kinds (some net.Conn /
// net.Addr implementations are struct values), so guard by kind first.
func isNilConn(c net.Conn) bool {
	return isNilInterface(c)
}

func isNilAddr(a net.Addr) bool {
	return isNilInterface(a)
}

func isNilInterface(v interface{}) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice, reflect.UnsafePointer:
		return rv.IsNil()
	default:
		return false
	}
}

func NewRedirector(ctx context.Context) *Redirector {
	r := &Redirector{
		ctx:             ctx,
		redirectionChan: make(chan *Redirection, 64),
		sem:             make(chan struct{}, maxConcurrentRedirections),
	}
	go r.worker()
	return r
}
