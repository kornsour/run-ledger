package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	req := httptest.NewRequest(http.MethodPost, "/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(w, req)
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
	// unattributable sits directly on the response, a sibling of a/b/
	// same_experiment/fields -- not nested under a "result" wrapper key.
	var got struct {
		A              string `json:"a"`
		SameExperiment bool   `json:"same_experiment"`
		Unattributable bool   `json:"unattributable"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.A != a {
		t.Fatalf("want a to be top-level, got %s", w.Body)
	}
	if !got.SameExperiment {
		t.Fatalf("want same_experiment true, got %s", w.Body)
	}
	if !got.Unattributable {
		t.Fatalf("same experiment with different loss must be flagged: %s", w.Body)
	}

	var rawShape map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &rawShape)
	if _, ok := rawShape["result"]; ok {
		t.Fatalf("response must not nest under a result key: %s", w.Body)
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

func TestSinceUntilNarrowListing(t *testing.T) {
	h := srv(t)
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	rec := func(id string, at time.Time) {
		body := fmt.Sprintf(`{"project":"p","git_commit":"c","config_hash":"cfg","run_id":%q,"started_at":%q}`,
			id, at.Format(time.RFC3339))
		w := post(t, h, body)
		if w.Code != http.StatusCreated {
			t.Fatalf("setup failed for %s: %d %s", id, w.Code, w.Body)
		}
	}
	rec("before", base.Add(-time.Hour))
	rec("at-since", base)
	rec("in-range", base.Add(time.Minute))
	rec("after", base.Add(time.Hour))

	url := fmt.Sprintf("/runs?since=%s&until=%s",
		base.Format(time.RFC3339), base.Add(time.Hour).Format(time.RFC3339))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	var got struct {
		Runs []struct {
			RunID string `json:"run_id"`
		} `json:"runs"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Runs) != 2 {
		t.Fatalf("want at-since and in-range only, got %+v", got.Runs)
	}
	seen := map[string]bool{}
	for _, r := range got.Runs {
		seen[r.RunID] = true
	}
	if !seen["at-since"] || !seen["in-range"] || seen["before"] || seen["after"] {
		t.Fatalf("since/until did not narrow correctly: %+v", got.Runs)
	}
}

func TestSinceUntilCombineWithOtherFilters(t *testing.T) {
	h := srv(t)
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	rec := func(id, project string, at time.Time) {
		body := fmt.Sprintf(`{"project":%q,"git_commit":"c","config_hash":"cfg","run_id":%q,"started_at":%q}`,
			project, id, at.Format(time.RFC3339))
		w := post(t, h, body)
		if w.Code != http.StatusCreated {
			t.Fatalf("setup failed for %s: %d %s", id, w.Code, w.Body)
		}
	}
	rec("alpha-in", "alpha", base)
	rec("beta-in", "beta", base)
	rec("alpha-out", "alpha", base.Add(-time.Hour))

	url := fmt.Sprintf("/runs?project=alpha&status=created&since=%s&until=%s",
		base.Format(time.RFC3339), base.Add(time.Hour).Format(time.RFC3339))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	var got struct {
		Runs []struct {
			RunID string `json:"run_id"`
		} `json:"runs"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Runs) != 1 || got.Runs[0].RunID != "alpha-in" {
		t.Fatalf("project/status/since/until together did not narrow correctly: %+v", got.Runs)
	}
}

func TestMalformedSinceUntilIsRejected(t *testing.T) {
	for _, q := range []string{"since=not-a-time", "until=not-a-time", "since=2024-01-01"} {
		w := httptest.NewRecorder()
		srv(t).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/runs?"+q, nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d: %s", q, w.Code, w.Body)
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

func TestUnknownQueryParamIsRejected(t *testing.T) {
	w := httptest.NewRecorder()
	srv(t).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/runs?projct=demo", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a typo'd query parameter must be refused, got %d: %s", w.Code, w.Body)
	}
}

func TestInvalidStatusIsRejected(t *testing.T) {
	w := httptest.NewRecorder()
	srv(t).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/runs?status=succeded", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a typo'd status must be refused, not silently return an empty result, got %d: %s", w.Code, w.Body)
	}
}

func TestValidStatusIsAccepted(t *testing.T) {
	h := srv(t)
	w := post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("setup failed: %d %s", w.Code, w.Body)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/runs?status=created", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	var got struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Count != 1 {
		t.Fatalf("want the one created run, got %d", got.Count)
	}
}

func TestListWithRecognizedParamsStillWorks(t *testing.T) {
	h := srv(t)
	w := post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg","device":"gpu0"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("setup failed: %d %s", w.Code, w.Body)
	}
	var run map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &run)
	fingerprint := run["fingerprint"].(string)

	url := fmt.Sprintf("/runs?project=p&git_commit=abc&fingerprint=%s&status=created&device=gpu0&limit=10&cursor=", fingerprint)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 for a query using only recognized params, got %d: %s", w.Code, w.Body)
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
	req.Header.Set("Content-Type", "application/json")
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
	req.Header.Set("Content-Type", "application/json")
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
	req.Header.Set("Content-Type", "application/json")
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
	// The token is valid, just scoped to reads -- that's a 403 (scope
	// denial), not a 401 (invalid credentials) that would invite the client
	// to retry forever with the same token.
	if w.Code != http.StatusForbidden {
		t.Fatalf("a read token must not be able to write, want 403, got %d: %s", w.Code, w.Body)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("a scope denial is not a request for different credentials, want no WWW-Authenticate, got %q", got)
	}
}

func TestMissingOrGarbageTokenIs401WithChallenge(t *testing.T) {
	h := srvWithAuth(t, Auth{WriteToken: "write-secret", ReadToken: "read-secret"})

	for _, token := range []string{"", "garbage"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, authed(http.MethodGet, "/runs", token))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("token %q against a read endpoint: want 401, got %d: %s", token, w.Code, w.Body)
		}
		if got := w.Header().Get("WWW-Authenticate"); got == "" {
			t.Fatalf("token %q: want a WWW-Authenticate challenge on 401, got none", token)
		}

		w = httptest.NewRecorder()
		h.ServeHTTP(w, authed(http.MethodPost, "/runs", token))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("token %q against a write endpoint: want 401, got %d: %s", token, w.Code, w.Body)
		}
		if got := w.Header().Get("WWW-Authenticate"); got == "" {
			t.Fatalf("token %q: want a WWW-Authenticate challenge on 401, got none", token)
		}
	}
}

func TestFingerprintsListOnlyIncludesRepeats(t *testing.T) {
	h := srv(t)
	// Two runs of the same experiment (identical identity fields, so they
	// share a fingerprint), one run of a different experiment.
	post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg","seed":1,"metrics":{"loss":0.4}}`)
	post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg","seed":1,"metrics":{"loss":0.5}}`)
	post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg","seed":2,"metrics":{"loss":0.1}}`)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/fingerprints?project=p", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	var out struct {
		Count  int `json:"count"`
		Groups []struct {
			Fingerprint string `json:"fingerprint"`
			Count       int    `json:"count"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 1 || len(out.Groups) != 1 {
		t.Fatalf("want exactly one group with repeats, got %+v", out)
	}
	if out.Groups[0].Count != 2 {
		t.Fatalf("want the repeated group to report 2 runs, got %d", out.Groups[0].Count)
	}
}

func TestFingerprintsListMinRunsIncludesLoneRuns(t *testing.T) {
	h := srv(t)
	// Two runs of the same experiment, one lone run of a different one --
	// the default (min_runs=2) sees only the first; min_runs=1 or 0 must
	// also surface the lone run, matching what GET /fingerprints/{fp} would
	// already report for it (no_repeats: true, not absent from the list).
	post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg","seed":1,"metrics":{"loss":0.4}}`)
	post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg","seed":1,"metrics":{"loss":0.5}}`)
	w := post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg","seed":2,"metrics":{"loss":0.1}}`)
	var lone map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &lone)
	loneFP := lone["fingerprint"].(string)

	for _, minRuns := range []string{"1", "0"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/fingerprints?project=p&min_runs="+minRuns, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("min_runs=%s: want 200, got %d: %s", minRuns, w.Code, w.Body)
		}
		var out struct {
			Count  int `json:"count"`
			Groups []struct {
				Fingerprint string `json:"fingerprint"`
				NoRepeats   bool   `json:"no_repeats"`
			} `json:"groups"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Count != 2 || len(out.Groups) != 2 {
			t.Fatalf("min_runs=%s: want both fingerprints, got %+v", minRuns, out)
		}
		found := false
		for _, g := range out.Groups {
			if g.Fingerprint == loneFP {
				found = true
				if !g.NoRepeats {
					t.Fatalf("min_runs=%s: lone run's group must report no_repeats", minRuns)
				}
			}
		}
		if !found {
			t.Fatalf("min_runs=%s: lone-run fingerprint %q missing from the list, got %+v", minRuns, loneFP, out)
		}
	}
}

func TestFingerprintsListInvalidMinRunsIs400(t *testing.T) {
	for _, minRuns := range []string{"-1", "not-a-number", "1.5"} {
		w := httptest.NewRecorder()
		srv(t).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/fingerprints?min_runs="+minRuns, nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("min_runs=%s: want 400, got %d: %s", minRuns, w.Code, w.Body)
		}
	}
}

func TestFingerprintsListPaginatesWithoutSkippingOrRepeating(t *testing.T) {
	h := srv(t)
	const n = 5
	fps := map[string]bool{}
	for i := 0; i < n; i++ {
		// Distinct seeds -> distinct fingerprints; two runs each so every
		// group clears the default min_runs and has a real (zero) spread to
		// sort on.
		post(t, h, fmt.Sprintf(`{"project":"p","git_commit":"abc","config_hash":"cfg","seed":%d,"metrics":{"loss":0.1}}`, i))
		w := post(t, h, fmt.Sprintf(`{"project":"p","git_commit":"abc","config_hash":"cfg","seed":%d,"metrics":{"loss":0.2}}`, i))
		var created map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &created)
		fps[created["fingerprint"].(string)] = true
	}
	if len(fps) != n {
		t.Fatalf("setup: want %d distinct fingerprints, got %d", n, len(fps))
	}

	type page struct {
		Groups []struct {
			Fingerprint string `json:"fingerprint"`
		} `json:"groups"`
		Count      int    `json:"count"`
		Limit      int    `json:"limit"`
		NextCursor string `json:"next_cursor"`
	}

	// First page.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/fingerprints?project=p&limit=2", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("page 1: want 200, got %d: %s", w.Code, w.Body)
	}
	var p1 page
	if err := json.Unmarshal(w.Body.Bytes(), &p1); err != nil {
		t.Fatal(err)
	}
	if p1.Count != 2 || p1.Limit != 2 {
		t.Fatalf("page 1: want count=2 limit=2, got %+v", p1)
	}
	if p1.NextCursor == "" {
		t.Fatalf("page 1: want a next_cursor with more groups remaining, got %+v", p1)
	}

	seen := map[string]bool{}
	for _, g := range p1.Groups {
		seen[g.Fingerprint] = true
	}

	// Follow next_cursor to the end.
	cursor := p1.NextCursor
	var last page
	for i := 0; i < n; i++ { // generous guard against a cursor that never terminates
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/fingerprints?project=p&limit=2&cursor="+cursor, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("follow-up page: want 200, got %d: %s", w.Code, w.Body)
		}
		var pg page
		if err := json.Unmarshal(w.Body.Bytes(), &pg); err != nil {
			t.Fatal(err)
		}
		for _, g := range pg.Groups {
			if seen[g.Fingerprint] {
				t.Fatalf("fingerprint %q returned on more than one page", g.Fingerprint)
			}
			seen[g.Fingerprint] = true
		}
		last = pg
		if pg.NextCursor == "" {
			break
		}
		cursor = pg.NextCursor
	}
	if last.NextCursor != "" {
		t.Fatalf("last page must have no next_cursor, got %+v", last)
	}
	if len(seen) != n {
		t.Fatalf("want all %d fingerprints visited across pages with no gaps, got %d: %v", n, len(seen), seen)
	}
}

func TestFingerprintsListInvalidCursorIsRejected(t *testing.T) {
	for _, cursor := range []string{"not-base64url!!", "aGVsbG8"} {
		w := httptest.NewRecorder()
		srv(t).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/fingerprints?cursor="+cursor, nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("cursor=%s: want 400, got %d: %s", cursor, w.Code, w.Body)
		}
	}
}

func TestFingerprintsListEmptyResultIsEmptyArrayNotNull(t *testing.T) {
	// An empty store returns no groups at all -- the nil slice that
	// `var groups []spread.Group` would leave unappended-to serializes as
	// JSON null, not []; unmarshaling into a Go slice would hide that bug
	// (nil and [] decode the same way), so this checks the raw body text.
	w := httptest.NewRecorder()
	srv(t).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/fingerprints", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), `"groups":null`) {
		t.Fatalf("want groups:[] for an empty result, got: %s", w.Body)
	}

	// Also reachable with data present but min_runs set high enough that
	// nothing qualifies.
	h := srv(t)
	post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg","seed":1,"metrics":{"loss":0.4}}`)
	post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg","seed":1,"metrics":{"loss":0.5}}`)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/fingerprints?min_runs=5", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), `"groups":null`) {
		t.Fatalf("want groups:[] when min_runs excludes everything, got: %s", w.Body)
	}
}

func TestFingerprintOneGroupReportsMetricStats(t *testing.T) {
	h := srv(t)
	var a, b map[string]any
	w := post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg","seed":1,"metrics":{"loss":0.4}}`)
	_ = json.Unmarshal(w.Body.Bytes(), &a)
	w = post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg","seed":1,"metrics":{"loss":0.5}}`)
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	fp := a["fingerprint"].(string)

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/fingerprints/"+fp, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	var out struct {
		Count     int  `json:"count"`
		NoRepeats bool `json:"no_repeats"`
		Metrics   map[string]struct {
			Count  int     `json:"count"`
			Mean   float64 `json:"mean"`
			StdDev float64 `json:"stddev"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.NoRepeats || out.Count != 2 {
		t.Fatalf("want a two-run group, not no_repeats, got %+v", out)
	}
	loss, ok := out.Metrics["loss"]
	if !ok || loss.Count != 2 {
		t.Fatalf("want a loss stat over both runs, got %+v", out.Metrics)
	}
	if loss.Mean != 0.45 {
		t.Fatalf("want mean 0.45, got %v", loss.Mean)
	}
}

func TestFingerprintSingleRunIsNoRepeats(t *testing.T) {
	h := srv(t)
	w := post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg","seed":1,"metrics":{"loss":0.4}}`)
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	fp := created["fingerprint"].(string)

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/fingerprints/"+fp, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	var out struct {
		NoRepeats bool           `json:"no_repeats"`
		Metrics   map[string]any `json:"metrics"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.NoRepeats {
		t.Fatal("a single run recorded for this fingerprint must be reported as no repeats")
	}
	if len(out.Metrics) != 0 {
		t.Fatalf("a no-repeats group must not report metric stats, got %+v", out.Metrics)
	}
}

func TestFingerprintUnknownIs404(t *testing.T) {
	w := httptest.NewRecorder()
	srv(t).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/fingerprints/nope", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func patch(t *testing.T, h http.Handler, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/runs/"+id, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(w, req)
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

func TestRecordedRunOmitsEndedAtUntilItEnds(t *testing.T) {
	h := srv(t)
	w := post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg"}`)
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if v, ok := created["ended_at"]; ok {
		t.Fatalf("want no ended_at key on a fresh run, got %v", v)
	}
	id := created["run_id"].(string)

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/runs/"+id, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if v, ok := got["ended_at"]; ok {
		// A run that hasn't ended must never come back as the year-1
		// timestamp a zero-valued time.Time serializes to.
		t.Fatalf("want no ended_at key on GET of an unfinished run, got %v", v)
	}
}

func TestTerminalPatchWithoutEndedAtDefaultsToReceiveTime(t *testing.T) {
	h := srv(t)
	w := post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg"}`)
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	id := created["run_id"].(string)

	w = patch(t, h, id, `{"status":"running"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("created -> running: want 200, got %d: %s", w.Code, w.Body)
	}

	before := time.Now()
	w = patch(t, h, id, `{"status":"succeeded"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("running -> succeeded: want 200, got %d: %s", w.Code, w.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	endedAtStr, ok := got["ended_at"].(string)
	if !ok {
		t.Fatalf("want ended_at defaulted on a terminal transition, got %v", got["ended_at"])
	}
	endedAt, err := time.Parse(time.RFC3339, endedAtStr)
	if err != nil {
		t.Fatalf("ended_at %q did not parse: %v", endedAtStr, err)
	}
	if endedAt.Before(before.Add(-5*time.Second)) || endedAt.After(time.Now().Add(5*time.Second)) {
		t.Fatalf("want ended_at near now, got %v", endedAt)
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

func errBody(t *testing.T, w *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("error body did not decode as JSON: %v: %s", err, w.Body)
	}
	return got
}

func TestUpdateIdentityConflictHasSpecificCode(t *testing.T) {
	h := srv(t)
	w := post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg"}`)
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	id := created["run_id"].(string)

	w = patch(t, h, id, `{"git_commit":"different"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", w.Code, w.Body)
	}
	got := errBody(t, w)
	if got["code"] != "identity_conflict" {
		t.Fatalf("want code identity_conflict, got %q: %s", got["code"], w.Body)
	}
}

func TestUpdateIllegalTransitionHasSpecificCode(t *testing.T) {
	h := srv(t)
	w := post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg"}`) // starts at "created"
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	id := created["run_id"].(string)

	w = patch(t, h, id, `{"status":"succeeded"}`) // created -> succeeded skips running
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", w.Code, w.Body)
	}
	got := errBody(t, w)
	if got["code"] != "illegal_transition" {
		t.Fatalf("want code illegal_transition, got %q: %s", got["code"], w.Body)
	}
}

func TestRecordIDTakenHasSpecificCode(t *testing.T) {
	h := srv(t)
	body := `{"project":"p","git_commit":"abc","config_hash":"cfg","run_id":"fixed-id"}`
	post(t, h, body) // first record succeeds

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/runs",
		strings.NewReader(`{"project":"p","git_commit":"other","config_hash":"cfg","run_id":"fixed-id"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", w.Code, w.Body)
	}
	got := errBody(t, w)
	if got["code"] != "id_taken" {
		t.Fatalf("want code id_taken, got %q: %s", got["code"], w.Body)
	}
}

func TestErrorBodyEchoesRequestID(t *testing.T) {
	h := srv(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/runs/nope", nil)
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body)
	}
	headerID := w.Header().Get("X-Request-Id")
	if headerID == "" {
		t.Fatal("no X-Request-Id response header")
	}
	got := errBody(t, w)
	if got["request_id"] != headerID {
		t.Fatalf("want error body request_id %q to match response header, got %q", headerID, got["request_id"])
	}
}

func TestUpdateIdenticalTerminalPatchRetriedIs200(t *testing.T) {
	h := srv(t)
	w := post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg"}`)
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	id := created["run_id"].(string)

	w = patch(t, h, id, `{"status":"running"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("created -> running: want 200, got %d: %s", w.Code, w.Body)
	}

	body := `{"status":"succeeded","metrics":{"loss":0.4}}`
	w = patch(t, h, id, body)
	if w.Code != http.StatusOK {
		t.Fatalf("first terminal patch: want 200, got %d: %s", w.Code, w.Body)
	}

	// A client that retries after losing the response to the first call must
	// see the same success, not a 409 it cannot tell apart from a real
	// conflict.
	w = patch(t, h, id, body)
	if w.Code != http.StatusOK {
		t.Fatalf("identical terminal patch retried: want 200, got %d: %s", w.Code, w.Body)
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
	// A recognized token missing write scope is a 403, not a 401 -- see
	// TestReadTokenCannotWrite.
	if w.Code != http.StatusForbidden {
		t.Fatalf("a read token must not be able to PATCH, want 403, got %d: %s", w.Code, w.Body)
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

func TestUnmatchedPathIs404WithJSONEnvelope(t *testing.T) {
	w := httptest.NewRecorder()
	srv(t).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("want a JSON error body, got Content-Type %q: %s", ct, w.Body)
	}
	got := errBody(t, w)
	if got["code"] != "not_found" {
		t.Fatalf("want code not_found, got %q: %s", got["code"], w.Body)
	}
	if got["request_id"] == "" {
		t.Fatalf("want a request_id in the error body: %s", w.Body)
	}
}

func TestDisallowedMethodIs405WithAllowHeader(t *testing.T) {
	w := httptest.NewRecorder()
	srv(t).ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/runs", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d: %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("want a JSON error body, got Content-Type %q: %s", ct, w.Body)
	}
	got := errBody(t, w)
	if got["code"] != "method_not_allowed" {
		t.Fatalf("want code method_not_allowed, got %q: %s", got["code"], w.Body)
	}
	allow := w.Header().Get("Allow")
	if !strings.Contains(allow, "GET") || !strings.Contains(allow, "POST") {
		t.Fatalf("want Allow to list GET and POST for /runs, got %q", allow)
	}
}

func TestRecordSetsLocationHeader(t *testing.T) {
	w := post(t, srv(t), `{"project":"p","git_commit":"abc","config_hash":"cfg"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	runID, _ := got["run_id"].(string)
	if runID == "" {
		t.Fatalf("no run id assigned: %s", w.Body)
	}
	want := "/runs/" + runID
	if got := w.Header().Get("Location"); got != want {
		t.Fatalf("want Location %q, got %q", want, got)
	}
}

func TestDisallowedMethodOnPathParamRouteIs405(t *testing.T) {
	// /runs/{id} only supports GET and PATCH -- DELETE must be refused the
	// same way, with the path-templated route resolved correctly.
	w := httptest.NewRecorder()
	srv(t).ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/runs/xyz", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d: %s", w.Code, w.Body)
	}
	allow := w.Header().Get("Allow")
	if !strings.Contains(allow, "GET") || !strings.Contains(allow, "PATCH") {
		t.Fatalf("want Allow to list GET and PATCH for /runs/{id}, got %q", allow)
	}
}

func requestWithContentType(method, path, contentType, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

func TestRecordWrongContentTypeIs415(t *testing.T) {
	body := `{"project":"p","git_commit":"abc","config_hash":"cfg"}`
	for _, ct := range []string{"text/plain", ""} {
		w := httptest.NewRecorder()
		srv(t).ServeHTTP(w, requestWithContentType(http.MethodPost, "/runs", ct, body))
		if w.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("Content-Type %q: want 415, got %d: %s", ct, w.Code, w.Body)
		}
		got := errBody(t, w)
		if got["code"] != "unsupported_media_type" {
			t.Fatalf("Content-Type %q: want code unsupported_media_type, got %q: %s", ct, got["code"], w.Body)
		}
	}
}

func TestRecordAcceptsJSONWithCharsetParameter(t *testing.T) {
	body := `{"project":"p","git_commit":"abc","config_hash":"cfg"}`
	w := httptest.NewRecorder()
	srv(t).ServeHTTP(w, requestWithContentType(http.MethodPost, "/runs", "application/json; charset=utf-8", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("a charset parameter must not be mistaken for a wrong media type, got %d: %s", w.Code, w.Body)
	}
}

func TestUpdateWrongContentTypeIs415(t *testing.T) {
	h := srv(t)
	w := post(t, h, `{"project":"p","git_commit":"abc","config_hash":"cfg"}`)
	var created map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	id := created["run_id"].(string)

	w = httptest.NewRecorder()
	h.ServeHTTP(w, requestWithContentType(http.MethodPatch, "/runs/"+id, "text/plain", `{"status":"running"}`))
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("want 415, got %d: %s", w.Code, w.Body)
	}
	got := errBody(t, w)
	if got["code"] != "unsupported_media_type" {
		t.Fatalf("want code unsupported_media_type, got %q: %s", got["code"], w.Body)
	}
}

func TestOversizedBodyIsCleanBadRequest(t *testing.T) {
	// One oversized value is enough to blow the cap without needing a large
	// number of map entries.
	big := strings.Repeat("a", MaxRequestBodyBytes+1024)
	body := fmt.Sprintf(`{"project":"p","git_commit":"abc","config_hash":"cfg","params":{"huge":%q}}`, big)

	w := httptest.NewRecorder()
	srv(t).ServeHTTP(w, requestWithContentType(http.MethodPost, "/runs", "application/json", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a body over the cap must fail cleanly as 400, got %d: %s", w.Code, w.Body)
	}
}

func TestSameStartedAtWithoutClientRunIDDoesNotCollide(t *testing.T) {
	h := srv(t)
	// Same identity fields (so the same fingerprint) and the same
	// client-supplied started_at, as a scheduler emitting second-granularity
	// timestamps might send for two real repeats -- device differs so the
	// two runs are not byte-identical (which would make the second record an
	// idempotent no-op rather than exercise the id-collision path at all).
	startedAt := "2026-01-01T00:00:00Z"
	body1 := fmt.Sprintf(`{"project":"p","git_commit":"abc","config_hash":"cfg","seed":1,"started_at":%q,"device":"gpu0"}`, startedAt)
	body2 := fmt.Sprintf(`{"project":"p","git_commit":"abc","config_hash":"cfg","seed":1,"started_at":%q,"device":"gpu1"}`, startedAt)

	w1 := post(t, h, body1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first record: want 201, got %d: %s", w1.Code, w1.Body)
	}
	w2 := post(t, h, body2)
	if w2.Code != http.StatusCreated {
		t.Fatalf("second record sharing started_at: want 201, not a spurious id conflict, got %d: %s", w2.Code, w2.Body)
	}

	var r1, r2 map[string]any
	_ = json.Unmarshal(w1.Body.Bytes(), &r1)
	_ = json.Unmarshal(w2.Body.Bytes(), &r2)
	if r1["run_id"] == r2["run_id"] {
		t.Fatalf("two distinct runs sharing a fingerprint and started_at must not collide into one run_id: %v", r1["run_id"])
	}
}
