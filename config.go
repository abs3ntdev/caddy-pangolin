package caddypangolin

import (
	"fmt"
	"net"
	"net/url"
	"slices"
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
	Refresh        caddy.Duration `json:"refresh,omitempty"`
	MethodRefresh  caddy.Duration `json:"method_refresh,omitempty"`
	MaxStale       caddy.Duration `json:"max_stale,omitempty"`
	InitialTimeout caddy.Duration `json:"initial_timeout,omitempty"`

	// Skip TLS verification when talking to the Pangolin API.
	InsecureSkipVerify bool `json:"insecure_skip_verify,omitempty"`
	AllowHTTP          bool `json:"allow_http,omitempty"`

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
	cfg, err := m.resolvedConfig()
	if err != nil {
		return err
	}
	m.cfg = cfg
	if err := initMetrics(ctx.GetMetricsRegistry()); err != nil {
		return fmt.Errorf("registering metrics: %w", err)
	}
	m.poller, err = getPoller(ctx, cfg)
	if err != nil {
		return err
	}
	if err := m.poller.waitReady(cfg.InitialTimeout); err != nil {
		releasePoller(cfg)
		m.poller = nil
		return err
	}
	return nil
}

func (m *ModuleConfig) resolvedConfig() (Config, error) {
	repl := caddy.NewReplacer()
	cfg := Config{
		Endpoint:           repl.ReplaceAll(m.Endpoint, ""),
		APIKey:             repl.ReplaceAll(m.APIKey, ""),
		OrgID:              repl.ReplaceAll(m.OrgID, ""),
		Refresh:            time.Duration(m.Refresh),
		MethodRefresh:      time.Duration(m.MethodRefresh),
		MaxStale:           time.Duration(m.MaxStale),
		InitialTimeout:     time.Duration(m.InitialTimeout),
		InsecureSkipVerify: m.InsecureSkipVerify,
		AllowHTTP:          m.AllowHTTP,
	}
	for _, s := range m.Sites {
		s = strings.ToLower(strings.TrimSpace(repl.ReplaceAll(s, "")))
		if s != "" && !slices.Contains(cfg.Sites, s) {
			cfg.Sites = append(cfg.Sites, s)
		}
	}
	slices.Sort(cfg.Sites)
	for _, raw := range m.Resolvers {
		r, err := normalizeResolver(repl.ReplaceAll(raw, ""))
		if err != nil {
			return Config{}, fmt.Errorf("invalid resolver %q: %w", raw, err)
		}
		if !slices.Contains(cfg.Resolvers, r) {
			cfg.Resolvers = append(cfg.Resolvers, r)
		}
	}
	slices.Sort(cfg.Resolvers)
	if cfg.Endpoint == "" {
		return Config{}, fmt.Errorf("endpoint is required")
	}
	if cfg.APIKey == "" {
		return Config{}, fmt.Errorf("api_key is required")
	}
	if cfg.OrgID == "" {
		return Config{}, fmt.Errorf("org_id is required")
	}
	parsedEndpoint, err := url.Parse(cfg.Endpoint)
	if err != nil || parsedEndpoint.Host == "" {
		return Config{}, fmt.Errorf("endpoint must be an absolute URL")
	}
	if parsedEndpoint.Scheme != "https" && (parsedEndpoint.Scheme != "http" || !cfg.AllowHTTP) {
		return Config{}, fmt.Errorf("endpoint must use https (set allow_http to explicitly permit http)")
	}
	if parsedEndpoint.User != nil || parsedEndpoint.RawQuery != "" || parsedEndpoint.Fragment != "" {
		return Config{}, fmt.Errorf("endpoint must not contain user info, a query, or a fragment")
	}
	cfg.Endpoint = strings.TrimSuffix(parsedEndpoint.String(), "/")
	if cfg.Refresh <= 0 {
		cfg.Refresh = 60 * time.Second
	}
	if cfg.MethodRefresh <= 0 {
		cfg.MethodRefresh = 5 * time.Minute
	}
	if cfg.MaxStale < 0 {
		return Config{}, fmt.Errorf("max_stale must not be negative")
	}
	if cfg.InitialTimeout < 0 {
		return Config{}, fmt.Errorf("initial_timeout must not be negative")
	}
	return cfg, nil
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
		case "method_refresh":
			var v string
			if !d.AllArgs(&v) {
				return d.ArgErr()
			}
			dur, err := caddy.ParseDuration(v)
			if err != nil {
				return d.Errf("parsing method refresh duration: %v", err)
			}
			m.MethodRefresh = caddy.Duration(dur)
		case "max_stale":
			var v string
			if !d.AllArgs(&v) {
				return d.ArgErr()
			}
			dur, err := caddy.ParseDuration(v)
			if err != nil {
				return d.Errf("parsing max stale duration: %v", err)
			}
			m.MaxStale = caddy.Duration(dur)
		case "initial_timeout":
			var v string
			if !d.AllArgs(&v) {
				return d.ArgErr()
			}
			dur, err := caddy.ParseDuration(v)
			if err != nil {
				return d.Errf("parsing initial timeout duration: %v", err)
			}
			m.InitialTimeout = caddy.Duration(dur)
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
		case "allow_http":
			if d.NextArg() {
				return d.ArgErr()
			}
			m.AllowHTTP = true
		default:
			return d.Errf("unrecognized option '%s'", d.Val())
		}
	}
	return nil
}
