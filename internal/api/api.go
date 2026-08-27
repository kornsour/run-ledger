// Package api serves the run ledger over HTTP.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kornsour/run-ledger/internal/compare"
	"github.com/kornsour/run-ledger/internal/lineage"
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
	store store.Store
	log   *slog.Logger
	auth  Auth
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
	return srv
}

// Handler returns the routed mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /runs", s.requireAuth(true, s.record))
	mux.HandleFunc("GET /runs", s.requireAuth(false, s.list))
	mux.HandleFunc("GET /runs/{id}", s.requireAuth(false, s.get))
	mux.HandleFunc("GET /compare", s.requireAuth(false, s.compare))
	// /healthz stays unauthenticated so a liveness probe does not need a
	// credential.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
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
			writeErr(w, http.StatusConflict, err)
		default:
			writeErr(w, http.StatusBadRequest, err)
		}
		return
	}
	s.log.Info("recorded run", "run_id", run.RunID, "project", run.Project, "fingerprint", run.Fingerprint)
	writeJSON(w, http.StatusCreated, run)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
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
		writeErr(w, statusFor(err), fmt.Errorf("run a: %w", err))
		return
	}
	b, err := s.store.Get(r.Context(), idB)
	if err != nil {
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
