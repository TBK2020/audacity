package engine

import (
	"testing"
	"time"

	"labdeck/internal/config"
	"labdeck/internal/probe"
)

func testEngine(t *testing.T) *Engine {
	t.Helper()
	cfg := &config.Config{
		Services: []config.Service{{
			ID:   "svc",
			Name: "Svc",
			Checks: []config.Check{{
				Type: "http", URL: "http://x",
				FailureThreshold: 3, SuccessThreshold: 2,
			}},
		}},
	}
	if err := applyIDs(cfg); err != nil {
		t.Fatal(err)
	}
	return New(cfg)
}

// applyIDs mimics config.Load's finalize for hand-built configs.
func applyIDs(cfg *config.Config) error {
	for si := range cfg.Services {
		for ci := range cfg.Services[si].Checks {
			ck := &cfg.Services[si].Checks[ci]
			ck.ID = cfg.Services[si].ID + "/" + ck.Type
		}
	}
	return nil
}

// feed pushes one probe result through the state machine and returns the
// service transition, if any.
func feed(e *Engine, ok, degraded bool) *Transition {
	e.mu.Lock()
	defer e.mu.Unlock()
	cs := e.services["svc"].Checks[0]
	res := probe.Result{OK: ok, Degraded: degraded, Latency: time.Millisecond}
	cs.LastResult = res
	e.applyResult(cs, res, time.Now())
	return e.recomputeService("svc", time.Now(), cs)
}

func TestPendingGoesUpOnFirstSuccess(t *testing.T) {
	e := testEngine(t)
	tr := feed(e, true, false)
	if tr == nil || tr.From != StatusPending || tr.To != StatusUp {
		t.Fatalf("transition = %+v, want pending->up", tr)
	}
}

func TestDownRequiresFailureThreshold(t *testing.T) {
	e := testEngine(t)
	feed(e, true, false) // up

	if tr := feed(e, false, false); tr != nil {
		t.Fatalf("1st failure should not transition, got %+v", tr)
	}
	if tr := feed(e, false, false); tr != nil {
		t.Fatalf("2nd failure should not transition, got %+v", tr)
	}
	tr := feed(e, false, false)
	if tr == nil || tr.To != StatusDown {
		t.Fatalf("3rd failure should go down, got %+v", tr)
	}
}

func TestRecoveryRequiresSuccessThreshold(t *testing.T) {
	e := testEngine(t)
	feed(e, true, false)
	feed(e, false, false)
	feed(e, false, false)
	feed(e, false, false) // now down

	if tr := feed(e, true, false); tr != nil {
		t.Fatalf("1st success should not recover, got %+v", tr)
	}
	tr := feed(e, true, false)
	if tr == nil || tr.From != StatusDown || tr.To != StatusUp {
		t.Fatalf("2nd success should recover, got %+v", tr)
	}
}

func TestFlappingIsDebounced(t *testing.T) {
	e := testEngine(t)
	feed(e, true, false)
	// Alternating fail/ok never reaches failure_threshold=3.
	for i := 0; i < 10; i++ {
		if tr := feed(e, false, false); tr != nil {
			t.Fatalf("flap iteration %d transitioned: %+v", i, tr)
		}
		feed(e, true, false)
	}
}

func TestDegradedTracksLatestResultWhileUp(t *testing.T) {
	e := testEngine(t)
	feed(e, true, false)
	tr := feed(e, true, true)
	if tr == nil || tr.To != StatusDegraded {
		t.Fatalf("want up->degraded, got %+v", tr)
	}
	tr = feed(e, true, false)
	if tr == nil || tr.To != StatusUp {
		t.Fatalf("want degraded->up, got %+v", tr)
	}
}

func TestSnapshotShape(t *testing.T) {
	e := testEngine(t)
	feed(e, true, false)
	snap := e.Snapshot()
	if len(snap) != 1 || snap[0].ID != "svc" || snap[0].Status != StatusUp {
		t.Fatalf("snapshot = %+v", snap)
	}
	if len(snap[0].Checks) != 1 || snap[0].Checks[0].ID != "svc/http" {
		t.Fatalf("checks = %+v", snap[0].Checks)
	}
}
