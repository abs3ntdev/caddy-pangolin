package caddypangolin

import (
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	caddy.RegisterModule(HTTPSBackendMatcher{})
	caddy.RegisterModule(RemoteMatcher{})
}

// HTTPSBackendMatcher matches requests whose Pangolin resource has at least
// one locally reachable target with method "https". Use it to route those
// hosts through a reverse_proxy with an HTTPS transport.
type HTTPSBackendMatcher struct {
	ModuleConfig
}

func (HTTPSBackendMatcher) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.pangolin_https_backend",
		New: func() caddy.Module { return new(HTTPSBackendMatcher) },
	}
}

func (m *HTTPSBackendMatcher) Provision(ctx caddy.Context) error {
	return m.provision(ctx)
}

func (m *HTTPSBackendMatcher) Cleanup() error {
	return m.cleanup()
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
	return m.unmarshalCaddyfile(d)
}

// RemoteMatcher matches requests whose Pangolin resource exists but has no
// locally reachable targets (all its targets live on sites excluded by the
// `sites` filter). Use it to route those hosts back through the public
// Pangolin instance.
type RemoteMatcher struct {
	ModuleConfig
}

func (RemoteMatcher) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.pangolin_remote",
		New: func() caddy.Module { return new(RemoteMatcher) },
	}
}

func (m *RemoteMatcher) Provision(ctx caddy.Context) error {
	return m.provision(ctx)
}

func (m *RemoteMatcher) Cleanup() error {
	return m.cleanup()
}

func (m *RemoteMatcher) Match(r *http.Request) bool {
	ok, _ := m.MatchWithError(r)
	return ok
}

func (m *RemoteMatcher) MatchWithError(r *http.Request) (bool, error) {
	snap := m.poller.current()
	if snap == nil {
		return false, nil
	}
	entry, ok := snap.lookup(r.Host)
	if !ok {
		return false, nil
	}
	return entry.Remote && len(entry.Backends) == 0, nil
}

func (m *RemoteMatcher) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	return m.unmarshalCaddyfile(d)
}

var (
	_ caddy.Provisioner        = (*HTTPSBackendMatcher)(nil)
	_ caddy.CleanerUpper       = (*HTTPSBackendMatcher)(nil)
	_ caddyhttp.RequestMatcher = (*HTTPSBackendMatcher)(nil)
	_ caddyfile.Unmarshaler    = (*HTTPSBackendMatcher)(nil)
	_ caddy.Provisioner        = (*RemoteMatcher)(nil)
	_ caddy.CleanerUpper       = (*RemoteMatcher)(nil)
	_ caddyhttp.RequestMatcher = (*RemoteMatcher)(nil)
	_ caddyfile.Unmarshaler    = (*RemoteMatcher)(nil)
)
