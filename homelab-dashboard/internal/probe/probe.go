// Package probe implements the individual check types (http, tcp, icmp, tls-cert).
package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"labdeck/internal/config"
)

// Result is the outcome of a single probe execution.
type Result struct {
	OK       bool
	Degraded bool // OK but above latency threshold / cert close to expiry
	Latency  time.Duration
	Detail   string
}

func Run(ctx context.Context, ck *config.Check) Result {
	ctx, cancel := context.WithTimeout(ctx, ck.Timeout.Std())
	defer cancel()
	switch ck.Type {
	case "http":
		return httpProbe(ctx, ck)
	case "tcp":
		return tcpProbe(ctx, ck)
	case "icmp":
		return icmpProbe(ctx, ck)
	case "tls-cert":
		return tlsCertProbe(ctx, ck)
	default:
		return Result{Detail: fmt.Sprintf("unknown check type %q", ck.Type)}
	}
}

var httpClient = &http.Client{
	// Each request carries its own context timeout; avoid following endless redirects.
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	},
	Transport: &http.Transport{
		// Homelab services are overwhelmingly self-signed; reachability is what
		// this check asserts. Certificate hygiene is the tls-cert check's job.
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		DisableKeepAlives: true,
	},
}

func httpProbe(ctx context.Context, ck *config.Check) Result {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ck.URL, nil)
	if err != nil {
		return Result{Detail: err.Error()}
	}
	req.Header.Set("User-Agent", "labdeck/0.1")
	start := time.Now()
	resp, err := httpClient.Do(req)
	latency := time.Since(start)
	if err != nil {
		return Result{Latency: latency, Detail: trimErr(err)}
	}
	defer resp.Body.Close()

	if ck.ExpectStatus != 0 {
		if resp.StatusCode != ck.ExpectStatus {
			return Result{Latency: latency, Detail: fmt.Sprintf("status %d, want %d", resp.StatusCode, ck.ExpectStatus)}
		}
	} else if resp.StatusCode >= 400 {
		return Result{Latency: latency, Detail: fmt.Sprintf("status %d", resp.StatusCode)}
	}
	if ck.Keyword != "" {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if !strings.Contains(string(body), ck.Keyword) {
			return Result{Latency: latency, Detail: fmt.Sprintf("keyword %q not found", ck.Keyword)}
		}
	}
	return withLatencyGrade(ck, Result{OK: true, Latency: latency, Detail: fmt.Sprintf("status %d", resp.StatusCode)})
}

func tcpProbe(ctx context.Context, ck *config.Check) Result {
	var d net.Dialer
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", ck.Address)
	latency := time.Since(start)
	if err != nil {
		return Result{Latency: latency, Detail: trimErr(err)}
	}
	conn.Close()
	return withLatencyGrade(ck, Result{OK: true, Latency: latency, Detail: "connected"})
}

// icmpProbe shells out to the system ping: raw ICMP sockets need CAP_NET_RAW and
// unprivileged UDP ping needs a sysctl, while /bin/ping is setuid/setcap on
// effectively every Linux distro and BusyBox image.
func icmpProbe(ctx context.Context, ck *config.Check) Result {
	waitSec := int(ck.Timeout.Std().Seconds())
	if waitSec < 1 {
		waitSec = 1
	}
	start := time.Now()
	out, err := exec.CommandContext(ctx, "ping", "-c", "1", "-W", fmt.Sprint(waitSec), ck.Address).CombinedOutput()
	latency := time.Since(start)
	if err != nil {
		detail := trimErr(err)
		if len(out) > 0 {
			detail = lastLine(string(out))
		}
		return Result{Latency: latency, Detail: detail}
	}
	return withLatencyGrade(ck, Result{OK: true, Latency: latency, Detail: "ping ok"})
}

func tlsCertProbe(ctx context.Context, ck *config.Check) Result {
	addr := ck.Address
	if addr == "" {
		u, err := url.Parse(ck.URL)
		if err != nil {
			return Result{Detail: err.Error()}
		}
		addr = u.Host
		if u.Port() == "" {
			addr = net.JoinHostPort(u.Hostname(), "443")
		}
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
		addr = net.JoinHostPort(addr, "443")
	}
	var d net.Dialer
	start := time.Now()
	rawConn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return Result{Latency: time.Since(start), Detail: trimErr(err)}
	}
	defer rawConn.Close()
	// Expiry is what we assert; chain trust would reject homelab self-signed CAs.
	conn := tls.Client(rawConn, &tls.Config{ServerName: host, InsecureSkipVerify: true})
	if err := conn.HandshakeContext(ctx); err != nil {
		return Result{Latency: time.Since(start), Detail: trimErr(err)}
	}
	latency := time.Since(start)
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return Result{Latency: latency, Detail: "no peer certificate"}
	}
	expiry := certs[0].NotAfter
	daysLeft := int(time.Until(expiry).Hours() / 24)
	detail := fmt.Sprintf("cert expires %s (%dd)", expiry.Format("2006-01-02"), daysLeft)
	if daysLeft < 0 {
		return Result{Latency: latency, Detail: "certificate expired: " + detail}
	}
	if daysLeft <= ck.CertWarnDays {
		return Result{OK: true, Degraded: true, Latency: latency, Detail: detail}
	}
	return Result{OK: true, Latency: latency, Detail: detail}
}

func withLatencyGrade(ck *config.Check, r Result) Result {
	if ck.DegradedLatency > 0 && r.Latency > ck.DegradedLatency.Std() {
		r.Degraded = true
		r.Detail += fmt.Sprintf(" (slow: %dms)", r.Latency.Milliseconds())
	}
	return r
}

func trimErr(err error) string {
	s := err.Error()
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
