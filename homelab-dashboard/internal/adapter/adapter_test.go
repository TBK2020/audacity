package adapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/vmware/govmomi/simulator"

	"labdeck/internal/config"
)

func TestProxmoxFetch(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/cluster/resources" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"data":[
			{"type":"node","node":"pve1","status":"online","cpu":0.25,"maxcpu":8,"mem":8589934592,"maxmem":34359738368,"disk":100,"maxdisk":1000},
			{"type":"qemu","node":"pve1","vmid":101,"name":"jellyfin","status":"running","cpu":0.5,"maxcpu":4,"mem":2147483648,"maxmem":4294967296},
			{"type":"lxc","node":"pve1","vmid":200,"name":"adguard","status":"stopped","cpu":0,"maxcpu":1},
			{"type":"storage","node":"pve1","status":"available"}
		]}`))
	}))
	defer srv.Close()

	a, err := newProxmox(&config.Integration{
		ID: "pve", URL: srv.URL, TokenID: "root@pam!labdeck", TokenSecret: "sekrit",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "PVEAPIToken=root@pam!labdeck=sekrit" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if len(res) != 3 { // storage rows are ignored
		t.Fatalf("resources = %d, want 3", len(res))
	}
	node, vm, lxc := res[0], res[1], res[2]
	if node.Kind != "node" || node.CPUPct != 25 || node.CPUCores != 8 || node.Status != "running" {
		t.Errorf("node = %+v", node)
	}
	if vm.Kind != "vm" || vm.ParentID != "node/pve1" || vm.CPUPct != 50 || vm.Name != "jellyfin" {
		t.Errorf("vm = %+v", vm)
	}
	if lxc.Kind != "lxc" || lxc.Status != "stopped" {
		t.Errorf("lxc = %+v", lxc)
	}
}

func TestDockerFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.24/containers/json":
			w.Write([]byte(`[
				{"Id":"aaaaaaaaaaaa1111","Names":["/jellyfin"],"State":"running"},
				{"Id":"bbbbbbbbbbbb2222","Names":["/backup"],"State":"exited"}
			]`))
		case "/v1.24/containers/aaaaaaaaaaaa1111/stats":
			w.Write([]byte(`{
				"cpu_stats":{"cpu_usage":{"total_usage":200000000},"system_cpu_usage":2000000000,"online_cpus":4},
				"precpu_stats":{"cpu_usage":{"total_usage":100000000},"system_cpu_usage":1000000000},
				"memory_stats":{"usage":1073741824,"limit":4294967296,"stats":{"inactive_file":73741824}}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a, err := newDocker(&config.Integration{ID: "dk", Endpoint: "tcp://" + srv.Listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("resources = %d, want 2", len(res))
	}
	run := res[0]
	if run.Name != "jellyfin" || run.Kind != "container" || run.Status != "running" {
		t.Errorf("running container = %+v", run)
	}
	// cpuDelta/sysDelta = 1e8/1e9 = 10% of host
	if run.CPUPct < 9.9 || run.CPUPct > 10.1 || run.CPUCores != 4 {
		t.Errorf("cpu = %v cores = %v", run.CPUPct, run.CPUCores)
	}
	if run.MemUsed != 1073741824-73741824 || run.MemTotal != 4294967296 {
		t.Errorf("mem = %d/%d", run.MemUsed, run.MemTotal)
	}
	if res[1].Status != "exited" || res[1].CPUPct != 0 {
		t.Errorf("stopped container = %+v", res[1])
	}
}

func TestESXiFetchAgainstSimulator(t *testing.T) {
	model := simulator.VPX()
	defer model.Remove()
	if err := model.Create(); err != nil {
		t.Fatal(err)
	}
	s := model.Service.NewServer()
	defer s.Close()

	pass, _ := s.URL.User.Password()
	a, err := newESXi(&config.Integration{
		ID: "esx", URL: s.URL.Scheme + "://" + s.URL.Host,
		Username: s.URL.User.Username(), Password: pass, InsecureTLS: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var nodes, vms, parented int
	for _, r := range res {
		switch r.Kind {
		case "node":
			nodes++
			if r.MemTotal == 0 {
				t.Errorf("node %s has no memory capacity", r.Name)
			}
		case "vm":
			vms++
			if r.ParentID != "" {
				parented++
			}
		}
	}
	if nodes == 0 || vms == 0 {
		t.Fatalf("nodes=%d vms=%d, want both > 0", nodes, vms)
	}
	if parented == 0 {
		t.Error("no VM linked to its host node")
	}
	// Second fetch reuses the cached SOAP session.
	if _, err := a.Fetch(context.Background()); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
}

func TestCollectorKeepsLastGoodSnapshot(t *testing.T) {
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"data":[{"type":"node","node":"pve1","status":"online","cpu":0.1,"maxcpu":4}]}`))
	}))
	defer srv.Close()

	cfgs := []config.Integration{{ID: "pve", Name: "PVE", Type: "proxmox",
		URL: srv.URL, TokenID: "t", TokenSecret: "s"}}
	adapters, err := Build(cfgs)
	if err != nil {
		t.Fatal(err)
	}
	col := NewCollector(cfgs, adapters)

	col.fetchOne(context.Background(), adapters[0])
	snap := col.Snapshot()[0]
	if snap.Error != "" || len(snap.Resources) != 1 {
		t.Fatalf("first fetch: %+v", snap)
	}

	fail = true
	col.fetchOne(context.Background(), adapters[0])
	snap = col.Snapshot()[0]
	if snap.Error == "" {
		t.Fatal("expected error recorded")
	}
	if len(snap.Resources) != 1 {
		t.Fatalf("last good resources dropped: %+v", snap)
	}
}

func TestBuildRejectsBadEndpoint(t *testing.T) {
	_, err := newDocker(&config.Integration{ID: "dk", Endpoint: "ftp://nope"})
	if err == nil {
		t.Fatal("ftp endpoint accepted")
	}
	if _, err := url.Parse("://"); err == nil {
		_ = err
	}
}
