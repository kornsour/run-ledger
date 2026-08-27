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

func srvWithAuth(t *testing.T, auth Auth) http.Handler {
	t.Helper()
	return New(store.NewMemory(), nil, WithAuth(auth)).Handler()
}

func authed(method, path, token string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(""))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
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

func TestNoTokenConfiguredAllowsEverything(t *testing.T) {
	h := srv(t) // no Auth option: the default, single-user, unauthenticated server.

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authed(http.MethodGet, "/runs", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 with no token configured, got %d: %s", w.Code, w.Body)
	}

	body := `{"project":"p","git_commit":"abc","config_hash":"cfg"}`
	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/runs", strings.NewReader(body))
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201 with no token configured, got %d: %s", w.Code, w.Body)
	}
}

func TestTokenConfiguredRefusesMissingOrWrongOnBothVerbs(t *testing.T) {
	h := srvWithAuth(t, Auth{WriteToken: "write-secret"})

	cases := []struct {
		name   string
		method string
		path   string
		token  string
	}{
		{"read, no token", http.MethodGet, "/runs", ""},
		{"read, wrong token", http.MethodGet, "/runs", "nope"},
		{"write, no token", http.MethodPost, "/runs", ""},
		{"write, wrong token", http.MethodPost, "/runs", "nope"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, authed(c.method, c.path, c.token))
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("want 401, got %d: %s", w.Code, w.Body)
			}
		})
	}

	// The correct token still works on both verbs.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, authed(http.MethodGet, "/runs", "write-secret"))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 with the correct token, got %d: %s", w.Code, w.Body)
	}

	body := `{"project":"p","git_commit":"abc","config_hash":"cfg"}`
	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/runs", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer write-secret")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201 with the correct token, got %d: %s", w.Code, w.Body)
	}
}

func TestReadTokenCannotWrite(t *testing.T) {
	h := srvWithAuth(t, Auth{WriteToken: "write-secret", ReadToken: "read-secret"})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authed(http.MethodGet, "/runs", "read-secret"))
	if w.Code != http.StatusOK {
		t.Fatalf("read token should be able to read, got %d: %s", w.Code, w.Body)
	}

	body := `{"project":"p","git_commit":"abc","config_hash":"cfg"}`
	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/runs", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer read-secret")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("a read token must not be able to write, got %d: %s", w.Code, w.Body)
	}
}

func TestHealthzUnauthenticatedEvenWithTokenConfigured(t *testing.T) {
	h := srvWithAuth(t, Auth{WriteToken: "write-secret"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
}
