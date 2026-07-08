package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	"labdeck/internal/config"
)

// docker talks to the Engine API directly (unix socket or tcp), so it works
// against dockerd, a socket proxy, and Podman's compat API alike.
type docker struct {
	id     string
	client *http.Client
	base   string // http://docker or http://host:port
}

func newDocker(c *config.Integration) (Adapter, error) {
	ep := c.Endpoint
	switch {
	case strings.HasPrefix(ep, "unix://"):
		sock := strings.TrimPrefix(ep, "unix://")
		return &docker{
			id:   c.ID,
			base: "http://docker",
			client: &http.Client{Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sock)
				},
			}},
		}, nil
	case strings.HasPrefix(ep, "tcp://"):
		return &docker{
			id:     c.ID,
			base:   "http://" + strings.TrimPrefix(ep, "tcp://"),
			client: &http.Client{},
		}, nil
	case strings.HasPrefix(ep, "http://"), strings.HasPrefix(ep, "https://"):
		return &docker{id: c.ID, base: strings.TrimRight(ep, "/"), client: &http.Client{}}, nil
	default:
		return nil, fmt.Errorf("docker endpoint must start with unix://, tcp:// or http(s)://")
	}
}

func (d *docker) ID() string   { return d.id }
func (d *docker) Type() string { return "docker" }

type dockerContainer struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
	State string   `json:"State"` // running | exited | paused | …
}

type dockerStats struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs  int    `json:"online_cpus"`
	} `json:"cpu_stats"`
	PreCPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemUsage uint64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
}

func (d *docker) Fetch(ctx context.Context) ([]Resource, error) {
	var containers []dockerContainer
	if err := d.getJSON(ctx, "/v1.24/containers/json?all=1", &containers); err != nil {
		return nil, err
	}

	out := make([]Resource, len(containers))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4) // stats calls block ~1s each; bound the burst
	for i, c := range containers {
		name := c.ID[:12]
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out[i] = Resource{ID: "ct/" + c.ID[:12], Kind: "container", Name: name, Status: c.State}
		if c.State != "running" {
			continue
		}
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var st dockerStats
			// stream=false yields one sample with precpu prefilled — enough for a rate.
			if err := d.getJSON(ctx, "/v1.24/containers/"+id+"/stats?stream=false", &st); err != nil {
				return
			}
			cpuDelta := float64(st.CPUStats.CPUUsage.TotalUsage) - float64(st.PreCPUStats.CPUUsage.TotalUsage)
			sysDelta := float64(st.CPUStats.SystemUsage) - float64(st.PreCPUStats.SystemUsage)
			cores := st.CPUStats.OnlineCPUs
			if cores == 0 {
				cores = 1
			}
			if sysDelta > 0 && cpuDelta >= 0 {
				// Fraction of the whole host (0–100), matching CPUPct semantics.
				out[i].CPUPct = cpuDelta / sysDelta * 100
			}
			out[i].CPUCores = float64(cores)
			mem := st.MemoryStats.Usage
			// cgroup v1 counts page cache in usage; inactive_file backs it out.
			if inactive, ok := st.MemoryStats.Stats["inactive_file"]; ok && inactive < mem {
				mem -= inactive
			}
			out[i].MemUsed = int64(mem)
			out[i].MemTotal = int64(st.MemoryStats.Limit)
		}(i, c.ID)
	}
	wg.Wait()
	return out, nil
}

func (d *docker) getJSON(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker api %s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
