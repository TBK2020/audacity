---
name: verify
description: Build, run and smoke-test the labdeck server end-to-end (applies to homelab-dashboard/).
---

# Verifying labdeck

Build: `cd homelab-dashboard && go build -o /tmp/labdeck ./cmd/labdeck`

Smoke recipe (all local, no external deps):
1. Start a probe target: `python3 -m http.server 9911 --bind 127.0.0.1 &`
2. Write a minimal config with fast thresholds (`interval: 2s`, `failure_threshold: 2`,
   `success_threshold: 1`); include one service pointing at :9911 (should go up),
   one tcp check at an unused port (should go down), and a `webhook` notifier at a
   local listener to capture notifications.
3. Run: `LABDECK_PASSWORD=x /tmp/labdeck -config cfg.yaml -db /tmp/l.db -listen :8399`
4. Wait ~8s, then assert:
   - `curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:8399/api/summary` → 401
   - `curl -u admin:x .../api/summary` → statuses up/down as configured
   - `.../api/events`, `.../api/services/<id>/uptime` return rows
   - webhook listener received down (and up after you start a listener on the dead port)
5. UI: Playwright with `executablePath: '/opt/pw-browsers/chromium'` and
   `httpCredentials`, screenshot `/`, check `#conn-text` shows 实时 (WebSocket live)
   and no console errors.

Gotchas:
- The icmp check shells out to `ping`; not present in this sandbox (expected fail —
  the Dockerfile installs iputils-ping).
- Probe start has per-check jitter up to one interval; wait interval+jitter before
  asserting statuses.
