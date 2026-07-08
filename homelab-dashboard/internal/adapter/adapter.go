// Package adapter implements deep integrations (Proxmox, Docker, ESXi) that
// pull a resource inventory — nodes, VMs/LXCs, containers — with live
// CPU/memory/disk usage, feeding the resource top view.
package adapter

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"labdeck/internal/config"
)

// Resource is one row of the inventory tree.
type Resource struct {
	ID       string `json:"id"`                  // unique within the integration
	ParentID string `json:"parent_id,omitempty"` // "" = child of the integration root
	Kind     string `json:"kind"`                // node | vm | lxc | container
	Name     string `json:"name"`
	Status   string `json:"status"` // running | stopped | paused | …

	// CPUPct is percent of the resource's own allocation (0–100);
	// CPUCores is that allocation, so absolute load = CPUPct × CPUCores.
	CPUPct   float64 `json:"cpu_pct"`
	CPUCores float64 `json:"cpu_cores,omitempty"`

	MemUsed   int64 `json:"mem_used,omitempty"`
	MemTotal  int64 `json:"mem_total,omitempty"`
	DiskUsed  int64 `json:"disk_used,omitempty"`
	DiskTotal int64 `json:"disk_total,omitempty"`
}

type Adapter interface {
	ID() string
	Type() string
	Fetch(ctx context.Context) ([]Resource, error)
}

// Build instantiates adapters from config.
func Build(cfgs []config.Integration) ([]Adapter, error) {
	var out []Adapter
	for i := range cfgs {
		c := &cfgs[i]
		var (
			a   Adapter
			err error
		)
		switch c.Type {
		case "proxmox":
			a, err = newProxmox(c)
		case "docker":
			a, err = newDocker(c)
		case "esxi":
			a, err = newESXi(c)
		default:
			err = fmt.Errorf("unknown integration type %q", c.Type)
		}
		if err != nil {
			return nil, fmt.Errorf("integration %s: %w", c.ID, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// IntegrationSnapshot is the latest fetch result for one integration.
type IntegrationSnapshot struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Type      string     `json:"type"`
	FetchedAt time.Time  `json:"fetched_at"`
	Error     string     `json:"error,omitempty"`
	Resources []Resource `json:"resources"`
}

// Collector polls all adapters on their intervals and caches snapshots.
type Collector struct {
	adapters []Adapter
	names    map[string]string
	interval map[string]time.Duration

	mu    sync.RWMutex
	snaps map[string]*IntegrationSnapshot
}

func NewCollector(cfgs []config.Integration, adapters []Adapter) *Collector {
	c := &Collector{
		adapters: adapters,
		names:    map[string]string{},
		interval: map[string]time.Duration{},
		snaps:    map[string]*IntegrationSnapshot{},
	}
	for _, ic := range cfgs {
		c.names[ic.ID] = ic.Name
		c.interval[ic.ID] = ic.Interval.Std()
	}
	return c
}

func (c *Collector) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, a := range c.adapters {
		wg.Add(1)
		go func(a Adapter) {
			defer wg.Done()
			interval := c.interval[a.ID()]
			if interval <= 0 {
				interval = 10 * time.Second
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				c.fetchOne(ctx, a)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}(a)
	}
	wg.Wait()
}

func (c *Collector) fetchOne(ctx context.Context, a Adapter) {
	fctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resources, err := a.Fetch(fctx)
	snap := &IntegrationSnapshot{
		ID:        a.ID(),
		Name:      c.names[a.ID()],
		Type:      a.Type(),
		FetchedAt: time.Now(),
		Resources: resources,
	}
	if err != nil {
		snap.Error = err.Error()
		slog.Warn("integration fetch failed", "integration", a.ID(), "err", err)
		// Keep last good resources so the view degrades instead of blanking.
		c.mu.RLock()
		if prev, ok := c.snaps[a.ID()]; ok && len(prev.Resources) > 0 {
			snap.Resources = prev.Resources
		}
		c.mu.RUnlock()
	}
	if snap.Resources == nil {
		snap.Resources = []Resource{}
	}
	c.mu.Lock()
	c.snaps[a.ID()] = snap
	c.mu.Unlock()
}

// Snapshot returns the latest state of every integration, in config order.
func (c *Collector) Snapshot() []IntegrationSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]IntegrationSnapshot, 0, len(c.adapters))
	for _, a := range c.adapters {
		if s, ok := c.snaps[a.ID()]; ok {
			out = append(out, *s)
		} else {
			out = append(out, IntegrationSnapshot{
				ID: a.ID(), Name: c.names[a.ID()], Type: a.Type(), Resources: []Resource{},
			})
		}
	}
	return out
}
