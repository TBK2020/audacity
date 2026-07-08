// Package engine schedules probes, runs the per-check state machine and
// aggregates check states into service status.
package engine

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"labdeck/internal/config"
	"labdeck/internal/probe"
)

type Status string

const (
	StatusPending  Status = "pending"
	StatusUp       Status = "up"
	StatusDegraded Status = "degraded"
	StatusDown     Status = "down"
)

// rank orders statuses from best to worst for aggregation.
func rank(s Status) int {
	switch s {
	case StatusUp:
		return 0
	case StatusPending:
		return 1
	case StatusDegraded:
		return 2
	case StatusDown:
		return 3
	}
	return 1
}

type CheckState struct {
	Check      *config.Check
	Status     Status
	LastResult probe.Result
	LastRun    time.Time
	LastChange time.Time

	consecFail int
	consecOK   int
}

type ServiceState struct {
	Service    *config.Service
	Status     Status
	LastChange time.Time
	Checks     []*CheckState
}

// ProbeRecord is emitted for every probe execution (history persistence).
type ProbeRecord struct {
	CheckID string
	Time    time.Time
	OK      bool
	Latency time.Duration
	Detail  string
}

// Transition is emitted when a service's aggregated status changes.
type Transition struct {
	ServiceID   string
	ServiceName string
	From, To    Status
	Time        time.Time
	Reason      string
}

type Engine struct {
	cfg *config.Config

	mu       sync.RWMutex
	services map[string]*ServiceState
	order    []string        // service ids in config order
	sshHosts map[string]bool // host ids with an ssh block

	OnRecord     func(ProbeRecord)
	OnTransition func(Transition)
}

func New(cfg *config.Config) *Engine {
	e := &Engine{cfg: cfg, services: map[string]*ServiceState{}, sshHosts: map[string]bool{}}
	for _, h := range cfg.Hosts {
		if h.SSH != nil {
			e.sshHosts[h.ID] = true
		}
	}
	now := time.Now()
	for si := range cfg.Services {
		svc := &cfg.Services[si]
		st := &ServiceState{Service: svc, Status: StatusPending, LastChange: now}
		for ci := range svc.Checks {
			st.Checks = append(st.Checks, &CheckState{
				Check:      &svc.Checks[ci],
				Status:     StatusPending,
				LastChange: now,
			})
		}
		e.services[svc.ID] = st
		e.order = append(e.order, svc.ID)
	}
	return e
}

// Run blocks until ctx is cancelled, driving one goroutine per check.
func (e *Engine) Run(ctx context.Context) {
	var wg sync.WaitGroup
	e.mu.RLock()
	for _, svc := range e.services {
		for _, cs := range svc.Checks {
			wg.Add(1)
			go func(svcID string, cs *CheckState) {
				defer wg.Done()
				e.runCheckLoop(ctx, svcID, cs)
			}(svc.Service.ID, cs)
		}
	}
	e.mu.RUnlock()
	wg.Wait()
}

func (e *Engine) runCheckLoop(ctx context.Context, svcID string, cs *CheckState) {
	interval := cs.Check.Interval.Std()
	// Initial jitter spreads probes out so a restart doesn't stampede weak
	// devices (routers, SBCs) with every check at once.
	jitter := time.Duration(rand.Int63n(int64(interval)))
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		e.executeOnce(ctx, svcID, cs)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (e *Engine) executeOnce(ctx context.Context, svcID string, cs *CheckState) {
	res := probe.Run(ctx, cs.Check)
	now := time.Now()

	if e.OnRecord != nil {
		e.OnRecord(ProbeRecord{
			CheckID: cs.Check.ID, Time: now, OK: res.OK,
			Latency: res.Latency, Detail: res.Detail,
		})
	}

	e.mu.Lock()
	cs.LastResult = res
	cs.LastRun = now
	e.applyResult(cs, res, now)
	tr := e.recomputeService(svcID, now, cs)
	e.mu.Unlock()

	if tr != nil && e.OnTransition != nil {
		e.OnTransition(*tr)
	}
}

// applyResult runs the debounced per-check state machine. Failure and recovery
// thresholds are independent so one flaky probe neither pages nor un-pages.
func (e *Engine) applyResult(cs *CheckState, res probe.Result, now time.Time) {
	if res.OK {
		cs.consecOK++
		cs.consecFail = 0
	} else {
		cs.consecFail++
		cs.consecOK = 0
	}

	next := cs.Status
	switch {
	case !res.OK && cs.consecFail >= cs.Check.FailureThreshold:
		next = StatusDown
	case res.OK && (cs.Status == StatusDown || cs.Status == StatusPending):
		if cs.consecOK >= cs.Check.SuccessThreshold || cs.Status == StatusPending {
			next = gradeUp(res)
		}
	case res.OK:
		// Already up/degraded: degradation tracks the latest result directly.
		next = gradeUp(res)
	}
	if next != cs.Status {
		cs.Status = next
		cs.LastChange = now
	}
}

func gradeUp(res probe.Result) Status {
	if res.Degraded {
		return StatusDegraded
	}
	return StatusUp
}

// recomputeService aggregates check states (worst wins) and returns a
// Transition if the service status changed. Caller holds e.mu.
func (e *Engine) recomputeService(svcID string, now time.Time, changed *CheckState) *Transition {
	svc := e.services[svcID]
	agg := StatusPending
	first := true
	for _, cs := range svc.Checks {
		if first || rank(cs.Status) > rank(agg) {
			agg = cs.Status
			first = false
		}
	}
	if agg == svc.Status {
		return nil
	}
	from := svc.Status
	svc.Status = agg
	svc.LastChange = now
	reason := ""
	if changed != nil {
		reason = changed.Check.ID + ": " + changed.LastResult.Detail
	}
	slog.Info("service status change", "service", svcID, "from", from, "to", agg, "reason", reason)
	return &Transition{
		ServiceID: svcID, ServiceName: svc.Service.Name,
		From: from, To: agg, Time: now, Reason: reason,
	}
}

// Snapshot returns a deep-enough copy of all service states in config order.
func (e *Engine) Snapshot() []ServiceView {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]ServiceView, 0, len(e.order))
	for _, id := range e.order {
		st := e.services[id]
		sv := ServiceView{
			ID:         st.Service.ID,
			Name:       st.Service.Name,
			Group:      st.Service.Group,
			Icon:       st.Service.Icon,
			Host:       st.Service.Host,
			URLs:       st.Service.URLs,
			Status:     st.Status,
			LastChange: st.LastChange,
		}
		if e.sshHosts[st.Service.Host] {
			sv.SSHHost = st.Service.Host
		}
		for _, cs := range st.Checks {
			sv.Checks = append(sv.Checks, CheckView{
				ID:        cs.Check.ID,
				Type:      cs.Check.Type,
				Status:    cs.Status,
				LatencyMS: cs.LastResult.Latency.Milliseconds(),
				Detail:    cs.LastResult.Detail,
				LastRun:   cs.LastRun,
			})
			if cs.LastResult.Latency > 0 && sv.LatencyMS == 0 {
				sv.LatencyMS = cs.LastResult.Latency.Milliseconds()
			}
		}
		out = append(out, sv)
	}
	return out
}

// ServiceView / CheckView are the JSON-facing read models.
type ServiceView struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Group      string            `json:"group"`
	Icon       string            `json:"icon,omitempty"`
	Host       string            `json:"host,omitempty"`
	URLs       map[string]string `json:"urls,omitempty"`
	Status     Status            `json:"status"`
	LatencyMS  int64             `json:"latency_ms"`
	LastChange time.Time         `json:"last_change"`
	SSHHost    string            `json:"ssh_host,omitempty"`
	Checks     []CheckView       `json:"checks"`
}

type CheckView struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Status    Status    `json:"status"`
	LatencyMS int64     `json:"latency_ms"`
	Detail    string    `json:"detail"`
	LastRun   time.Time `json:"last_run"`
}
