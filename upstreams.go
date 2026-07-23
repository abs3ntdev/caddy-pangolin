package caddypangolin

import (
	"fmt"
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
)

func init() {
	caddy.RegisterModule(Upstreams{})
}

// Upstreams is a dynamic upstream source that resolves the backend for a
// request by matching its Host against Pangolin resources.
type Upstreams struct {
	ModuleConfig
}

// CaddyModule returns the Caddy module information.
func (Upstreams) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.reverse_proxy.upstreams.pangolin",
		New: func() caddy.Module { return new(Upstreams) },
	}
}

// Provision implements caddy.Provisioner.
func (u *Upstreams) Provision(ctx caddy.Context) error {
	return u.provision(ctx)
}

// Cleanup implements caddy.CleanerUpper.
func (u *Upstreams) Cleanup() error {
	return u.cleanup()
}

// GetUpstreams implements reverseproxy.UpstreamSource by resolving the
// request's host against the current Pangolin resource map.
func (u *Upstreams) GetUpstreams(r *http.Request) ([]*reverseproxy.Upstream, error) {
	entry, ok, loaded := u.poller.lookupRequest(r)
	if !ok {
		if !loaded {
			return nil, fmt.Errorf("pangolin resource map not loaded yet")
		}
		return nil, fmt.Errorf("no pangolin resource for host %q", r.Host)
	}
	if len(entry.Backends) == 0 {
		return nil, fmt.Errorf("no locally reachable targets for host %q (remote site)", r.Host)
	}
	ups := make([]*reverseproxy.Upstream, 0, len(entry.Backends))
	preferHTTPS := false
	for _, b := range entry.Backends {
		preferHTTPS = preferHTTPS || b.HTTPS
	}
	for _, b := range entry.Backends {
		if b.HTTPS != preferHTTPS {
			continue
		}
		ups = append(ups, &reverseproxy.Upstream{Dial: b.Dial})
	}
	return ups, nil
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (u *Upstreams) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	return u.unmarshalCaddyfile(d)
}

var (
	_ caddy.Provisioner           = (*Upstreams)(nil)
	_ caddy.CleanerUpper          = (*Upstreams)(nil)
	_ reverseproxy.UpstreamSource = (*Upstreams)(nil)
	_ caddyfile.Unmarshaler       = (*Upstreams)(nil)
)
