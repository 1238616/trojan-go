package cluster

import (
	"net"
	"testing"

	"github.com/p4gefau1t/trojan-go/tunnel"
)

func TestMatcherCIDR(t *testing.T) {
	tm := NewTargetMatcher([]string{"cidr:149.154.160.0/20"})

	// Should match: 149.154.175.53 is in 149.154.160.0/20
	addr := &tunnel.Address{
		IP:          net.ParseIP("149.154.175.53"),
		Port:        443,
		AddressType: tunnel.IPv4,
	}
	if !tm.Match(addr) {
		t.Fatal("expected 149.154.175.53 to match cidr:149.154.160.0/20")
	}

	// Should not match: 8.8.8.8
	addr2 := &tunnel.Address{
		IP:          net.ParseIP("8.8.8.8"),
		Port:        443,
		AddressType: tunnel.IPv4,
	}
	if tm.Match(addr2) {
		t.Fatal("expected 8.8.8.8 to NOT match cidr:149.154.160.0/20")
	}
}

func TestMatcherDomain(t *testing.T) {
	tm := NewTargetMatcher([]string{"domain:telegram.org"})

	// Exact match
	addr := &tunnel.Address{
		DomainName:  "telegram.org",
		Port:        443,
		AddressType: tunnel.DomainName,
	}
	if !tm.Match(addr) {
		t.Fatal("expected telegram.org to match")
	}

	// Subdomain match
	addr2 := &tunnel.Address{
		DomainName:  "api.telegram.org",
		Port:        443,
		AddressType: tunnel.DomainName,
	}
	if !tm.Match(addr2) {
		t.Fatal("expected api.telegram.org to match domain:telegram.org")
	}

	// Non-match
	addr3 := &tunnel.Address{
		DomainName:  "example.com",
		Port:        80,
		AddressType: tunnel.DomainName,
	}
	if tm.Match(addr3) {
		t.Fatal("expected example.com to NOT match domain:telegram.org")
	}
}

func TestMatcherIP(t *testing.T) {
	tm := NewTargetMatcher([]string{"ip:149.154.175.53"})

	addr := &tunnel.Address{
		IP:          net.ParseIP("149.154.175.53"),
		Port:        443,
		AddressType: tunnel.IPv4,
	}
	if !tm.Match(addr) {
		t.Fatal("expected 149.154.175.53 to match ip:149.154.175.53")
	}

	addr2 := &tunnel.Address{
		IP:          net.ParseIP("1.1.1.1"),
		Port:        443,
		AddressType: tunnel.IPv4,
	}
	if tm.Match(addr2) {
		t.Fatal("expected 1.1.1.1 to NOT match ip:149.154.175.53")
	}
}

func TestMatcherEmpty(t *testing.T) {
	tm := NewTargetMatcher([]string{})

	addr := &tunnel.Address{
		IP:          net.ParseIP("1.1.1.1"),
		Port:        80,
		AddressType: tunnel.IPv4,
	}
	if tm.Match(addr) {
		t.Fatal("empty matcher should not match anything")
	}
}

func TestMatcherMultiRules(t *testing.T) {
	tm := NewTargetMatcher([]string{
		"cidr:149.154.160.0/20",
		"cidr:91.108.0.0/16",
		"domain:telegram.org",
		"ip:1.2.3.4",
	})

	tests := []struct {
		name  string
		addr  *tunnel.Address
		match bool
	}{
		{"cidr match 149.154", &tunnel.Address{IP: net.ParseIP("149.154.170.1"), Port: 443, AddressType: tunnel.IPv4}, true},
		{"cidr match 91.108", &tunnel.Address{IP: net.ParseIP("91.108.56.1"), Port: 443, AddressType: tunnel.IPv4}, true},
		{"domain match", &tunnel.Address{DomainName: "web.telegram.org", Port: 443, AddressType: tunnel.DomainName}, true},
		{"ip match", &tunnel.Address{IP: net.ParseIP("1.2.3.4"), Port: 80, AddressType: tunnel.IPv4}, true},
		{"no match", &tunnel.Address{IP: net.ParseIP("8.8.8.8"), Port: 53, AddressType: tunnel.IPv4}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tm.Match(tt.addr)
			if got != tt.match {
				t.Fatalf("Match()=%v, want %v", got, tt.match)
			}
		})
	}
}

func TestMatcherNil(t *testing.T) {
	var tm *TargetMatcher
	addr := &tunnel.Address{IP: net.ParseIP("1.1.1.1"), Port: 80, AddressType: tunnel.IPv4}
	if tm.Match(addr) {
		t.Fatal("nil matcher should not match")
	}
}
