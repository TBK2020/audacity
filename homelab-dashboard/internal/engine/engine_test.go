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

// feedSvc pushes one probe result through a service's state machine and
// returns all resulting effective transitions.
func feedSvc(e *Engine, id string, ok, degraded bool) []Transition {
	e.mu.Lock()
	defer e.mu.Unlock()
	cs := e.services[id].Checks[0]
	res := probe.Result{OK: ok, Degraded: degraded, Latency: time.Millisecond}
	cs.LastResult = res
	e.applyResult(cs, res, time.Now())
	return e.recomputeService(id, time.Now(), cs)
}

// feed is the single-service shorthand used by the state machine tests.
func feed(e *Engine, ok, degraded bool) *Transition {
	trs := feedSvc(e, "svc", ok, degraded)
	if len(trs) == 0 {
		return nil
	}
	return &trs[0]
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

// depEngine: router ← nas ← jellyfin (jellyfin depends on nas, nas on router).
func depEngine(t *testing.T) *Engine {
	t.Helper()
	mk := func(id string, deps ...string) config.Service {
		return config.Service{
			ID: id, Name: id, DependsOn: deps,
			Checks: []config.Check{{Type: "tcp", Address: "x:1",
				FailureThreshold: 1, SuccessThreshold: 1}},
		}
	}
	cfg := &config.Config{Services: []config.Service{
		mk("router"), mk("nas", "router"), mk("jellyfin", "nas"),
	}}
	if err := applyIDs(cfg); err != nil {
		t.Fatal(err)
	}
	e := New(cfg)
	for _, id := range []string{"router", "nas", "jellyfin"} {
		feedSvc(e, id, true, false) // everything up
	}
	return e
}

func statusOf(e *Engine, id string) Status {
	for _, sv := range e.Snapshot() {
		if sv.ID == id {
			return sv.Status
		}
	}
	return ""
}

func TestDependencySuppression(t *testing.T) {
	e := depEngine(t)

	// Router dies; nas and jellyfin also fail their probes (they're behind it).
	trs := feedSvc(e, "router", false, false)
	if len(trs) != 1 || trs[0].To != StatusDown {
		t.Fatalf("router down transitions = %+v", trs)
	}
	trs = feedSvc(e, "nas", false, false)
	if len(trs) != 1 || trs[0].To != StatusUnreachable {
		t.Fatalf("nas should go unreachable, got %+v", trs)
	}
	// Transitive: jellyfin depends on nas (unreachable) → also unreachable.
	trs = feedSvc(e, "jellyfin", false, false)
	if len(trs) != 1 || trs[0].To != StatusUnreachable {
		t.Fatalf("jellyfin should go unreachable, got %+v", trs)
	}

	// Router recovers; nas is still down → nas flips unreachable→down (a real
	// alert now), jellyfin stays unreachable behind nas.
	trs = feedSvc(e, "router", true, false)
	toByID := map[string]Status{}
	for _, tr := range trs {
		toByID[tr.ServiceID] = tr.To
	}
	if toByID["router"] != StatusUp || toByID["nas"] != StatusDown {
		t.Fatalf("after router recovery: %+v", trs)
	}
	if statusOf(e, "jellyfin") != StatusUnreachable {
		t.Fatalf("jellyfin = %s, want unreachable", statusOf(e, "jellyfin"))
	}

	// nas recovers → jellyfin's own down status resurfaces.
	feedSvc(e, "nas", true, false)
	if statusOf(e, "jellyfin") != StatusDown {
		t.Fatalf("jellyfin = %s, want down", statusOf(e, "jellyfin"))
	}
	feedSvc(e, "jellyfin", true, false)
	if statusOf(e, "jellyfin") != StatusUp {
		t.Fatalf("jellyfin = %s, want up", statusOf(e, "jellyfin"))
	}
}

func TestUpServiceUnaffectedByDeadDependency(t *testing.T) {
	e := depEngine(t)
	feedSvc(e, "router", false, false)
	// nas probes still succeed (e.g. probed via another path) → stays up.
	if statusOf(e, "nas") != StatusUp {
		t.Fatalf("nas = %s, want up", statusOf(e, "nas"))
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
