# caddy-pangolin

Caddy plugin that lets a local Caddy instance mirror the resources of a remote
[Pangolin](https://github.com/fosrl/pangolin) instance, so LAN clients can reach
your services directly without hairpinning traffic through your VPS
(see [fosrl discussion #685](https://github.com/orgs/fosrl/discussions/685)).

It provides three modules:

- `http.reverse_proxy.upstreams.pangolin` — a [dynamic upstreams](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy#dynamic-upstreams)
  source that resolves the backend for each request by matching the request `Host`
  against Pangolin's resource `fullDomain`s and their targets (`ip:port`).
- `http.matchers.pangolin_https_backend` — a request matcher that matches hosts
  whose Pangolin target uses `https`, so you can route them through a TLS transport.
- `http.matchers.pangolin_remote` — a request matcher that matches hosts that
  exist in Pangolin but have no locally reachable targets, so you can route them
  back through the public Pangolin instance (see [Remote sites](#remote-sites)).

A background poller fetches `GET /v1/org/{org}/resources` (plus per-resource
targets for the http/https method) from Pangolin's Integration API and caches
the map in memory. If Pangolin becomes unreachable, the last-known map keeps
serving. Pollers are shared between modules with identical config.

## Build

```sh
xcaddy build \
  --with github.com/abs3ntdev/caddy-pangolin \
  --with github.com/caddy-dns/cloudflare
```

A prebuilt Docker image (hotio/caddy base with this plugin included) is
published from [abs3ntdev/caddy-image](https://github.com/abs3ntdev/caddy-image).

## Pangolin setup

1. Enable the Integration API (default port `3003`) and expose it to your LAN
   host (e.g. proxy it or tunnel it — it is a separate listener from the main API).
2. Create an API key with permission to list resources and targets, scoped to
   your org. The key is used as `Authorization: Bearer <id>.<secret>`.

## Caddyfile example

```caddyfile
{
	http_port 8080
	https_port 8443
}

(pangolin_cfg) {
	endpoint https://pangolin.example.com:3003
	api_key {env.PANGOLIN_API_KEY}
	org_id myorg
	refresh 60s
}

*.example.com, example.org {
	tls {
		dns cloudflare {env.CF_API_TOKEN}
	}

	# hosts whose Pangolin target method is https (e.g. unraid, nextcloud)
	@https_backend pangolin_https_backend {
		import pangolin_cfg
	}
	reverse_proxy @https_backend {
		dynamic pangolin {
			import pangolin_cfg
		}
		transport http {
			tls
			tls_insecure_skip_verify
		}
	}

	# everything else
	reverse_proxy {
		dynamic pangolin {
			import pangolin_cfg
		}
	}
}
```

Point your local DNS wildcard (`*.example.com`) at this Caddy host and LAN
traffic stays local; external traffic still flows through Pangolin on the VPS.

## Options

| Option | Required | Description |
| --- | --- | --- |
| `endpoint` | yes | Base URL of the Pangolin Integration API (`https://host:3003`) |
| `api_key` | yes | API key as `<id>.<secret>`; placeholders like `{env.X}` supported |
| `org_id` | yes | Pangolin organization ID |
| `refresh` | no | Poll interval (default `60s`) |
| `sites` | no | Site names or niceIds whose targets are locally reachable; targets on other sites are treated as remote (default: all sites local) |
| `resolvers` | no | DNS server addresses (port 53 assumed) used to resolve the Pangolin endpoint instead of the system resolver — set this when split-horizon DNS would resolve the endpoint back to this Caddy instance |
| `insecure_skip_verify` | no | Skip TLS verification when talking to the Pangolin API |

## Remote sites

If your Pangolin org has resources on sites that are not on this LAN, their
targets are not reachable by the local Caddy. Set `sites` to the site(s) local
to this Caddy instance, and use the `pangolin_remote` matcher to send
everything else back through the public Pangolin instance:

```caddyfile
(pangolin_cfg) {
	endpoint https://pangolin-api.example.com
	api_key {env.PANGOLIN_API_KEY}
	org_id default
	sites Home
}

*.example.com {
	@remote pangolin_remote {
		import pangolin_cfg
	}
	reverse_proxy @remote https://<vps-ip> {
		transport http {
			tls
			tls_server_name {http.request.host}
		}
	}

	reverse_proxy {
		dynamic pangolin {
			import pangolin_cfg
		}
	}
}
```

`pangolin_remote` matches hosts that exist in Pangolin but have no locally
reachable targets. Requests are proxied to Pangolin's edge with SNI set to the
original host so traefik routes and terminates them normally (auth rules
included, since the request goes through the real Pangolin path).

## Offline resilience

Every successful sync is persisted to disk under Caddy's data directory
(`$XDG_DATA_HOME/pangolin/<hash>.json`, i.e. `/config/pangolin/` on hotio).
On startup the last snapshot is loaded from disk *before* any network call,
so once the plugin has synced at least once:

- Caddy restarts serve immediately from cache (no 503 window)
- if the WAN or the Pangolin instance is down, local routing keeps working
  indefinitely with the last-known resource map
- the poller keeps retrying in the background and replaces the cache as soon
  as Pangolin is reachable again

The cache is keyed by endpoint + org + sites, so config changes get a fresh
cache rather than reusing a stale one.

## Metrics

When [Caddy metrics](https://caddyserver.com/docs/metrics) are enabled, the
plugin exposes:

| Metric | Type | Labels | Description |
| --- | --- | --- | --- |
| `caddy_pangolin_refresh_total` | counter | `org`, `outcome` | Refresh attempts by outcome (`success`/`error`) |
| `caddy_pangolin_last_refresh_success_timestamp_seconds` | gauge | `org` | When the resource map was last refreshed successfully |
| `caddy_pangolin_mapped_hosts` | gauge | `org`, `kind` | Hosts in the resource map (`exact`/`wildcard`) |

Alerting on `time() - caddy_pangolin_last_refresh_success_timestamp_seconds`
tells you when the map has gone stale (e.g. API unreachable).

## Behavior notes

- Only enabled resources with enabled targets are mapped; disabled ones 404
  through your normal fallback.
- Pangolin health checks are respected: targets marked unhealthy are removed
  from rotation while at least one healthy target remains. If every target of
  a resource is unhealthy, they are all kept (failing loudly beats hiding the
  resource).
- Polling is cheap in steady state: the per-resource target details (http vs
  https method) are only refetched when the resource/target topology changes,
  so an idle org costs one API request per refresh interval.
- Refreshes only log at INFO level when the resource map actually changed;
  unchanged polls log at DEBUG.
- Wildcard resources (`*.example.com`) match any single-level subdomain.
- Multiple enabled targets become multiple upstreams and use the
  `reverse_proxy` load balancing policy you configure.
- Pangolin auth rules (SSO, pins, passwords) are NOT enforced by this plugin —
  local traffic bypasses Pangolin entirely. Gate the site block yourself
  (e.g. `@block not remote_ip private_ranges` + `abort @block`).
