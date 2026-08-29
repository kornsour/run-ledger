// Package api serves the run ledger over HTTP.
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
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

// DefaultListLimit is the page size GET /runs uses when a request does not
// specify limit.
const DefaultListLimit = 50

// MaxListLimit is the largest page GET /runs will ever return, regardless of
// what limit a request asks for. Without a ceiling, a client (or the size of
// the ledger itself) decides how large a response the server hands back.
const MaxListLimit = 500

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
		// Query{} with no Limit is intentionally unbounded here -- this is
		// the total ledger size, not a page of it. GET /runs's own default
		// and maximum limit live at the HTTP handler, not in the store.
		page, err := s.List(context.Background(), store.Query{})
		if err != nil {
			return -1
		}
		return float64(len(page.Runs))
	})
	return srv
}

// route is one entry in the server's route table: the exact pattern
// ServeMux registers it under (e.g. "POST /runs"), paired with its handler.
type route struct {
	pattern string
	handler http.HandlerFunc
}

// routes is the server's route table -- the single source of truth for what
// this server serves. Handler builds the mux from it, and spec_test.go
// checks it against docs/openapi.yaml, so an endpoint added or removed here
// without a matching spec change fails CI rather than shipping undocumented.
func (s *Server) routes() []route {
	return []route{
		{"POST /runs", s.requireAuth(true, s.record)},
		{"PATCH /runs/{id}", s.requireAuth(true, s.update)},
		{"GET /runs", s.requireAuth(false, s.list)},
		{"GET /runs/{id}", s.requireAuth(false, s.get)},
		{"GET /compare", s.requireAuth(false, s.compare)},
		{"GET /fingerprints", s.requireAuth(false, s.spreadList)},
		{"GET /fingerprints/{fingerprint}", s.requireAuth(false, s.spreadOne)},
		// /healthz stays unauthenticated so a liveness probe does not need a
		// credential.
		{"GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
		}},
		// Ready means the store answers a call, not just that the process is
		// up. That distinction is a no-op today -- the only backend is
		// in-memory -- but becomes real once the store is out of process.
		{"GET /readyz", s.readyz},
		{"GET /metrics", s.serveMetrics},
	}
}

// Handler returns the routed mux, wrapped in request-scoped logging,
// duration/metrics instrumentation, and panic recovery.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		mux.HandleFunc(rt.pattern, rt.handler)
	}
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

// patchRequest is the PATCH /runs/{id} body. Every field is a pointer (or,
// for the two map fields, a nil-vs-set map) so the handler can tell "the
// caller did not mention this" apart from "the caller set this to its zero
// value" -- a plain lineage.Run cannot make that distinction, and a PATCH
// endpoint runs on it.
//
// The identity fields are listed here too, even though PATCH exists to
// carry provenance: a client that sends one is not silently ignored, it is
// checked against the stored run and rejected with a conflict if it
// differs. Leaving them out of this struct entirely would turn an attempt
// to rewrite a run's identity into a same-looking "unknown field" 400
// instead of the 409 that states what actually happened.
type patchRequest struct {
	Project        *string           `json:"project"`
	GitCommit      *string           `json:"git_commit"`
	GitDirty       *bool             `json:"git_dirty"`
	ConfigHash     *string           `json:"config_hash"`
	DatasetVersion *string           `json:"dataset_version"`
	ModelVersion   *string           `json:"model_version"`
	Seed           *int64            `json:"seed"`
	Params         map[string]string `json:"params"`

	Status           *lineage.Status    `json:"status"`
	EndedAt          *time.Time         `json:"ended_at"`
	CheckpointURI    *string            `json:"checkpoint_uri"`
	Host             *string            `json:"host"`
	Device           *string            `json:"device"`
	FrameworkVersion *string            `json:"framework_version"`
	Metrics          map[string]float64 `json:"metrics"`
}

func (s *Server) update(w http.ResponseWriter, r *http.Request) {
	var req patchRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	p := store.Patch{
		Project: req.Project, GitCommit: req.GitCommit, GitDirty: req.GitDirty,
		ConfigHash: req.ConfigHash, DatasetVersion: req.DatasetVersion,
		ModelVersion: req.ModelVersion, Seed: req.Seed, Params: req.Params,
		Status: req.Status, EndedAt: req.EndedAt, CheckpointURI: req.CheckpointURI,
		Host: req.Host, Device: req.Device, FrameworkVersion: req.FrameworkVersion,
		Metrics: req.Metrics,
	}
	run, err := s.store.Update(r.Context(), r.PathValue("id"), p)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			s.metrics.StoreError("not_found")
			writeErr(w, http.StatusNotFound, err)
		case errors.Is(err, store.ErrConflict):
			s.metrics.StoreError("conflict")
			writeErr(w, http.StatusConflict, err)
		default:
			s.metrics.StoreError("invalid")
			writeErr(w, http.StatusBadRequest, err)
		}
		return
	}
	if req.Status != nil {
		s.metrics.RecordRun(run.Project, string(run.Status))
	}
	s.log.Info("updated run", "run_id", run.RunID, "project", run.Project, "status", run.Status)
	writeJSON(w, http.StatusOK, run)
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
	var after *store.Cursor
	if raw := q.Get("cursor"); raw != "" {
		c, err := decodeCursor(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		after = &c
	}
	page, err := s.store.List(r.Context(), store.Query{
		Project:     q.Get("project"),
		GitCommit:   q.Get("git_commit"),
		Fingerprint: q.Get("fingerprint"),
		Status:      lineage.Status(q.Get("status")),
		Device:      q.Get("device"),
		Limit:       limit,
		After:       after,
	})
	if err != nil {
		s.metrics.StoreError("internal")
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	resp := map[string]any{"runs": page.Runs, "count": len(page.Runs), "limit": limit}
	// next_cursor is present only when more rows may follow this page --
	// its absence is how a client knows the traversal is done, not an empty
	// string it has to special-case.
	if page.Next != nil {
		resp["next_cursor"] = encodeCursor(*page.Next)
	}
	writeJSON(w, http.StatusOK, resp)
}

// encodeCursor and decodeCursor make a store.Cursor an opaque token safe to
// hand to a client and accept back. The wire format (a version tag, the
// start-time in nanoseconds, and the run id, colon-separated and
// base64url-encoded) is not part of the API contract -- callers must treat
// the string as opaque -- but is versioned so a future change to what a
// cursor encodes cannot be silently misread as the old format.
const cursorVersion = "v1"

func encodeCursor(c store.Cursor) string {
	raw := fmt.Sprintf("%s:%d:%s", cursorVersion, c.StartedAt.UnixNano(), c.RunID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (store.Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return store.Cursor{}, fmt.Errorf("invalid cursor")
	}
	version, rest, ok := strings.Cut(string(raw), ":")
	if !ok || version != cursorVersion {
		return store.Cursor{}, fmt.Errorf("invalid or unsupported cursor")
	}
	nsStr, runID, ok := strings.Cut(rest, ":")
	if !ok || runID == "" {
		return store.Cursor{}, fmt.Errorf("invalid cursor")
	}
	ns, err := strconv.ParseInt(nsStr, 10, 64)
	if err != nil {
		return store.Cursor{}, fmt.Errorf("invalid cursor")
	}
	return store.Cursor{StartedAt: time.Unix(0, ns).UTC(), RunID: runID}, nil
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
	// Limit: 0 is intentionally unbounded here -- spread has to see every
	// run for a fingerprint to report its true spread, not just one page.
	page, err := s.store.List(r.Context(), store.Query{Project: r.URL.Query().Get("project")})
	if err != nil {
		s.metrics.StoreError("internal")
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var groups []spread.Group
	for _, g := range spread.Compute(page.Runs) {
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
	page, err := s.store.List(r.Context(), store.Query{Fingerprint: fp})
	if err != nil {
		s.metrics.StoreError("internal")
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if len(page.Runs) == 0 {
		s.metrics.StoreError("not_found")
		writeErr(w, http.StatusNotFound, fmt.Errorf("no run recorded with fingerprint %q", fp))
		return
	}
	writeJSON(w, http.StatusOK, spread.One(fp, page.Runs))
}

// parseLimit resolves the effective page size for GET /runs: DefaultListLimit
// when the request specifies none, the request's own value when it is a
// valid positive integer at or under MaxListLimit, and MaxListLimit itself
// when the request asks for more than that. There is deliberately no way to
// request "unlimited" -- that is the behavior this cap exists to remove.
func parseLimit(s string) (int, error) {
	if s == "" {
		return DefaultListLimit, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("limit must be a positive integer, got %q", s)
	}
	if n > MaxListLimit {
		n = MaxListLimit
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
