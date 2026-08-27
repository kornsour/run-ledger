// Package api serves the run ledger over HTTP.
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kornsour/run-ledger/internal/compare"
	"github.com/kornsour/run-ledger/internal/lineage"
	"github.com/kornsour/run-ledger/internal/metrics"
	"github.com/kornsour/run-ledger/internal/spread"
	"github.com/kornsour/run-ledger/internal/store"
)

// Auth holds the bearer tokens that gate access to the API. A zero-value Auth
// requires no token at all, which is the default: a single-user local ledger
// has no one to authenticate against.
//
// WriteToken grants both reads and writes. ReadToken grants reads only, so a
// dashboard or a CI job can be handed a credential that cannot record runs.
type Auth struct {
	WriteToken string
	ReadToken  string
}

func (a Auth) enabled() bool {
	return a.WriteToken != "" || a.ReadToken != ""
}

// allows reports whether token authorizes the given access. write requests
// need the write token; reads accept either token.
func (a Auth) allows(write bool, token string) bool {
	if a.WriteToken != "" && constantTimeEqual(token, a.WriteToken) {
		return true
	}
	if !write && a.ReadToken != "" && constantTimeEqual(token, a.ReadToken) {
		return true
	}
	return false
}

func constantTimeEqual(a, b string) bool {
	// ConstantTimeCompare itself only compares in constant time when the
	// lengths already match; the length check below is not a secret, so
	// leaking it early does not weaken the comparison.
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Server wires the store to an HTTP mux.
type Server struct {
	store   store.Store
	log     *slog.Logger
	metrics *metrics.Registry
	auth    Auth
}

// Option configures a Server at construction time.
type Option func(*Server)

// WithAuth requires a bearer token on every request except /healthz. Leaving
// this unset (or passing a zero-value Auth) leaves the server unauthenticated.
func WithAuth(a Auth) Option {
	return func(s *Server) { s.auth = a }
}

// New returns a Server. A nil logger discards output.
func New(s store.Store, log *slog.Logger, opts ...Option) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	srv := &Server{store: s, log: log}
	for _, opt := range opts {
		opt(srv)
	}
	if !srv.auth.enabled() {
		log.Warn("running without authentication: any client that can reach this server can write to the ledger; set RUNLEDGER_TOKEN to require a bearer token")
	}
	// The gauge is driven by the store at scrape time, not by a local
	// increment-only counter: recording is idempotent, so a retried record
	// must not inflate it, and once the store is out of process it becomes
	// the only source of truth anyway.
	srv.metrics = metrics.New(func() float64 {
		runs, err := s.List(context.Background(), store.Query{})
		if err != nil {
			return -1
		}
		return float64(len(runs))
	})
	return srv
}

// Handler returns the routed mux, wrapped in request-scoped logging,
// duration/metrics instrumentation, and panic recovery.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /runs", s.requireAuth(true, s.record))
	mux.HandleFunc("GET /runs", s.requireAuth(false, s.list))
	mux.HandleFunc("GET /runs/{id}", s.requireAuth(false, s.get))
	mux.HandleFunc("GET /compare", s.requireAuth(false, s.compare))
	mux.HandleFunc("GET /fingerprints", s.requireAuth(false, s.spreadList))
	mux.HandleFunc("GET /fingerprints/{fingerprint}", s.requireAuth(false, s.spreadOne))
	// /healthz stays unauthenticated so a liveness probe does not need a
	// credential.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	// Ready means the store answers a call, not just that the process is
	// up. That distinction is a no-op today -- the only backend is
	// in-memory -- but becomes real once the store is out of process.
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /metrics", s.serveMetrics)
	return s.instrument(mux)
}

// instrument wraps mux with request-scoped logging, request-duration
// metrics, an echoed X-Request-Id, and panic recovery. A handler panic
// becomes a logged 500 instead of a dropped connection.
func (s *Server) instrument(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := r.Header.Get("X-Request-Id")
		if reqID == "" {
			reqID = newRequestID()
		}
		w.Header().Set("X-Request-Id", reqID)

		// The pattern mux would route this request to (e.g. "GET /runs/{id}"),
		// resolved up front so the metrics and log line report the route
		// rather than the raw, high-cardinality path.
		_, pattern := mux.Handler(r)
		if pattern == "" {
			pattern = "unmatched"
		}

		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic recovered", "route", pattern, "request_id", reqID,
					"err", rec, "stack", string(debug.Stack()))
				if !rw.wroteHeader {
					rw.WriteHeader(http.StatusInternalServerError)
				}
			}
			dur := time.Since(start)
			s.metrics.ObserveRequest(pattern, rw.status, dur.Seconds())
			s.log.Info("request",
				"method", r.Method, "route", pattern, "status", rw.status,
				"duration_ms", float64(dur.Microseconds())/1000, "request_id", reqID)
		}()
		mux.ServeHTTP(rw, r)
	})
}

// statusRecorder captures the status code a handler wrote so the
// instrumentation middleware can observe it after the fact.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (rw *statusRecorder) WriteHeader(code int) {
	if rw.wroteHeader {
		return
	}
	rw.status = code
	rw.wroteHeader = true
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *statusRecorder) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		rw.WriteHeader(http.StatusOK)
	}
	return rw.ResponseWriter.Write(b)
}

func newRequestID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand failing means the platform is broken beyond this
		// request's concern; fall back to a fixed, clearly-synthetic id
		// rather than panicking a request over an unrelated fault.
		return "unavailable"
	}
	return hex.EncodeToString(buf[:])
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.List(r.Context(), store.Query{Limit: 1}); err != nil {
		s.metrics.StoreError("readyz")
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("store not ready: %w", err))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) serveMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = s.metrics.WriteTo(w)
}

// requireAuth wraps next so it only runs once the request carries a bearer
// token Auth allows for the given access. With no token configured at all,
// every request passes through unchecked.
func (s *Server) requireAuth(write bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.enabled() {
			next(w, r)
			return
		}
		token, ok := bearerToken(r)
		if !ok || !s.auth.allows(write, token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="run-ledger"`)
			writeErr(w, http.StatusUnauthorized, errors.New("missing or invalid bearer token"))
			return
		}
		next(w, r)
	}
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	token := strings.TrimPrefix(h, prefix)
	return token, token != ""
}

func (s *Server) record(w http.ResponseWriter, r *http.Request) {
	var run lineage.Run
	dec := json.NewDecoder(r.Body)
	// An unknown field is a typo in a lineage record. Accepting it silently
	// would store a run that claims to describe an experiment it does not.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&run); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now().UTC()
	}
	if run.Status == "" {
		run.Status = lineage.StatusCreated
	}
	// The server computes the fingerprint; a client-supplied one would let a
	// caller assert that two different experiments were the same.
	run.Fingerprint = run.Compute()
	if run.RunID == "" {
		run.RunID = run.Fingerprint[:16] + "-" + strconv.FormatInt(run.StartedAt.UnixNano(), 36)
	}
	if err := s.store.Record(r.Context(), run); err != nil {
		switch {
		case errors.Is(err, store.ErrConflict):
			s.metrics.StoreError("conflict")
			writeErr(w, http.StatusConflict, err)
		default:
			s.metrics.StoreError("invalid")
			writeErr(w, http.StatusBadRequest, err)
		}
		return
	}
	s.metrics.RecordRun(run.Project, string(run.Status))
	s.log.Info("recorded run", "run_id", run.RunID, "project", run.Project, "fingerprint", run.Fingerprint)
	writeJSON(w, http.StatusCreated, run)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		s.metrics.StoreError("not_found")
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		s.metrics.StoreError("internal")
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := parseLimit(q.Get("limit"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	runs, err := s.store.List(r.Context(), store.Query{
		Project:     q.Get("project"),
		GitCommit:   q.Get("git_commit"),
		Fingerprint: q.Get("fingerprint"),
		Status:      lineage.Status(q.Get("status")),
		Device:      q.Get("device"),
		Limit:       limit,
	})
	if err != nil {
		s.metrics.StoreError("internal")
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs, "count": len(runs)})
}

func (s *Server) compare(w http.ResponseWriter, r *http.Request) {
	idA, idB := r.URL.Query().Get("a"), r.URL.Query().Get("b")
	if idA == "" || idB == "" {
		writeErr(w, http.StatusBadRequest, errors.New("both a and b are required"))
		return
	}
	a, err := s.store.Get(r.Context(), idA)
	if err != nil {
		s.metrics.StoreError(errKind(err))
		writeErr(w, statusFor(err), fmt.Errorf("run a: %w", err))
		return
	}
	b, err := s.store.Get(r.Context(), idB)
	if err != nil {
		s.metrics.StoreError(errKind(err))
		writeErr(w, statusFor(err), fmt.Errorf("run b: %w", err))
		return
	}
	res := compare.Runs(a, b)
	writeJSON(w, http.StatusOK, map[string]any{
		"result":         res,
		"unattributable": res.Unattributable(),
	})
}

// spreadList answers "which experiments in this project reproduce worst?" --
// every fingerprint with more than one recorded run, ranked by the widest
// relative metric spread. project is optional; omitted, it ranks across
// every project the store holds.
func (s *Server) spreadList(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.List(r.Context(), store.Query{Project: r.URL.Query().Get("project")})
	if err != nil {
		s.metrics.StoreError("internal")
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var groups []spread.Group
	for _, g := range spread.Compute(runs) {
		if g.Count > 1 {
			groups = append(groups, g)
		}
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Widest() > groups[j].Widest() })
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups, "count": len(groups)})
}

// spreadOne answers "how much do this experiment's own repeats vary?" for
// one fingerprint, including a group of size one -- reported as no repeats
// rather than a misleadingly perfect standard deviation of zero.
func (s *Server) spreadOne(w http.ResponseWriter, r *http.Request) {
	fp := r.PathValue("fingerprint")
	runs, err := s.store.List(r.Context(), store.Query{Fingerprint: fp})
	if err != nil {
		s.metrics.StoreError("internal")
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if len(runs) == 0 {
		s.metrics.StoreError("not_found")
		writeErr(w, http.StatusNotFound, fmt.Errorf("no run recorded with fingerprint %q", fp))
		return
	}
	writeJSON(w, http.StatusOK, spread.One(fp, runs))
}

func parseLimit(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("limit must be a non-negative integer, got %q", s)
	}
	return n, nil
}

func statusFor(err error) int {
	if errors.Is(err, store.ErrNotFound) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

func errKind(err error) string {
	if errors.Is(err, store.ErrNotFound) {
		return "not_found"
	}
	return "internal"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
