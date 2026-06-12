package cluster

import (
	"net"
	"strings"

	"github.com/p4gefau1t/trojan-go/tunnel"
)

// TargetMatcher determines whether a destination address falls within
// the cluster's managed target set. Supports CIDR and domain rules.
type TargetMatcher struct {
	cidrs   []*net.IPNet
	domains []string // exact or suffix match
	ips     []net.IP // individual IP addresses
}

// NewTargetMatcher parses target rules from config.
// Supported formats:
//   - "cidr:192.168.0.0/16"
//   - "domain:telegram.org"
//   - "ip:149.154.175.53"
func NewTargetMatcher(targets []string) *TargetMatcher {
	tm := &TargetMatcher{}
	for _, t := range targets {
		parts := strings.SplitN(t, ":", 2)
		if len(parts) != 2 {
			continue
		}
		ruleType := strings.ToLower(strings.TrimSpace(parts[0]))
		ruleValue := strings.TrimSpace(parts[1])

		switch ruleType {
		case "cidr":
			_, ipnet, err := net.ParseCIDR(ruleValue)
			if err == nil {
				tm.cidrs = append(tm.cidrs, ipnet)
			}
		case "domain":
			tm.domains = append(tm.domains, strings.ToLower(ruleValue))
		case "ip":
			ip := net.ParseIP(ruleValue)
			if ip != nil {
				tm.ips = append(tm.ips, ip)
			}
		case "geoip", "geosite":
			// TODO: integrate v2router geo database for full geoip/geosite support
		}
	}
	return tm
}

// Match checks if an address is within the cluster target set.
func (tm *TargetMatcher) Match(addr *tunnel.Address) bool {
	if tm == nil {
		return false
	}
	if len(tm.cidrs) == 0 && len(tm.domains) == 0 && len(tm.ips) == 0 {
		return false
	}

	// Check domain match
	if addr.AddressType == tunnel.DomainName && addr.DomainName != "" {
		domain := strings.ToLower(addr.DomainName)
		for _, d := range tm.domains {
			if domain == d || strings.HasSuffix(domain, "."+d) {
				return true
			}
		}
	}

	// Resolve IP for CIDR/IP matching
	ip := addr.IP
	if ip == nil && addr.DomainName != "" {
		resolved, err := net.ResolveIPAddr("ip", addr.DomainName)
		if err != nil {
			return false
		}
		ip = resolved.IP
	}

	if ip == nil {
		return false
	}

	// Check individual IP match
	for _, target := range tm.ips {
		if ip.Equal(target) {
			return true
		}
	}

	// Check CIDR match
	for _, cidr := range tm.cidrs {
		if cidr.Contains(ip) {
			return true
		}
	}

	return false
}

// HasRules returns true if any static target rules are configured.
func (tm *TargetMatcher) HasRules() bool {
	if tm == nil {
		return false
	}
	return len(tm.cidrs) > 0 || len(tm.domains) > 0 || len(tm.ips) > 0
}

// MatchIP checks if an IP string is within the cluster target set.
func (tm *TargetMatcher) MatchIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, target := range tm.ips {
		if ip.Equal(target) {
			return true
		}
	}
	for _, cidr := range tm.cidrs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}
