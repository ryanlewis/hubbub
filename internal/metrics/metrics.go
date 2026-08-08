// Package metrics: hand-rendered Prometheus exposition (~name{label="x"}
// value lines) — no client library, stdlib-only. Counters always increment;
// config only controls whether the ops listener exposes them. Label values
// are channel ids and outcomes only — never caller ids or payload content.
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type Metrics struct {
	mu         sync.Mutex
	start      time.Time
	requests   map[string]uint64    // by outcome
	deliveries map[[2]string]uint64 // by channel, outcome
	authFails  uint64
	// adminChanges is by action; nil until the dashboard is used, so a hub
	// without one exposes no admin series at all.
	adminChanges map[string]uint64
	// recentReads is by outcome; nil until the delivery-log endpoint is used.
	// Reads get their own series rather than sharing the request counter: a
	// poll is not a notification, and counting one as the other would put a
	// dashboard's traffic into the numbers an operator sizes the rate cap from.
	recentReads map[string]uint64
}

func New() *Metrics {
	return &Metrics{
		start:      time.Now(),
		requests:   make(map[string]uint64),
		deliveries: make(map[[2]string]uint64),
	}
}

func (m *Metrics) Request(outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[outcome]++
}

func (m *Metrics) Delivery(channel, outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deliveries[[2]string{channel, outcome}]++
}

func (m *Metrics) AuthFailure() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.authFails++
}

// AdminChange counts a config change made through the dashboard. The label is
// the action name — a fixed, code-defined set — so the endpoint stays free of
// caller ids and payload content like every other label here.
func (m *Metrics) AdminChange(action string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.adminChanges == nil {
		m.adminChanges = make(map[string]uint64)
	}
	m.adminChanges[action]++
}

// RecentRead counts a delivery-log read by outcome — a fixed, code-defined set
// (`ok`, `unauthorized`, `forbidden`, `rejected`, `error`), so this stays free
// of caller ids like every other label here.
func (m *Metrics) RecentRead(outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.recentReads == nil {
		m.recentReads = make(map[string]uint64)
	}
	m.recentReads[outcome]++
}

func (m *Metrics) Render() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var b strings.Builder
	b.WriteString("# TYPE notify_requests_total counter\n")
	for _, k := range sortedKeys(m.requests) {
		fmt.Fprintf(&b, "notify_requests_total{outcome=%q} %d\n", k, m.requests[k])
	}
	b.WriteString("# TYPE notify_deliveries_total counter\n")
	keys := make([][2]string, 0, len(m.deliveries))
	for k := range m.deliveries {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	for _, k := range keys {
		fmt.Fprintf(&b, "notify_deliveries_total{channel=%q,outcome=%q} %d\n", k[0], k[1], m.deliveries[k])
	}
	b.WriteString("# TYPE notify_auth_failures_total counter\n")
	fmt.Fprintf(&b, "notify_auth_failures_total %d\n", m.authFails)
	if len(m.recentReads) > 0 {
		b.WriteString("# TYPE notify_recent_reads_total counter\n")
		for _, k := range sortedKeys(m.recentReads) {
			fmt.Fprintf(&b, "notify_recent_reads_total{outcome=%q} %d\n", k, m.recentReads[k])
		}
	}
	if len(m.adminChanges) > 0 {
		b.WriteString("# TYPE notify_admin_changes_total counter\n")
		for _, k := range sortedKeys(m.adminChanges) {
			fmt.Fprintf(&b, "notify_admin_changes_total{action=%q} %d\n", k, m.adminChanges[k])
		}
	}
	b.WriteString("# TYPE process_start_time_seconds gauge\n")
	fmt.Fprintf(&b, "process_start_time_seconds %d\n", m.start.Unix())
	return b.String()
}

func (m *Metrics) Uptime() time.Duration {
	return time.Since(m.start)
}

func sortedKeys(mp map[string]uint64) []string {
	ks := make([]string, 0, len(mp))
	for k := range mp {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
