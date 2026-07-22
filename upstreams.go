package caddypangolin

import (
	"fmt"
	"net/http"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
)

func init() {
	caddy.RegisterModule(Upstreams{})
}

type Upstreams struct {
	// Base URL of the Pangolin integration API, e.g. https://pangolin.example.com:3003
	Endpoint string `json:"endpoint,omitempty"`

	// API key in the form "<id>.<secret>" (sent as Authorization: Bearer).
	APIKey string `json:"api_key,omitempty"`

	// The Pangolin organization ID to list resources from.
	OrgID string `json:"org_id,omitempty"`

	// How often to refresh the resource map. Default: 60s.
	Refresh caddy.Duration `json:"refresh,omitempty"`

	// Skip TLS verification when talking to the Pangolin API.
	InsecureSkipVerify bool `json:"insecure_skip_verify,omitempty"`

	cfg    Config
	poller *poller
}

func (Upstreams) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.reverse_proxy.upstreams.pangolin",
		New: func() caddy.Module { return new(Upstreams) },
	}
}

func buildConfig(endpoint, apiKey, orgID string, refresh caddy.Duration, insecure bool) (Config, error) {
	repl := caddy.NewReplacer()
	cfg := Config{
		Endpoint:           repl.ReplaceAll(endpoint, ""),
		APIKey:             repl.ReplaceAll(apiKey, ""),
		OrgID:              repl.ReplaceAll(orgID, ""),
		Refresh:            time.Duration(refresh),
		InsecureSkipVerify: insecure,
	}
	if cfg.Endpoint == "" {
		return cfg, fmt.Errorf("endpoint is required")
	}
	if cfg.APIKey == "" {
		return cfg, fmt.Errorf("api_key is required")
	}
	if cfg.OrgID == "" {
		return cfg, fmt.Errorf("org_id is required")
	}
	if cfg.Refresh <= 0 {
		cfg.Refresh = 60 * time.Second
	}
	return cfg, nil
}

func (u *Upstreams) Provision(ctx caddy.Context) error {
	cfg, err := buildConfig(u.Endpoint, u.APIKey, u.OrgID, u.Refresh, u.InsecureSkipVerify)
	if err != nil {
		return err
	}
	u.cfg = cfg
	u.poller, err = getPoller(ctx, cfg)
	return err
}

func (u *Upstreams) Cleanup() error {
	if u.poller != nil {
		releasePoller(u.cfg)
	}
	return nil
}

func (u *Upstreams) GetUpstreams(r *http.Request) ([]*reverseproxy.Upstream, error) {
	snap := u.poller.current()
	if snap == nil {
		return nil, fmt.Errorf("pangolin resource map not loaded yet")
	}
	entry, ok := snap.lookup(r.Host)
	if !ok {
		return nil, fmt.Errorf("no pangolin resource for host %q", r.Host)
	}
	ups := make([]*reverseproxy.Upstream, 0, len(entry.Backends))
	for _, b := range entry.Backends {
		ups = append(ups, &reverseproxy.Upstream{Dial: b.Dial})
	}
	return ups, nil
}

// UnmarshalCaddyfile sets up the module from Caddyfile tokens. Syntax:
//
//	dynamic pangolin {
//	    endpoint <url>
//	    api_key <id.secret>
//	    org_id <org>
//	    refresh <duration>
//	    insecure_skip_verify
//	}
func (u *Upstreams) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	for d.NextBlock(0) {
		switch d.Val() {
		case "endpoint":
			if !d.AllArgs(&u.Endpoint) {
				return d.ArgErr()
			}
		case "api_key":
			if !d.AllArgs(&u.APIKey) {
				return d.ArgErr()
			}
		case "org_id":
			if !d.AllArgs(&u.OrgID) {
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
			u.Refresh = caddy.Duration(dur)
		case "insecure_skip_verify":
			if d.NextArg() {
				return d.ArgErr()
			}
			u.InsecureSkipVerify = true
		default:
			return d.Errf("unrecognized option '%s'", d.Val())
		}
	}
	return nil
}

var (
	_ caddy.Provisioner           = (*Upstreams)(nil)
	_ caddy.CleanerUpper          = (*Upstreams)(nil)
	_ reverseproxy.UpstreamSource = (*Upstreams)(nil)
	_ caddyfile.Unmarshaler       = (*Upstreams)(nil)
)
