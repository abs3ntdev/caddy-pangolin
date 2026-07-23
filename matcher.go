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

// CaddyModule returns the Caddy module information.
func (HTTPSBackendMatcher) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.pangolin_https_backend",
		New: func() caddy.Module { return new(HTTPSBackendMatcher) },
	}
}

// Provision implements caddy.Provisioner.
func (m *HTTPSBackendMatcher) Provision(ctx caddy.Context) error {
	return m.provision(ctx)
}

// Cleanup implements caddy.CleanerUpper.
func (m *HTTPSBackendMatcher) Cleanup() error {
	return m.cleanup()
}

// Match implements caddyhttp.RequestMatcher.
func (m *HTTPSBackendMatcher) Match(r *http.Request) bool {
	ok, _ := m.MatchWithError(r)
	return ok
}

// MatchWithError reports whether the request's host maps to a Pangolin
// resource with at least one locally reachable HTTPS target.
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

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
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

// CaddyModule returns the Caddy module information.
func (RemoteMatcher) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.matchers.pangolin_remote",
		New: func() caddy.Module { return new(RemoteMatcher) },
	}
}

// Provision implements caddy.Provisioner.
func (m *RemoteMatcher) Provision(ctx caddy.Context) error {
	return m.provision(ctx)
}

// Cleanup implements caddy.CleanerUpper.
func (m *RemoteMatcher) Cleanup() error {
	return m.cleanup()
}

// Match implements caddyhttp.RequestMatcher.
func (m *RemoteMatcher) Match(r *http.Request) bool {
	ok, _ := m.MatchWithError(r)
	return ok
}

// MatchWithError reports whether the request's host maps to a Pangolin
// resource that has no locally reachable targets.
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

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
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
