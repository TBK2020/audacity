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
	Listen    string     `yaml:"listen"`
	Auth      Auth       `yaml:"auth"`
	Defaults  Defaults   `yaml:"defaults"`
	Notifiers []Notifier `yaml:"notifiers"`
	Hosts     []Host     `yaml:"hosts"`
	Services  []Service  `yaml:"services"`
}

type Auth struct {
	// Password enables HTTP Basic auth for the UI and API when non-empty.
	Password string `yaml:"password"`
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
	ID      string `yaml:"id"`
	Name    string `yaml:"name"`
	Address string `yaml:"address"`
}

type Service struct {
	ID     string            `yaml:"id"`
	Name   string            `yaml:"name"`
	Group  string            `yaml:"group"`
	Icon   string            `yaml:"icon"`
	Host   string            `yaml:"host"`
	URLs   map[string]string `yaml:"urls"`
	Checks []Check           `yaml:"checks"`
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
