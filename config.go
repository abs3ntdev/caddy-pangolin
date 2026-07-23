package caddypangolin

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// ModuleConfig holds the user-facing configuration shared by all
// caddy-pangolin modules.
type ModuleConfig struct {
	// Base URL of the Pangolin integration API, e.g. https://pangolin-api.example.com
	Endpoint string `json:"endpoint,omitempty"`

	// API key in the form "<id>.<secret>" (sent as Authorization: Bearer).
	APIKey string `json:"api_key,omitempty"`

	// The Pangolin organization ID to list resources from.
	OrgID string `json:"org_id,omitempty"`

	// How often to refresh the resource map. Default: 60s.
	Refresh caddy.Duration `json:"refresh,omitempty"`

	// Skip TLS verification when talking to the Pangolin API.
	InsecureSkipVerify bool `json:"insecure_skip_verify,omitempty"`

	// Sites whose targets are locally reachable (name or niceId,
	// case-insensitive). Targets on other sites are treated as remote.
	// Empty means all sites are local.
	Sites []string `json:"sites,omitempty"`

	// Resolvers are DNS server addresses (port 53 assumed) used to resolve
	// the Pangolin endpoint instead of the system resolver. Set this when
	// split-horizon DNS would resolve the endpoint back to this Caddy.
	Resolvers []string `json:"resolvers,omitempty"`

	cfg    Config
	poller *poller
}

func (m *ModuleConfig) provision(ctx caddy.Context) error {
	repl := caddy.NewReplacer()
	cfg := Config{
		Endpoint:           repl.ReplaceAll(m.Endpoint, ""),
		APIKey:             repl.ReplaceAll(m.APIKey, ""),
		OrgID:              repl.ReplaceAll(m.OrgID, ""),
		Refresh:            time.Duration(m.Refresh),
		InsecureSkipVerify: m.InsecureSkipVerify,
	}
	for _, s := range m.Sites {
		cfg.Sites = append(cfg.Sites, repl.ReplaceAll(s, ""))
	}
	for _, raw := range m.Resolvers {
		r, err := normalizeResolver(repl.ReplaceAll(raw, ""))
		if err != nil {
			return fmt.Errorf("invalid resolver %q: %w", raw, err)
		}
		cfg.Resolvers = append(cfg.Resolvers, r)
	}
	if cfg.Endpoint == "" {
		return fmt.Errorf("endpoint is required")
	}
	if cfg.APIKey == "" {
		return fmt.Errorf("api_key is required")
	}
	if cfg.OrgID == "" {
		return fmt.Errorf("org_id is required")
	}
	if cfg.Refresh <= 0 {
		cfg.Refresh = 60 * time.Second
	}
	m.cfg = cfg
	if err := initMetrics(ctx.GetMetricsRegistry()); err != nil {
		return fmt.Errorf("registering metrics: %w", err)
	}
	var err error
	m.poller, err = getPoller(ctx, cfg)
	return err
}

func normalizeResolver(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("address is empty")
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return net.JoinHostPort(value, "53"), nil
	}
	if host == "" {
		return "", fmt.Errorf("host is empty")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("port must be between 1 and 65535")
	}
	return net.JoinHostPort(host, port), nil
}

func (m *ModuleConfig) cleanup() error {
	if m.poller != nil {
		releasePoller(m.cfg)
	}
	return nil
}

// unmarshalCaddyfile parses the shared option block:
//
//	{
//	    endpoint <url>
//	    api_key <id.secret>
//	    org_id <org>
//	    refresh <duration>
//	    sites <name...>
//	    insecure_skip_verify
//	}
func (m *ModuleConfig) unmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	for d.NextBlock(0) {
		switch d.Val() {
		case "endpoint":
			if !d.AllArgs(&m.Endpoint) {
				return d.ArgErr()
			}
		case "api_key":
			if !d.AllArgs(&m.APIKey) {
				return d.ArgErr()
			}
		case "org_id":
			if !d.AllArgs(&m.OrgID) {
				return d.ArgErr()
			}
		case "refresh":
			var v string
			if !d.AllArgs(&v) {
				return d.ArgErr()
			}
			dur, err := caddy.ParseDuration(v)
			if err != nil {
				return d.Errf("parsing refresh duration: %v", err)
			}
			m.Refresh = caddy.Duration(dur)
		case "sites":
			args := d.RemainingArgs()
			if len(args) == 0 {
				return d.ArgErr()
			}
			m.Sites = append(m.Sites, args...)
		case "resolvers":
			args := d.RemainingArgs()
			if len(args) == 0 {
				return d.ArgErr()
			}
			m.Resolvers = append(m.Resolvers, args...)
		case "insecure_skip_verify":
			if d.NextArg() {
				return d.ArgErr()
			}
			m.InsecureSkipVerify = true
		default:
			return d.Errf("unrecognized option '%s'", d.Val())
		}
	}
	return nil
}
