package metrics

import (
	"strings"
	"testing"
)

func TestWriteToRendersExpositionFormat(t *testing.T) {
	r := New(func() float64 { return 2 })
	r.RecordRun("demo", "succeeded")
	r.RecordRun("demo", "succeeded")
	r.RecordRun("demo", "failed")
	r.StoreError("conflict")
	r.ObserveRequest("POST /runs", 201, 0.0012)
	r.ObserveRequest("POST /runs", 201, 2.0)

	var b strings.Builder
	if _, err := r.WriteTo(&b); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := b.String()

	for _, want := range []string{
		"# TYPE runledger_runs_recorded_total counter",
		`runledger_runs_recorded_total{project="demo",status="succeeded"} 2`,
		`runledger_runs_recorded_total{project="demo",status="failed"} 1`,
		"# TYPE runledger_store_errors_total counter",
		`runledger_store_errors_total{kind="conflict"} 1`,
		"# TYPE runledger_request_duration_seconds histogram",
		`runledger_request_duration_seconds_bucket{route="POST /runs",code="201",le="+Inf"} 2`,
		`runledger_request_duration_seconds_count{route="POST /runs",code="201"} 2`,
		"# TYPE runledger_runs gauge",
		"runledger_runs 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestObserveRequestBucketsAreCumulative(t *testing.T) {
	r := New(func() float64 { return 0 })
	// One fast request (fits every bucket) and one slow one (only +Inf).
	r.ObserveRequest("GET /runs", 200, 0.0001)
	r.ObserveRequest("GET /runs", 200, 999)

	var b strings.Builder
	if _, err := r.WriteTo(&b); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := b.String()

	// The smallest bucket (le="0.0005") must count only the fast request.
	if !strings.Contains(out, `runledger_request_duration_seconds_bucket{route="GET /runs",code="200",le="0.0005"} 1`) {
		t.Errorf("smallest bucket did not capture the fast request:\n%s", out)
	}
	if !strings.Contains(out, `runledger_request_duration_seconds_bucket{route="GET /runs",code="200",le="+Inf"} 2`) {
		t.Errorf("+Inf bucket did not capture both requests:\n%s", out)
	}
}

func TestRunsGaugeReflectsCallbackNotLocalCount(t *testing.T) {
	// The gauge must be driven by the store, not by how many times
	// RecordRun was called -- a retried, idempotent record should not
	// inflate it.
	r := New(func() float64 { return 5 })
	r.RecordRun("p", "created")
	r.RecordRun("p", "created")

	var b strings.Builder
	if _, err := r.WriteTo(&b); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if !strings.Contains(b.String(), "runledger_runs 5") {
		t.Errorf("gauge did not reflect callback value:\n%s", b.String())
	}
}
