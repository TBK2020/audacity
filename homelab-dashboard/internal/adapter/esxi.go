package adapter

import (
	"context"
	"fmt"
	"net/url"
	"sync"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/mo"

	"labdeck/internal/config"
)

// esxi speaks the SOAP VIM API via govmomi — the only management API present
// on standalone ESXi hosts (the REST vAPI ships with vCenter only). One
// ContainerView + property retrieval per fetch covers all hosts and VMs.
type esxi struct {
	id       string
	u        *url.URL
	insecure bool

	mu     sync.Mutex
	client *govmomi.Client
}

func newESXi(c *config.Integration) (Adapter, error) {
	u, err := url.Parse(c.URL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/sdk"
	}
	u.User = url.UserPassword(c.Username, c.Password)
	return &esxi{id: c.ID, u: u, insecure: c.InsecureTLS}, nil
}

func (e *esxi) ID() string   { return e.id }
func (e *esxi) Type() string { return "esxi" }

// connect lazily establishes (and reuses) the SOAP session.
func (e *esxi) connect(ctx context.Context) (*govmomi.Client, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.client != nil {
		if ok, _ := e.client.SessionManager.SessionIsActive(ctx); ok {
			return e.client, nil
		}
		e.client = nil
	}
	c, err := govmomi.NewClient(ctx, e.u, e.insecure)
	if err != nil {
		return nil, fmt.Errorf("vim login: %w", err)
	}
	e.client = c
	return c, nil
}

func (e *esxi) Fetch(ctx context.Context) ([]Resource, error) {
	c, err := e.connect(ctx)
	if err != nil {
		return nil, err
	}
	m := view.NewManager(c.Client)
	v, err := m.CreateContainerView(ctx, c.ServiceContent.RootFolder,
		[]string{"HostSystem", "VirtualMachine"}, true)
	if err != nil {
		return nil, err
	}
	defer v.Destroy(ctx)

	var hosts []mo.HostSystem
	if err := v.Retrieve(ctx, []string{"HostSystem"}, []string{"summary"}, &hosts); err != nil {
		return nil, err
	}
	var vms []mo.VirtualMachine
	if err := v.Retrieve(ctx, []string{"VirtualMachine"}, []string{"summary", "runtime.host"}, &vms); err != nil {
		return nil, err
	}

	out := make([]Resource, 0, len(hosts)+len(vms))
	// Host MHz capacity indexed by managed object ref, for VM percent math.
	hostRef := map[string]string{}
	for _, h := range hosts {
		hw := h.Summary.Hardware
		qs := h.Summary.QuickStats
		var totalMhz, cores float64
		if hw != nil {
			totalMhz = float64(hw.CpuMhz) * float64(hw.NumCpuCores)
			cores = float64(hw.NumCpuCores)
		}
		name := "esxi-host"
		if hw != nil && h.Summary.Config.Name != "" {
			name = h.Summary.Config.Name
		}
		id := "host/" + h.Self.Value
		hostRef[h.Self.Value] = id
		res := Resource{
			ID:       id,
			Kind:     "node",
			Name:     name,
			Status:   "running",
			CPUCores: cores,
		}
		if totalMhz > 0 {
			res.CPUPct = float64(qs.OverallCpuUsage) / totalMhz * 100
		}
		if hw != nil {
			res.MemTotal = hw.MemorySize
			res.MemUsed = int64(qs.OverallMemoryUsage) << 20 // MB → bytes
		}
		out = append(out, res)
	}

	for _, vm := range vms {
		cfg := vm.Summary.Config
		qs := vm.Summary.QuickStats
		res := Resource{
			ID:       "vm/" + vm.Self.Value,
			Kind:     "vm",
			Name:     cfg.Name,
			Status:   vmStatus(string(vm.Summary.Runtime.PowerState)),
			CPUCores: float64(cfg.NumCpu),
			MemTotal: int64(cfg.MemorySizeMB) << 20,
			MemUsed:  int64(qs.HostMemoryUsage) << 20,
		}
		if vm.Runtime.Host != nil {
			res.ParentID = hostRef[vm.Runtime.Host.Value]
		}
		// MaxCpuUsage is the VM's MHz allocation (cores × host clock).
		if maxMhz := vm.Summary.Runtime.MaxCpuUsage; maxMhz > 0 {
			res.CPUPct = float64(qs.OverallCpuUsage) / float64(maxMhz) * 100
		}
		out = append(out, res)
	}
	return out, nil
}

func vmStatus(powerState string) string {
	switch powerState {
	case "poweredOn":
		return "running"
	case "poweredOff":
		return "stopped"
	case "suspended":
		return "paused"
	}
	return powerState
}
