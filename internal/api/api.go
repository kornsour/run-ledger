// Package api serves the run ledger over HTTP.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/kornsour/run-ledger/internal/compare"
	"github.com/kornsour/run-ledger/internal/lineage"
	"github.com/kornsour/run-ledger/internal/store"
)

// Server wires the store to an HTTP mux.
type Server struct {
	store store.Store
	log   *slog.Logger
}

// New returns a Server. A nil logger discards output.
func New(s store.Store, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Server{store: s, log: log}
}

// Handler returns the routed mux.
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
	return mux
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
