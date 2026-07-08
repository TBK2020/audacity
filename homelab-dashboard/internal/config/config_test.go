package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const sample = `
listen: ":9999"
defaults:
  interval: 15s
  failure_threshold: 2
notifiers:
  - type: telegram
    token: ${LABDECK_TEST_TOKEN}
    chat_id: "42"
services:
  - id: jellyfin
    name: Jellyfin
    group: 媒体
    urls:
      internal: http://jf.lan:8096
    checks:
      - type: http
        url: http://jf.lan:8096/health
      - type: tcp
        address: jf.lan:8096
        interval: 5s
  - id: router
    checks:
      - type: icmp
        address: 10.0.0.1
`

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "services.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaultsAndEnvExpansion(t *testing.T) {
	t.Setenv("LABDECK_TEST_TOKEN", "sekrit")
	cfg, err := Load(writeTemp(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9999" {
		t.Errorf("listen = %q", cfg.Listen)
	}
	if cfg.Notifiers[0].Token != "sekrit" {
		t.Errorf("env not expanded: %q", cfg.Notifiers[0].Token)
	}

	jf := cfg.Services[0]
	httpCk, tcpCk := jf.Checks[0], jf.Checks[1]
	if httpCk.ID != "jellyfin/http" || tcpCk.ID != "jellyfin/tcp" {
		t.Errorf("check ids = %q, %q", httpCk.ID, tcpCk.ID)
	}
	if httpCk.Interval.Std() != 15*time.Second {
		t.Errorf("default interval not applied: %v", httpCk.Interval.Std())
	}
	if tcpCk.Interval.Std() != 5*time.Second {
		t.Errorf("explicit interval overridden: %v", tcpCk.Interval.Std())
	}
	if httpCk.FailureThreshold != 2 {
		t.Errorf("default failure_threshold not applied: %d", httpCk.FailureThreshold)
	}
	if httpCk.SuccessThreshold != 2 { // built-in default
		t.Errorf("built-in success_threshold not applied: %d", httpCk.SuccessThreshold)
	}

	router := cfg.Services[1]
	if router.Name != "router" || router.Group != "默认" {
		t.Errorf("name/group fallback: %q %q", router.Name, router.Group)
	}
}

func TestLoadRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"duplicate id": `
services:
  - id: a
  - id: a
`,
		"missing url": `
services:
  - id: a
    checks:
      - type: http
`,
		"unknown type": `
services:
  - id: a
    checks:
      - type: gopher
        url: http://x
`,
	}
	for name, content := range cases {
		if _, err := Load(writeTemp(t, content)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}
