package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kornsour/run-ledger/internal/metrics"
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
	for _, limit := range []string{"-3", "0", "not-a-number"} {
		w := httptest.NewRecorder()
		srv(t).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/runs?limit="+limit, nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("limit=%s: want 400, got %d", limit, w.Code)
		}
	}
}

func TestBadCursorIsRejected(t *testing.T) {
	for _, cursor := range []string{"not-base64url!!", "aGVsbG8", ""} {
		if cursor == "" {
			continue // empty cursor means "from the top", not an error
		}
		w := httptest.NewRecorder()
		srv(t).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/runs?cursor="+cursor, nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("cursor=%s: want 400, got %d: %s", cursor, w.Code, w.Body)
		}
	}
}

func TestListDefaultsAndCapsLimit(t *testing.T) {
	h := srv(t)
	for i := 0; i < 3; i++ {
		w := post(t, h, fmt.Sprintf(`{"project":"p","git_commit":"c%d","config_hash":"cfg"}`, i))
		if w.Code != http.StatusCreated {
			t.Fatalf("setup failed: %d %s", w.Code, w.Body)
		}
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/runs?project=p", nil))
	var got struct {
		Runs  []map[string]any `json:"runs"`
		Count int              `json:"count"`
		Limit int              `json:"limit"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Limit != DefaultListLimit {
		t.Fatalf("want the effective limit echoed as %d, got %d", DefaultListLimit, got.Limit)
	}
	if got.Count != 3 {
		t.Fatalf("want 3 runs under the default limit, got %d", got.Count)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/runs?project=p&limit=%d", MaxListLimit+1000), nil))
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Limit != MaxListLimit {
		t.Fatalf("a request over MaxListLimit must be clamped, got effective limit %d", got.Limit)
	}
}

func TestListCursorPagesWithoutSkippingOrRepeating(t *testing.T) {
	h := srv(t)
	const n = 5
	for i := 0; i < n; i++ {
		w := post(t, h, fmt.Sprintf(`{"project":"p","git_commit":"c%d","config_hash":"cfg"}`, i))
		if w.Code != http.StatusCreated {
			t.Fatalf("setup failed: %d %s", w.Code, w.Body)
		}
	}

	type page struct {
		Runs       []map[string]any `json:"runs"`
		NextCursor string           `json:"next_cursor"`
	}
	seen := map[string]bool{}
	cursor := ""
	for i := 0; i < n+1; i++ { // +1 guards against a cursor that never terminates
		url := "/runs?project=p&limit=2"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("page %d: want 200, got %d: %s", i, w.Code, w.Body)
		}
		var p page
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		for _, r := range p.Runs {
			id := r["run_id"].(string)
			if seen[id] {
				t.Fatalf("run %q was returned on more than one page", id)
			}
			seen[id] = true
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if len(seen) != n {
		t.Fatalf("want all %d runs visited across pages, got %d: %v", n, len(seen), seen)
	}
}

func TestHealthz(t *testing.T) {
	w := httptest.NewRecorder()
	srv(t).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
}

func TestReadyzOKWhenStoreAnswers(t *testing.T) {
	w := httptest.NewRecorder()
	srv(t).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
}

func TestRequestIDIsEchoedAndGeneratedWhenAbsent(t *testing.T) {
	h := srv(t)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Header().Get("X-Request-Id") == "" {
		t.Fatal("no X-Request-Id generated for a request that sent none")
	}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-Id", "caller-supplied-id")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if got := w.Header().Get("X-Request-Id"); got != "caller-supplied-id" {
		t.Fatalf("want caller's request id echoed back, got %q", got)
	}
}

func TestPanicRecoversInto500(t *testing.T) {
	s := &Server{store: store.NewMemory(), log: slog.New(slog.DiscardHandler)}
	s.metrics = metrics.New(func() float64 { return 0 })
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	})
	h := s.instrument(mux)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 after a handler panic, got %d", w.Code)
	}
}

func TestMetricsEndpointExposesRunsRecordedCounter(t *testing.T) {
	h := srv(t)
	post(t, h, `{"project":"demo","git_commit":"abc","config_hash":"cfg","status":"succeeded"}`)
	post(t, h, `{"project":"demo","git_commit":"def","config_hash":"cfg","status":"succeeded"}`)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	body := w.Body.String()
	want := `runledger_runs_recorded_total{project="demo",status="succeeded"} 2`
	if !strings.Contains(body, want) {
		t.Fatalf("counter did not increment as expected; want to find %q in:\n%s", want, body)
	}
	if !strings.Contains(body, "runledger_runs 2") {
		t.Fatalf("runs gauge did not reflect the store; got:\n%s", body)
	}
}

func TestMetricsEndpointReportsStoreConflict(t *testing.T) {
	h := srv(t)
	body := `{"project":"p","git_commit":"abc","config_hash":"cfg","run_id":"fixed-id"}`
	post(t, h, body) // first record succeeds

	w := httptest.NewRecorder()
	// Same run id, different content -> conflict.
	req := httptest.NewRequest(http.MethodPost, "/runs",
		strings.NewReader(`{"project":"p","git_commit":"other","config_hash":"cfg","run_id":"fixed-id"}`))
	h.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", w.Code, w.Body)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(w.Body.String(), `runledger_store_errors_total{kind="conflict"} 1`) {
		t.Fatalf("store error counter did not record the conflict; got:\n%s", w.Body.String())
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

func patch(t *testing.T, h http.Handler, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, "/runs/"+id, strings.NewReader(body)))
	return w
}

func TestUpdateWalksCreatedToSucceeded(t *testing.T) {
	h := srv(t)
	w := post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg"}`)
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	id := created["run_id"].(string)

	w = patch(t, h, id, `{"status":"running"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("created -> running: want 200, got %d: %s", w.Code, w.Body)
	}

	w = patch(t, h, id, `{"status":"succeeded","ended_at":"2999-01-01T00:00:00Z","metrics":{"loss":0.1}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("running -> succeeded: want 200, got %d: %s", w.Code, w.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["status"] != "succeeded" {
		t.Fatalf("want status succeeded, got %v", got["status"])
	}
	metrics, _ := got["metrics"].(map[string]any)
	if metrics["loss"] != 0.1 {
		t.Fatalf("want loss=0.1 in the response, got %v", got["metrics"])
	}
}

func TestUpdateUnknownRunIs404(t *testing.T) {
	w := patch(t, srv(t), "nope", `{"status":"running"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body)
	}
}

func TestUpdateRejectsIdentityChange(t *testing.T) {
	h := srv(t)
	w := post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg"}`)
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	id := created["run_id"].(string)

	w = patch(t, h, id, `{"git_commit":"different"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("changing an identity field must be 409, got %d: %s", w.Code, w.Body)
	}
}

func TestUpdateRejectsIllegalTransition(t *testing.T) {
	h := srv(t)
	w := post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg"}`) // starts at "created"
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	id := created["run_id"].(string)

	w = patch(t, h, id, `{"status":"succeeded"}`) // created -> succeeded skips running
	if w.Code != http.StatusConflict {
		t.Fatalf("an illegal transition must be 409, got %d: %s", w.Code, w.Body)
	}
}

func TestUpdateRejectsChangeToTerminalRun(t *testing.T) {
	h := srv(t)
	w := post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg","status":"succeeded"}`)
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	id := created["run_id"].(string)

	w = patch(t, h, id, `{"checkpoint_uri":"s3://bucket/ckpt"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("a patch to a terminal run must be 409, got %d: %s", w.Code, w.Body)
	}
}

func TestUpdateUnknownFieldIsRejected(t *testing.T) {
	h := srv(t)
	w := post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg"}`)
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	id := created["run_id"].(string)

	w = patch(t, h, id, `{"statuz":"running"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a typo'd field must be refused, got %d: %s", w.Code, w.Body)
	}
}

func TestUpdateRequiresWriteToken(t *testing.T) {
	h := srvWithAuth(t, Auth{WriteToken: "write-secret", ReadToken: "read-secret"})
	req := httptest.NewRequest(http.MethodPatch, "/runs/whatever", strings.NewReader(`{"status":"running"}`))
	req.Header.Set("Authorization", "Bearer read-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("a read token must not be able to PATCH, got %d: %s", w.Code, w.Body)
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
