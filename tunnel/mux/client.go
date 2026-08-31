package mux

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/xtaci/smux"

	"github.com/p4gefau1t/trojan-go/common"
	"github.com/p4gefau1t/trojan-go/config"
	"github.com/p4gefau1t/trojan-go/log"
	"github.com/p4gefau1t/trojan-go/statistic/connmonitor"
	"github.com/p4gefau1t/trojan-go/tunnel"
)

type muxID uint32

func generateMuxID() muxID {
	return muxID(rand.Uint32())
}

type smuxClientInfo struct {
	id             muxID
	client         *smux.Session
	lastActiveTime time.Time
	underlayConn   tunnel.Conn
}

// Client is a smux client
type Client struct {
	clientPoolLock   sync.Mutex
	clientPool       map[muxID]*smuxClientInfo
	underlay         tunnel.Client
	concurrency      int
	maxPhysicalConns int // Phase 3: adaptive limit (0 = unlimited)
	timeout          time.Duration
	maxStreamBuffer  int // Phase 4: per-stream window
	maxReceiveBuffer int // Phase 4: session-level window
	ctx              context.Context
	cancel           context.CancelFunc
}

func (c *Client) Close() error {
	c.cancel()
	c.clientPoolLock.Lock()
	defer c.clientPoolLock.Unlock()
	for id, info := range c.clientPool {
		info.client.Close()
		log.Debug("mux client", id, "closed")
	}
	return nil
}

func (c *Client) cleanLoop() {
	var checkDuration time.Duration
	if c.timeout <= 0 {
		checkDuration = time.Second * 10
		log.Warn("negative mux timeout")
	} else {
		checkDuration = c.timeout / 4
	}
	log.Debug("check duration:", checkDuration.Seconds(), "s")
	metrics := connmonitor.GlobalMetrics()
	for {
		select {
		case <-time.After(checkDuration):
			// Phase 4: split into short critical section (delete stale)
			// + lock-free section (metrics + logging) to avoid blocking
			// DialConn for the full sweep duration.
			c.clientPoolLock.Lock()
			var toDelete []muxID
			snapshot := make([]*smuxClientInfo, 0, len(c.clientPool))
			for id, info := range c.clientPool {
				if info.client.IsClosed() {
					toDelete = append(toDelete, id)
					info.underlayConn.Close()
					log.Debug("mux client", id, "is dead")
					continue
				}
				if info.client.NumStreams() == 0 && time.Since(info.lastActiveTime) > c.timeout {
					toDelete = append(toDelete, id)
					info.client.Close()
					info.underlayConn.Close()
					log.Debug("mux client", id, "is closed due to inactivity")
					continue
				}
				snapshot = append(snapshot, info)
			}
			for _, id := range toDelete {
				delete(c.clientPool, id)
			}
			poolSize := len(c.clientPool)
			c.clientPoolLock.Unlock()

			// Lock-free: metrics sampling + debug logging.
			for _, info := range snapshot {
				n := info.client.NumStreams()
				metrics.ObserveMuxStreamsPerConn(float64(n))
			}
			log.Debug("current mux clients: ", poolSize)
			for _, info := range snapshot {
				log.Debug(fmt.Sprintf("  - %d/%d", info.client.NumStreams(), c.concurrency))
			}
		case <-c.ctx.Done():
			log.Debug("shutting down mux cleaner..")
			c.clientPoolLock.Lock()
			for id, info := range c.clientPool {
				info.client.Close()
				info.underlayConn.Close()
				delete(c.clientPool, id)
				log.Debug("mux client", id, "closed")
			}
			c.clientPoolLock.Unlock()
			return
		}
	}
}

func (c *Client) newMuxClient() (*smuxClientInfo, error) {
	// The mutex should be locked when this function is called
	id := generateMuxID()
	if _, found := c.clientPool[id]; found {
		return nil, common.NewError("duplicated id")
	}

	fakeAddr := &tunnel.Address{
		DomainName:  "MUX_CONN",
		AddressType: tunnel.DomainName,
	}
	conn, err := c.underlay.DialConn(fakeAddr, &Tunnel{})
	if err != nil {
		return nil, common.NewError("mux failed to dial").Base(err)
	}
	conn = newStickyConn(conn)

	smuxConfig := smux.DefaultConfig()
	// Phase 4: tune smux windows for high-BDP links.
	// Clamp to safe bounds so misconfiguration can't OOM.
	if c.maxStreamBuffer > 0 {
		sb := c.maxStreamBuffer
		if sb < 64<<10 {
			sb = 64 << 10 // min 64 KB
		}
		if sb > 64<<20 {
			sb = 64 << 20 // max 64 MB
		}
		smuxConfig.MaxStreamBuffer = sb
	}
	if c.maxReceiveBuffer > 0 {
		rb := c.maxReceiveBuffer
		if rb < 1<<20 {
			rb = 1 << 20 // min 1 MB
		}
		if rb > 256<<20 {
			rb = 256 << 20 // max 256 MB
		}
		smuxConfig.MaxReceiveBuffer = rb
	}
	smuxConfig.KeepAliveInterval = 15 * time.Second
	smuxConfig.KeepAliveTimeout = 60 * time.Second
	client, _ := smux.Client(conn, smuxConfig)
	info := &smuxClientInfo{
		client:         client,
		underlayConn:   conn,
		id:             id,
		lastActiveTime: time.Now(),
	}
	c.clientPool[id] = info
	return info, nil
}

func (c *Client) DialConn(*tunnel.Address, tunnel.Tunnel) (tunnel.Conn, error) {
	metrics := connmonitor.GlobalMetrics()
	createNewConn := func(info *smuxClientInfo) (tunnel.Conn, error) {
		rwc, err := info.client.Open()
		info.lastActiveTime = time.Now()
		if err != nil {
			info.underlayConn.Close()
			info.client.Close()
			delete(c.clientPool, info.id)
			return nil, common.NewError("mux failed to open stream from client").Base(err)
		}
		metrics.RecordMuxStreamOpen()
		return &Conn{
			rwc:     rwc,
			Conn:    info.underlayConn,
			metrics: metrics,
		}, nil
	}

	c.clientPoolLock.Lock()
	defer c.clientPoolLock.Unlock()

	// Phase 3: when all existing sessions are at capacity and we
	// haven't hit maxPhysicalConns, proactively open a new physical
	// connection to reduce stream-queue latency.
	for _, info := range c.clientPool {
		if info.client.IsClosed() {
			delete(c.clientPool, info.id)
			log.Debug(fmt.Sprintf("Mux client %x is closed", info.id))
			continue
		}
		if info.client.NumStreams() < c.concurrency || c.concurrency <= 0 {
			return createNewConn(info)
		}
	}

	// All existing sessions are at capacity. Check adaptive limit.
	if c.maxPhysicalConns > 0 && len(c.clientPool) >= c.maxPhysicalConns {
		// At the physical connection limit; reuse the first available
		// session even though it's over the concurrency soft-cap.
		for _, info := range c.clientPool {
			if !info.client.IsClosed() {
				return createNewConn(info)
			}
		}
	}

	info, err := c.newMuxClient()
	if err != nil {
		return nil, common.NewError("no available mux client found").Base(err)
	}
	return createNewConn(info)
}

func (c *Client) DialPacket(tunnel.Tunnel) (tunnel.PacketConn, error) {
	panic("not supported")
}

func NewClient(ctx context.Context, underlay tunnel.Client) (*Client, error) {
	clientConfig := config.FromContext(ctx, Name).(*Config)
	ctx, cancel := context.WithCancel(ctx)
	client := &Client{
		underlay:         underlay,
		concurrency:      clientConfig.Mux.Concurrency,
		maxPhysicalConns: clientConfig.Mux.MaxPhysicalConns,
		timeout:          time.Duration(clientConfig.Mux.IdleTimeout) * time.Second,
		maxStreamBuffer:  clientConfig.Mux.MaxStreamBuffer,
		maxReceiveBuffer: clientConfig.Mux.MaxReceiveBuffer,
		ctx:              ctx,
		cancel:           cancel,
		clientPool:       make(map[muxID]*smuxClientInfo),
	}
	go client.cleanLoop()
	log.Debug("mux client created")
	return client, nil
}
