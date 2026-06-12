package tls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"io/ioutil"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"

	"github.com/p4gefau1t/trojan-go/common"
	"github.com/p4gefau1t/trojan-go/config"
	"github.com/p4gefau1t/trojan-go/log"
	"github.com/p4gefau1t/trojan-go/statistic/connmonitor"
	"github.com/p4gefau1t/trojan-go/tunnel"
	"github.com/p4gefau1t/trojan-go/tunnel/tls/fingerprint"
	"github.com/p4gefau1t/trojan-go/tunnel/transport"
)

// sessionCache is the process-wide TLS session cache shared by all
// clients. Using a single LRU across all connections maximises the
// resumption ratio when clients open many parallel flows to the same
// SNI, which is the typical case for trojan clients.
//
// Capacity of 256 matches the design-plan recommendation. Entries are
// keyed by SNI ("host:port" is not used because the same SNI is
// normally shared across multiple transports).
var (
	sessionCache     tls.ClientSessionCache
	sessionCacheOnce sync.Once
)

func getSessionCache() tls.ClientSessionCache {
	sessionCacheOnce.Do(func() {
		sessionCache = tls.NewLRUClientSessionCache(256)
	})
	return sessionCache
}

// Client is a tls client
type Client struct {
	verify        bool
	sni           string
	ca            *x509.CertPool
	cipher        []uint16
	sessionTicket bool
	reuseSession  bool
	fingerprint   string
	helloID       utls.ClientHelloID
	keyLogger     io.WriteCloser
	underlay      tunnel.Client
}

func (c *Client) Close() error {
	if c.keyLogger != nil {
		c.keyLogger.Close()
	}
	return c.underlay.Close()
}

func (c *Client) DialPacket(tunnel.Tunnel) (tunnel.PacketConn, error) {
	panic("not supported")
}

func (c *Client) DialConn(_ *tunnel.Address, overlay tunnel.Tunnel) (tunnel.Conn, error) {
	conn, err := c.underlay.DialConn(nil, &Tunnel{})
	if err != nil {
		return nil, common.NewError("tls failed to dial conn").Base(err)
	}

	if c.fingerprint != "" {
		// utls fingerprint
		tlsConn := utls.UClient(conn, &utls.Config{
			RootCAs:            c.ca,
			ServerName:         c.sni,
			InsecureSkipVerify: !c.verify,
			KeyLogWriter:       c.keyLogger,
		}, c.helloID)
		if err := tlsConn.Handshake(); err != nil {
			return nil, common.NewError("tls failed to handshake with remote server").Base(err)
		}
		return &transport.Conn{
			Conn: tlsConn,
		}, nil
	}
	// golang default tls library
	tlsCfg := &tls.Config{
		InsecureSkipVerify:     !c.verify,
		ServerName:             c.sni,
		RootCAs:                c.ca,
		KeyLogWriter:           c.keyLogger,
		CipherSuites:           c.cipher,
		SessionTicketsDisabled: !c.sessionTicket,
	}
	// Phase 2: when session tickets are enabled, attach the
	// process-wide LRU session cache so repeat handshakes can resume
	// and skip the full RTT + server cert validation cost.
	if c.sessionTicket {
		tlsCfg.ClientSessionCache = getSessionCache()
	}
	hsStart := time.Now()
	tlsConn := tls.Client(conn, tlsCfg)
	err = tlsConn.Handshake()
	hsDur := time.Since(hsStart)
	metrics := connmonitor.GlobalMetrics()
	if err != nil {
		metrics.RecordHandshake(connmonitor.HandshakeTLS, false, hsDur)
		log.ErrorKV("tls handshake failed", "sni", c.sni, "dur_ms", hsDur.Milliseconds(), "err", err)
		return nil, common.NewError("tls failed to handshake with remote server").Base(err)
	}
	metrics.RecordHandshake(connmonitor.HandshakeTLS, true, hsDur)
	resumed := tlsConn.ConnectionState().DidResume
	if resumed {
		metrics.RecordTLSResumed()
	}
	log.DebugKV("tls handshake ok", "sni", c.sni, "dur_ms", hsDur.Milliseconds(), "resumed", resumed)
	return &transport.Conn{
		Conn: tlsConn,
	}, nil
}

// NewClient creates a tls client
func NewClient(ctx context.Context, underlay tunnel.Client) (*Client, error) {
	cfg := config.FromContext(ctx, Name).(*Config)

	helloID := utls.ClientHelloID{}
	if cfg.TLS.Fingerprint != "" {
		switch cfg.TLS.Fingerprint {
		case "firefox":
			helloID = utls.HelloFirefox_Auto
		case "chrome":
			helloID = utls.HelloChrome_Auto
		case "ios":
			helloID = utls.HelloIOS_Auto
		default:
			return nil, common.NewError("invalid fingerprint " + cfg.TLS.Fingerprint)
		}
		log.Info("tls fingerprint", cfg.TLS.Fingerprint, "applied")
	}

	if cfg.TLS.SNI == "" {
		cfg.TLS.SNI = cfg.RemoteHost
		log.Warn("tls sni is unspecified")
	}

	client := &Client{
		underlay:      underlay,
		verify:        cfg.TLS.Verify,
		sni:           cfg.TLS.SNI,
		cipher:        fingerprint.ParseCipher(strings.Split(cfg.TLS.Cipher, ":")),
		sessionTicket: cfg.TLS.ReuseSession,
		fingerprint:   cfg.TLS.Fingerprint,
		helloID:       helloID,
	}

	if cfg.TLS.CertPath != "" {
		caCertByte, err := ioutil.ReadFile(cfg.TLS.CertPath)
		if err != nil {
			return nil, common.NewError("failed to load cert file").Base(err)
		}
		client.ca = x509.NewCertPool()
		ok := client.ca.AppendCertsFromPEM(caCertByte)
		if !ok {
			log.Warn("invalid cert list")
		}
		log.Info("using custom cert")

		// print cert info
		pemCerts := caCertByte
		for len(pemCerts) > 0 {
			var block *pem.Block
			block, pemCerts = pem.Decode(pemCerts)
			if block == nil {
				break
			}
			if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
				continue
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				continue
			}
			log.Trace("issuer:", cert.Issuer, "subject:", cert.Subject)
		}
	}

	if cfg.TLS.CertPath == "" {
		log.Info("cert is unspecified, using default ca list")
	}

	log.Debug("tls client created")
	return client, nil
}
