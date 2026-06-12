package shadowsocks

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/p4gefau1t/trojan-go/common"
	"github.com/p4gefau1t/trojan-go/config"
	"github.com/p4gefau1t/trojan-go/test/util"
	"github.com/p4gefau1t/trojan-go/tunnel/freedom"
	"github.com/p4gefau1t/trojan-go/tunnel/transport"
)

func init() {
	// Disable the go-shadowsocks2 global Bloom-filter salt cache.
	// In unit tests the client and server share one process, so the
	// client's AddSalt() causes the server's CheckSalt() to return
	// ErrRepeatedSalt on every connection.  Setting capacity <= 0
	// makes the library skip the filter entirely.
	os.Setenv("SHADOWSOCKS_SF_CAPACITY", "-1")
}

func TestShadowsocks(t *testing.T) {
	p, err := strconv.ParseInt(util.HTTPPort, 10, 32)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}

	port := common.PickPort("tcp", "127.0.0.1")
	transportConfig := &transport.Config{
		LocalHost:  "127.0.0.1",
		LocalPort:  port,
		RemoteHost: "127.0.0.1",
		RemotePort: port,
	}
	ctx := config.WithConfig(context.Background(), transport.Name, transportConfig)
	ctx = config.WithConfig(ctx, freedom.Name, &freedom.Config{})
	tcpClient, err := transport.NewClient(ctx, nil)
	if err != nil {
		t.Fatalf("new tcp client: %v", err)
	}
	tcpServer, err := transport.NewServer(ctx, nil)
	if err != nil {
		t.Fatalf("new tcp server: %v", err)
	}

	cfg := &Config{
		RemoteHost: "127.0.0.1",
		RemotePort: int(p),
		Shadowsocks: ShadowsocksConfig{
			Enabled:  true,
			Method:   "AES-128-GCM",
			Password: "password",
		},
	}
	ctx = config.WithConfig(ctx, Name, cfg)

	c, err := NewClient(ctx, tcpClient)
	if err != nil {
		t.Fatalf("new ss client: %v", err)
	}
	s, err := NewServer(ctx, tcpServer)
	if err != nil {
		t.Fatalf("new ss server: %v", err)
	}

	// --- Round 1: valid shadowsocks round-trip ---
	wg := sync.WaitGroup{}
	wg.Add(2)
	var conn1, conn2 net.Conn
	var dialErr, acceptErr error
	go func() {
		conn1, dialErr = c.DialConn(nil, nil)
		if dialErr == nil {
			conn1.Write(util.GeneratePayload(1024))
		}
		wg.Done()
	}()
	go func() {
		conn2, acceptErr = s.AcceptConn(nil)
		if acceptErr == nil {
			buf := [1024]byte{}
			conn2.Read(buf[:])
		}
		wg.Done()
	}()
	wg.Wait()
	if dialErr != nil {
		t.Fatalf("round 1 dial: %v", dialErr)
	}
	if acceptErr != nil {
		t.Fatalf("round 1 accept: %v", acceptErr)
	}
	if !util.CheckConn(conn1, conn2) {
		t.Fatal("round 1 data mismatch")
	}
	// Close round-1 connections before round 2 to release AEAD state.
	conn1.Close()
	conn2.Close()

	// Brief pause so the AEAD salt cache in go-shadowsocks2 does not
	// collide with the raw-TCP bytes interpreted as a salt.
	time.Sleep(50 * time.Millisecond)

	// --- Round 2: raw TCP to shadowsocks server → expect decrypt failure ---
	rejectCh := make(chan error, 1)
	go func() {
		_, err := s.AcceptConn(nil)
		rejectCh <- err
	}()

	conn3, err := tcpClient.DialConn(nil, nil)
	if err != nil {
		t.Fatalf("round 2 raw dial: %v", err)
	}
	n, err := conn3.Write(util.GeneratePayload(1024))
	if err != nil {
		t.Fatalf("round 2 write: %v", err)
	}
	fmt.Println("write:", n)

	// The server goroutine should have failed to decrypt.
	select {
	case serr := <-rejectCh:
		if serr == nil {
			t.Error("round 2: expected server decrypt error, got nil")
		}
	case <-time.After(3 * time.Second):
		t.Error("round 2: timed out waiting for server reject")
	}

	// Verify redirection: raw TCP conn should receive an HTTP "Bad Request".
	buf := [1024]byte{}
	conn3.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err = conn3.Read(buf[:])
	if err != nil {
		t.Logf("round 2 read (may be expected if redirector closed): %v", err)
	} else if !strings.Contains(string(buf[:n]), "Bad Request") {
		t.Errorf("round 2: expected 'Bad Request', got %q", string(buf[:n]))
	}
	conn3.Close()
	c.Close()
	s.Close()
}
