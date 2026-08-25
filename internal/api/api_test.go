package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kornsour/run-ledger/internal/store"
)

func srv(t *testing.T) http.Handler {
	t.Helper()
	return New(store.NewMemory(), nil).Handler()
}

func post(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/runs", strings.NewReader(body)))
	return w
}

func TestRecordAssignsFingerprintAndID(t *testing.T) {
	w := post(t, srv(t), `{"project":"p","git_commit":"abc","config_hash":"cfg"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["fingerprint"] == "" || got["fingerprint"] == nil {
		t.Fatal("no fingerprint assigned")
	}
	if got["run_id"] == "" || got["run_id"] == nil {
		t.Fatal("no run id assigned")
	}
}

func TestClientCannotDictateTheFingerprint(t *testing.T) {
	// A caller asserting its own fingerprint could claim two different
	// experiments were the same run.
	w := post(t, srv(t), `{"project":"p","git_commit":"abc","config_hash":"cfg","fingerprint":"deadbeef"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["fingerprint"] == "deadbeef" {
		t.Fatal("server accepted a client-supplied fingerprint")
	}
}

func TestUnknownFieldIsRejected(t *testing.T) {
	w := post(t, srv(t), `{"project":"p","git_commit":"abc","comit_hash":"typo"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a typo'd field must be refused, got %d: %s", w.Code, w.Body)
	}
}

func TestInvalidRunIsRefused(t *testing.T) {
	w := post(t, srv(t), `{"project":"p"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a run with no commit, got %d", w.Code)
	}
}

func TestDirtyTreeWithoutConfigHashIsRefused(t *testing.T) {
	w := post(t, srv(t), `{"project":"p","git_commit":"abc","git_dirty":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body)
	}
}

func TestGetUnknownRunIs404(t *testing.T) {
	w := httptest.NewRecorder()
	srv(t).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/runs/nope", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestCompareRequiresBothIDs(t *testing.T) {
	w := httptest.NewRecorder()
	srv(t).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/compare?a=x", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestCompareFlagsUnattributableDifference(t *testing.T) {
	h := srv(t)
	idOf := func(body string) string {
		w := post(t, h, body)
		if w.Code != http.StatusCreated {
			t.Fatalf("setup failed: %d %s", w.Code, w.Body)
		}
		var got map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		return got["run_id"].(string)
	}
	a := idOf(`{"project":"p","git_commit":"abc","config_hash":"cfg","seed":1,"metrics":{"loss":0.1}}`)
	b := idOf(`{"project":"p","git_commit":"abc","config_hash":"cfg","seed":1,"metrics":{"loss":0.4}}`)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/compare?a="+a+"&b="+b, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	var got struct {
		Unattributable bool `json:"unattributable"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.Unattributable {
		t.Fatalf("same experiment with different loss must be flagged: %s", w.Body)
	}
}

func TestBadLimitIsRejected(t *testing.T) {
	w := httptest.NewRecorder()
	srv(t).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/runs?limit=-3", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestHealthz(t *testing.T) {
	w := httptest.NewRecorder()
	srv(t).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
}
