package connmonitor

import (
	"sort"
	"sync/atomic"
)

// perUserMetrics aggregates counters for one authenticated user (identified
// by the trojan password hash). The hot path is lock-free: atomic counters
// only.
//
// The process-global user map is capped at Metrics.userCap entries.
// When the cap is hit, excess users are merged into a synthetic
// "__other__" bucket so memory stays bounded under hash-flood attacks.
type PerUserMetrics struct {
	hash           string
	connsTotal     atomic.Uint64
	authFailTotal  atomic.Uint64
	bytesUpTotal   atomic.Uint64
	bytesDownTotal atomic.Uint64
}

// RecordConn increments the connection counter for this user.
func (u *PerUserMetrics) RecordConn() {
	if u == nil {
		return
	}
	u.connsTotal.Add(1)
}

// RecordAuthFail increments the auth-failure counter.
func (u *PerUserMetrics) RecordAuthFail() {
	if u == nil {
		return
	}
	u.authFailTotal.Add(1)
}

// RecordUp / RecordDown bump per-user byte counters.
func (u *PerUserMetrics) RecordUp(n int64) {
	if u == nil || n <= 0 {
		return
	}
	u.bytesUpTotal.Add(uint64(n))
}

func (u *PerUserMetrics) RecordDown(n int64) {
	if u == nil || n <= 0 {
		return
	}
	u.bytesDownTotal.Add(uint64(n))
}

// UserSample is the JSON-serialisable view of one user for the dashboard.
type UserSample struct {
	Hash      string `json:"hash"`
	Conns     uint64 `json:"conns_total"`
	AuthFails uint64 `json:"auth_fail_total"`
	BytesUp   uint64 `json:"bytes_up_total"`
	BytesDown uint64 `json:"bytes_down_total"`
}

// Snapshot returns a point-in-time UserSample.
func (u *PerUserMetrics) Snapshot() UserSample {
	if u == nil {
		return UserSample{}
	}
	return UserSample{
		Hash:      u.hash,
		Conns:     u.connsTotal.Load(),
		AuthFails: u.authFailTotal.Load(),
		BytesUp:   u.bytesUpTotal.Load(),
		BytesDown: u.bytesDownTotal.Load(),
	}
}

// userCapDefault is used when Metrics is constructed without an explicit cap.
const userCapDefault = 256

// userOtherKey is the synthetic bucket hash for overflow.
const userOtherKey = "__other__"

// SetUserCap adjusts the per-user map capacity at startup.
// Values <= 0 disable per-user accounting entirely.
func (m *Metrics) SetUserCap(n int) {
	m.usersMu.Lock()
	m.userCap = n
	m.usersMu.Unlock()
}

// GetUser returns the per-user metrics for hash, lazily creating the
// entry when capacity permits. When the map is full and hash is new,
// the returned *perUserMetrics points at the synthetic "__other__"
// bucket. When userCap <= 0, GetUser returns nil so that hot paths
// can cheaply short-circuit.
func (m *Metrics) GetUser(hash string) *PerUserMetrics {
	if m == nil {
		return nil
	}
	m.usersMu.Lock()
	defer m.usersMu.Unlock()
	if m.userCap <= 0 {
		return nil
	}
	if m.users == nil {
		m.users = make(map[string]*PerUserMetrics, m.userCap)
	}
	if u, ok := m.users[hash]; ok {
		return u
	}
	if len(m.users) >= m.userCap {
		hash = userOtherKey
		if u, ok := m.users[hash]; ok {
			return u
		}
	}
	u := &PerUserMetrics{hash: hash}
	m.users[hash] = u
	return u
}

// SnapshotUsers returns the top-N users sorted by BytesDown descending.
func (m *Metrics) SnapshotUsers(limit int) []UserSample {
	if m == nil || limit <= 0 {
		return nil
	}
	m.usersMu.Lock()
	all := make([]UserSample, 0, len(m.users))
	for _, u := range m.users {
		all = append(all, u.Snapshot())
	}
	m.usersMu.Unlock()
	sort.Slice(all, func(i, j int) bool {
		return all[i].BytesDown > all[j].BytesDown
	})
	if limit < len(all) {
		all = all[:limit]
	}
	return all
}
