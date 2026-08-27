// Package metrics collects the server's self-metrics and renders them in
// Prometheus exposition format.
//
// It is hand-rolled rather than built on a client library: run-ledger ships
// as one binary with nothing to fetch to try it, and a handful of counters
// and a histogram don't justify breaking that for every clone.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// requestBuckets are the upper bounds (seconds) of the request-duration
// histogram, chosen for a service whose handlers are in-memory map lookups:
// mostly sub-millisecond, with headroom out to one second for a slow client
// or a GC pause.
var requestBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1}

// Registry holds the counters and histogram this server reports about
// itself. The zero value is not usable; construct with New.
type Registry struct {
	mu sync.Mutex

	runsRecorded map[[2]string]uint64   // {project, status} -> count
	storeErrors  map[string]uint64      // kind -> count
	reqCount     map[[2]string]uint64   // {route, code} -> count
	reqSum       map[[2]string]float64  // {route, code} -> seconds
	reqBuckets   map[[2]string][]uint64 // {route, code} -> cumulative count per requestBuckets entry

	// runsGauge reports the store's current run count at scrape time. It is
	// a callback rather than a value this package tracks itself: the count
	// must reflect the store's own state (recording is idempotent, so a
	// local increment-only counter would overcount a retried record), and
	// once the store is out of process it is also the only source of truth.
	runsGauge func() float64
}

// New returns an empty Registry. runsGauge is called on every scrape of
// runledger_runs and should be cheap.
func New(runsGauge func() float64) *Registry {
	return &Registry{
		runsRecorded: make(map[[2]string]uint64),
		storeErrors:  make(map[string]uint64),
		reqCount:     make(map[[2]string]uint64),
		reqSum:       make(map[[2]string]float64),
		reqBuckets:   make(map[[2]string][]uint64),
		runsGauge:    runsGauge,
	}
}

// RecordRun increments runledger_runs_recorded_total for a project/status pair.
func (r *Registry) RecordRun(project string, status string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runsRecorded[[2]string{project, status}]++
}

// StoreError increments runledger_store_errors_total for an error kind
// (e.g. "not_found", "conflict", "internal").
func (r *Registry) StoreError(kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.storeErrors[kind]++
}

// ObserveRequest records one HTTP request's duration into
// runledger_request_duration_seconds for a route/status-code pair.
func (r *Registry) ObserveRequest(route string, code int, seconds float64) {
	key := [2]string{route, strconv.Itoa(code)}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqCount[key]++
	r.reqSum[key] += seconds
	buckets, ok := r.reqBuckets[key]
	if !ok {
		buckets = make([]uint64, len(requestBuckets))
		r.reqBuckets[key] = buckets
	}
	for i, bound := range requestBuckets {
		if seconds <= bound {
			buckets[i]++
		}
	}
}

// WriteTo renders every metric in Prometheus text exposition format.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var b strings.Builder

	writeHeader(&b, "runledger_runs_recorded_total",
		"Total number of runs recorded, by project and status.", "counter")
	for _, key := range sortedKeys2(r.runsRecorded) {
		fmt.Fprintf(&b, "runledger_runs_recorded_total{project=%q,status=%q} %d\n",
			key[0], key[1], r.runsRecorded[key])
	}

	writeHeader(&b, "runledger_store_errors_total",
		"Total number of store errors, by kind.", "counter")
	for _, kind := range sortedKeys1(r.storeErrors) {
		fmt.Fprintf(&b, "runledger_store_errors_total{kind=%q} %d\n", kind, r.storeErrors[kind])
	}

	writeHeader(&b, "runledger_request_duration_seconds",
		"HTTP request duration in seconds, by route and status code.", "histogram")
	for _, key := range sortedKeys2(r.reqCount) {
		route, code := key[0], key[1]
		buckets := r.reqBuckets[key]
		for i, bound := range requestBuckets {
			fmt.Fprintf(&b, "runledger_request_duration_seconds_bucket{route=%q,code=%q,le=%q} %d\n",
				route, code, formatFloat(bound), buckets[i])
		}
		fmt.Fprintf(&b, "runledger_request_duration_seconds_bucket{route=%q,code=%q,le=\"+Inf\"} %d\n",
			route, code, r.reqCount[key])
		fmt.Fprintf(&b, "runledger_request_duration_seconds_sum{route=%q,code=%q} %s\n",
			route, code, formatFloat(r.reqSum[key]))
		fmt.Fprintf(&b, "runledger_request_duration_seconds_count{route=%q,code=%q} %d\n",
			route, code, r.reqCount[key])
	}

	writeHeader(&b, "runledger_runs", "Current number of runs held by the store.", "gauge")
	fmt.Fprintf(&b, "runledger_runs %s\n", formatFloat(r.runsGauge()))

	n, err := io.WriteString(w, b.String())
	return int64(n), err
}

func writeHeader(b *strings.Builder, name, help, typ string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func sortedKeys1(m map[string]uint64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeys2(m map[[2]string]uint64) [][2]string {
	keys := make([][2]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	return keys
}
