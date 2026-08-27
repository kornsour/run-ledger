// Package api serves the run ledger over HTTP.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/kornsour/run-ledger/internal/compare"
	"github.com/kornsour/run-ledger/internal/lineage"
	"github.com/kornsour/run-ledger/internal/metrics"
	"github.com/kornsour/run-ledger/internal/store"
)

// Server wires the store to an HTTP mux.
type Server struct {
	store   store.Store
	log     *slog.Logger
	metrics *metrics.Registry
}

// New returns a Server. A nil logger discards output.
func New(s store.Store, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	srv := &Server{store: s, log: log}
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
	mux.HandleFunc("POST /runs", s.record)
	mux.HandleFunc("GET /runs", s.list)
	mux.HandleFunc("GET /runs/{id}", s.get)
	mux.HandleFunc("GET /compare", s.compare)
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
