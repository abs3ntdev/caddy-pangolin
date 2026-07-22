package caddypangolin

import (
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	caddy.RegisterModule(HTTPSBackendMatcher{})
}

// HTTPSBackendMatcher matches requests whose Pangolin resource has at least
// one enabled target with method "https". Use it to route those hosts through
// a reverse_proxy with an HTTPS transport.
type HTTPSBackendMatcher struct {
	Endpoint           string         `json:"endpoint,omitempty"`
	APIKey             string         `json:"api_key,omitempty"`
	OrgID              string         `json:"org_id,omitempty"`
	Refresh            caddy.Duration `json:"refresh,omitempty"`
	InsecureSkipVerify bool           `json:"insecure_skip_verify,omitempty"`

	cfg    Config
	poller *poller
}

func (HTTPSBackendMatcher) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.pangolin_https_backend",
		New: func() caddy.Module { return new(HTTPSBackendMatcher) },
	}
}

func (m *HTTPSBackendMatcher) Provision(ctx caddy.Context) error {
	cfg, err := buildConfig(m.Endpoint, m.APIKey, m.OrgID, m.Refresh, m.InsecureSkipVerify)
	if err != nil {
		return err
	}
	m.cfg = cfg
	m.poller, err = getPoller(ctx, cfg)
	return err
}

func (m *HTTPSBackendMatcher) Cleanup() error {
	if m.poller != nil {
		releasePoller(m.cfg)
	}
	return nil
}

func (m *HTTPSBackendMatcher) Match(r *http.Request) bool {
	ok, _ := m.MatchWithError(r)
	return ok
}

func (m *HTTPSBackendMatcher) MatchWithError(r *http.Request) (bool, error) {
	snap := m.poller.current()
	if snap == nil {
		return false, nil
	}
	entry, ok := snap.lookup(r.Host)
	if !ok {
		return false, nil
	}
	for _, b := range entry.Backends {
		if b.HTTPS {
			return true, nil
		}
	}
	return false, nil
}

func (m *HTTPSBackendMatcher) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
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

var (
	_ caddy.Provisioner        = (*HTTPSBackendMatcher)(nil)
	_ caddy.CleanerUpper       = (*HTTPSBackendMatcher)(nil)
	_ caddyhttp.RequestMatcher = (*HTTPSBackendMatcher)(nil)
	_ caddyfile.Unmarshaler    = (*HTTPSBackendMatcher)(nil)
)
