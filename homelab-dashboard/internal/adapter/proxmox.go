package adapter

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"labdeck/internal/config"
)

// proxmox pulls the whole cluster inventory in a single call:
// GET /api2/json/cluster/resources returns nodes and all qemu/lxc guests
// with live cpu/mem/disk — no per-guest requests, no agent in the guests.
type proxmox struct {
	id, name string
	baseURL  string
	auth     string // PVEAPIToken=user@realm!token=secret
	client   *http.Client
}

func newProxmox(c *config.Integration) (Adapter, error) {
	transport := &http.Transport{}
	if c.InsecureTLS {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &proxmox{
		id:      c.ID,
		name:    c.Name,
		baseURL: strings.TrimRight(c.URL, "/"),
		auth:    fmt.Sprintf("PVEAPIToken=%s=%s", c.TokenID, c.TokenSecret),
		client:  &http.Client{Transport: transport},
	}, nil
}

func (p *proxmox) ID() string   { return p.id }
func (p *proxmox) Type() string { return "proxmox" }

type pveResource struct {
	Type    string  `json:"type"` // node | qemu | lxc | storage | …
	Node    string  `json:"node"`
	VMID    int     `json:"vmid"`
	Name    string  `json:"name"`
	Status  string  `json:"status"`
	CPU     float64 `json:"cpu"` // fraction 0–1 of maxcpu
	MaxCPU  float64 `json:"maxcpu"`
	Mem     int64   `json:"mem"`
	MaxMem  int64   `json:"maxmem"`
	Disk    int64   `json:"disk"`
	MaxDisk int64   `json:"maxdisk"`
}

func (p *proxmox) Fetch(ctx context.Context) ([]Resource, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.baseURL+"/api2/json/cluster/resources", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", p.auth)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxmox api status %d (check token permissions)", resp.StatusCode)
	}
	var payload struct {
		Data []pveResource `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode cluster/resources: %w", err)
	}

	var out []Resource
	for _, r := range payload.Data {
		switch r.Type {
		case "node":
			out = append(out, Resource{
				ID:       "node/" + r.Node,
				Kind:     "node",
				Name:     r.Node,
				Status:   nodeStatus(r.Status),
				CPUPct:   r.CPU * 100,
				CPUCores: r.MaxCPU,
				MemUsed:  r.Mem, MemTotal: r.MaxMem,
				DiskUsed: r.Disk, DiskTotal: r.MaxDisk,
			})
		case "qemu", "lxc":
			kind := "vm"
			if r.Type == "lxc" {
				kind = "lxc"
			}
			name := r.Name
			if name == "" {
				name = fmt.Sprintf("%s-%d", r.Type, r.VMID)
			}
			out = append(out, Resource{
				ID:       fmt.Sprintf("%s/%d", r.Type, r.VMID),
				ParentID: "node/" + r.Node,
				Kind:     kind,
				Name:     name,
				Status:   r.Status,
				CPUPct:   r.CPU * 100,
				CPUCores: r.MaxCPU,
				MemUsed:  r.Mem, MemTotal: r.MaxMem,
				DiskUsed: r.Disk, DiskTotal: r.MaxDisk,
			})
		}
	}
	return out, nil
}

func nodeStatus(s string) string {
	if s == "online" {
		return "running"
	}
	return s
}
