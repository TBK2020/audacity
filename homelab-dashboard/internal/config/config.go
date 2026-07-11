// Package config loads and validates the labdeck YAML configuration.
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

type Config struct {
	Listen    string      `yaml:"listen"`
	Auth      Auth        `yaml:"auth"`
	Defaults  Defaults    `yaml:"defaults"`
	SSH          SSHSettings   `yaml:"ssh"`
	Notifiers    []Notifier    `yaml:"notifiers"`
	Hosts        []Host        `yaml:"hosts"`
	Services     []Service     `yaml:"services"`
	Integrations []Integration `yaml:"integrations"`
}

// Integration is a deep adapter pulling resource inventory (nodes/VMs/containers).
type Integration struct {
	ID       string   `yaml:"id"`
	Type     string   `yaml:"type"` // proxmox | docker | esxi
	Name     string   `yaml:"name"`
	Interval Duration `yaml:"interval"`

	URL         string `yaml:"url"`          // proxmox / esxi
	InsecureTLS bool   `yaml:"insecure_tls"` // self-signed homelab APIs

	TokenID     string `yaml:"token_id"`     // proxmox: user@realm!tokenname
	TokenSecret string `yaml:"token_secret"` // proxmox: token uuid

	Endpoint string `yaml:"endpoint"` // docker: unix:///var/run/docker.sock or tcp://host:2375

	Username string `yaml:"username"` // esxi
	Password string `yaml:"password"` // esxi
}

type SSHSettings struct {
	IdleTimeout Duration `yaml:"idle_timeout"` // terminal auto-disconnect
	MaxSessions int      `yaml:"max_sessions"` // concurrent terminal cap
	RequireTOTP bool     `yaml:"require_totp"` // sudo-mode: TOTP before every terminal
}

type Auth struct {
	// Password enables HTTP Basic auth for the UI and API when non-empty.
	Password string `yaml:"password"`
	// TOTPSecret (base32) backs the terminal's second-factor prompt.
	// Generate one with `labdeck -gen-totp`.
	TOTPSecret string `yaml:"totp_secret"`
}

type Defaults struct {
	Interval         Duration `yaml:"interval"`
	Timeout          Duration `yaml:"timeout"`
	FailureThreshold int      `yaml:"failure_threshold"`
	SuccessThreshold int      `yaml:"success_threshold"`
	CertWarnDays     int      `yaml:"cert_warn_days"`
}

type Notifier struct {
	Type string `yaml:"type"` // telegram | webhook
	// Telegram
	Token  string `yaml:"token"`
	ChatID string `yaml:"chat_id"`
	// Webhook
	URL string `yaml:"url"`
}

type Host struct {
	ID      string   `yaml:"id"`
	Name    string   `yaml:"name"`
	Address string   `yaml:"address"`
	SSH     *HostSSH `yaml:"ssh"`
}

type HostSSH struct {
	Port       int    `yaml:"port"`
	User       string `yaml:"user"`       // falls back to the credential's username
	Credential string `yaml:"credential"` // id in the encrypted credential store
}

type Service struct {
	ID     string            `yaml:"id"`
	Name   string            `yaml:"name"`
	Group  string            `yaml:"group"`
	Icon   string            `yaml:"icon"`
	Host   string            `yaml:"host"`
	URLs   map[string]string `yaml:"urls"`
	Checks []Check           `yaml:"checks"`
	// DependsOn lists service ids this one is behind (router, hypervisor…).
	// While a dependency is down this service reports "unreachable" and does
	// not fire its own alerts — no notification storm for one dead router.
	DependsOn []string `yaml:"depends_on"`
}

type Check struct {
	// ID is derived at load time: <service>/<type>[-<n>].
	ID   string `yaml:"-"`
	Type string `yaml:"type"` // http | tcp | icmp | tls-cert

	URL     string `yaml:"url"`     // http, tls-cert (or Address)
	Address string `yaml:"address"` // tcp host:port, icmp host, tls-cert host:port

	Interval Duration `yaml:"interval"`
	Timeout  Duration `yaml:"timeout"`

	ExpectStatus    int      `yaml:"expect_status"`    // http, 0 = any 2xx/3xx
	Keyword         string   `yaml:"keyword"`          // http body must contain
	DegradedLatency Duration `yaml:"degraded_latency"` // above this an UP check reports DEGRADED
	CertWarnDays    int      `yaml:"cert_warn_days"`   // tls-cert

	FailureThreshold int `yaml:"failure_threshold"`
	SuccessThreshold int `yaml:"success_threshold"`
}

var envRef = regexp.MustCompile(`\$\{(\w+)\}`)

// Load reads path, expands ${ENV_VAR} references, applies defaults and validates.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	expanded := envRef.ReplaceAllFunc(raw, func(m []byte) []byte {
		name := envRef.FindSubmatch(m)[1]
		return []byte(os.Getenv(string(name)))
	})
	var cfg Config
	if err := yaml.Unmarshal(expanded, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.finalize(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) finalize() error {
	if c.Listen == "" {
		c.Listen = ":8383"
	}
	d := &c.Defaults
	if d.Interval == 0 {
		d.Interval = Duration(30 * time.Second)
	}
	if d.Timeout == 0 {
		d.Timeout = Duration(10 * time.Second)
	}
	if d.FailureThreshold == 0 {
		d.FailureThreshold = 3
	}
	if d.SuccessThreshold == 0 {
		d.SuccessThreshold = 2
	}
	if d.CertWarnDays == 0 {
		d.CertWarnDays = 14
	}
	if c.SSH.IdleTimeout == 0 {
		c.SSH.IdleTimeout = Duration(15 * time.Minute)
	}
	if c.SSH.MaxSessions == 0 {
		c.SSH.MaxSessions = 5
	}

	hostSeen := map[string]bool{}
	for hi := range c.Hosts {
		h := &c.Hosts[hi]
		if h.ID == "" {
			return fmt.Errorf("host #%d: missing id", hi)
		}
		if hostSeen[h.ID] {
			return fmt.Errorf("duplicate host id %q", h.ID)
		}
		hostSeen[h.ID] = true
		if h.SSH != nil {
			if h.SSH.Port == 0 {
				h.SSH.Port = 22
			}
			if h.SSH.Credential == "" {
				return fmt.Errorf("host %s: ssh requires credential (id in the credential store)", h.ID)
			}
			if h.Address == "" {
				return fmt.Errorf("host %s: ssh requires address", h.ID)
			}
		}
	}

	intSeen := map[string]bool{}
	for ii := range c.Integrations {
		in := &c.Integrations[ii]
		if in.ID == "" {
			return fmt.Errorf("integration #%d: missing id", ii)
		}
		if intSeen[in.ID] {
			return fmt.Errorf("duplicate integration id %q", in.ID)
		}
		intSeen[in.ID] = true
		if in.Name == "" {
			in.Name = in.ID
		}
		if in.Interval == 0 {
			in.Interval = Duration(10 * time.Second)
		}
		switch in.Type {
		case "proxmox":
			if in.URL == "" || in.TokenID == "" || in.TokenSecret == "" {
				return fmt.Errorf("integration %s: proxmox requires url, token_id, token_secret", in.ID)
			}
		case "docker":
			if in.Endpoint == "" {
				return fmt.Errorf("integration %s: docker requires endpoint", in.ID)
			}
		case "esxi":
			if in.URL == "" || in.Username == "" || in.Password == "" {
				return fmt.Errorf("integration %s: esxi requires url, username, password", in.ID)
			}
		default:
			return fmt.Errorf("integration %s: unknown type %q", in.ID, in.Type)
		}
	}

	seen := map[string]bool{}
	for si := range c.Services {
		svc := &c.Services[si]
		if svc.ID == "" {
			return fmt.Errorf("service #%d: missing id", si)
		}
		if seen[svc.ID] {
			return fmt.Errorf("duplicate service id %q", svc.ID)
		}
		seen[svc.ID] = true
		if svc.Name == "" {
			svc.Name = svc.ID
		}
		if svc.Group == "" {
			svc.Group = "默认"
		}
		typeCount := map[string]int{}
		for ci := range svc.Checks {
			ck := &svc.Checks[ci]
			typeCount[ck.Type]++
			ck.ID = fmt.Sprintf("%s/%s", svc.ID, ck.Type)
			if n := typeCount[ck.Type]; n > 1 {
				ck.ID = fmt.Sprintf("%s-%d", ck.ID, n)
			}
			if ck.Interval == 0 {
				ck.Interval = d.Interval
			}
			if ck.Timeout == 0 {
				ck.Timeout = d.Timeout
			}
			if ck.FailureThreshold == 0 {
				ck.FailureThreshold = d.FailureThreshold
			}
			if ck.SuccessThreshold == 0 {
				ck.SuccessThreshold = d.SuccessThreshold
			}
			if ck.CertWarnDays == 0 {
				ck.CertWarnDays = d.CertWarnDays
			}
			if err := ck.validate(); err != nil {
				return fmt.Errorf("service %s check %s: %w", svc.ID, ck.ID, err)
			}
		}
	}

	return c.validateDependencies(seen)
}

// validateDependencies checks depends_on references and rejects cycles.
func (c *Config) validateDependencies(serviceIDs map[string]bool) error {
	deps := map[string][]string{}
	for _, svc := range c.Services {
		for _, dep := range svc.DependsOn {
			if dep == svc.ID {
				return fmt.Errorf("service %s depends on itself", svc.ID)
			}
			if !serviceIDs[dep] {
				return fmt.Errorf("service %s depends on unknown service %q", svc.ID, dep)
			}
		}
		deps[svc.ID] = svc.DependsOn
	}
	const (
		white = 0 // unvisited
		gray  = 1 // on the current DFS path
		black = 2 // done
	)
	color := map[string]int{}
	var visit func(id string) error
	visit = func(id string) error {
		switch color[id] {
		case gray:
			return fmt.Errorf("dependency cycle involving service %q", id)
		case black:
			return nil
		}
		color[id] = gray
		for _, dep := range deps[id] {
			if err := visit(dep); err != nil {
				return err
			}
		}
		color[id] = black
		return nil
	}
	for id := range deps {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func (ck *Check) validate() error {
	switch ck.Type {
	case "http":
		if ck.URL == "" {
			return fmt.Errorf("http check requires url")
		}
	case "tcp":
		if ck.Address == "" {
			return fmt.Errorf("tcp check requires address (host:port)")
		}
	case "icmp":
		if ck.Address == "" {
			return fmt.Errorf("icmp check requires address (host)")
		}
	case "tls-cert":
		if ck.Address == "" && ck.URL == "" {
			return fmt.Errorf("tls-cert check requires address (host:port) or url")
		}
	default:
		return fmt.Errorf("unknown check type %q", ck.Type)
	}
	return nil
}
