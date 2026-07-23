package caddypangolin

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/miekg/dns"
	"go.uber.org/zap"
)

func TestRefreshPreservesSnapshotOnUnsuccessfulEnvelope(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success":false,"message":"not ready"}`)
	}))
	defer srv.Close()

	original := &snapshot{
		exact:   map[string]resourceEntry{"app.example.com": {Backends: []backend{{Dial: "app:80"}}}},
		updated: time.Now(),
	}
	p := newPoller(Config{Endpoint: srv.URL, APIKey: "id.secret", OrgID: "default"}, zap.NewNop())
	p.snap = original
	p.refresh(context.Background())
	if p.current() != original {
		t.Fatal("unsuccessful response replaced the active snapshot")
	}
}

func TestRefreshPreservesSnapshotOnTargetFailure(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/org/default/resources", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success":true,"data":{"resources":[
			{"resourceId":1,"fullDomain":"app.example.com","enabled":true,"mode":"http","targets":[
				{"targetId":10,"ip":"app","port":443,"enabled":true,"siteName":"Home"}
			]}
		],"pagination":{"total":1,"page":1,"pageSize":100}}}`)
	})
	mux.HandleFunc("/v1/resource/1/targets", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	original := &snapshot{
		exact:   map[string]resourceEntry{"app.example.com": {Backends: []backend{{Dial: "app:80"}}}},
		updated: time.Now(),
	}
	p := newPoller(Config{Endpoint: srv.URL, APIKey: "id.secret", OrgID: "default"}, zap.NewNop())
	p.snap = original
	p.refresh(context.Background())
	if p.current() != original {
		t.Fatal("partial target response replaced the active snapshot")
	}
}

func TestTargetMethodRefreshWithoutTopologyChange(t *testing.T) {
	var https atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/org/default/resources", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success":true,"data":{"resources":[
			{"resourceId":1,"fullDomain":"app.example.com","enabled":true,"mode":"http","targets":[
				{"targetId":10,"ip":"app","port":443,"enabled":true,"siteName":"Home"}
			]}
		],"pagination":{"total":1,"page":1,"pageSize":100}}}`)
	})
	mux.HandleFunc("/v1/resource/1/targets", func(w http.ResponseWriter, _ *http.Request) {
		method := "http"
		if https.Load() {
			method = "https"
		}
		fmt.Fprintf(w, `{"success":true,"data":{"targets":[{"targetId":10,"method":%q}]}}`, method)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newPoller(Config{
		Endpoint:      srv.URL,
		APIKey:        "id.secret",
		OrgID:         "default",
		MethodRefresh: time.Nanosecond,
	}, zap.NewNop())
	first, err := p.fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.exact["app.example.com"].Backends[0].HTTPS {
		t.Fatal("initial backend unexpectedly uses https")
	}
	https.Store(true)
	second, err := p.fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !second.exact["app.example.com"].Backends[0].HTTPS {
		t.Fatal("method-only change was not refreshed")
	}
}

func TestMixedBackendsPreferHTTPS(t *testing.T) {
	snap := &snapshot{
		exact: map[string]resourceEntry{
			"app.example.com": {Backends: []backend{
				{Dial: "plain:80"},
				{Dial: "secure:443", HTTPS: true},
			}},
		},
		updated: time.Now(),
	}
	p := &poller{snap: snap}
	u := Upstreams{ModuleConfig: ModuleConfig{poller: p}}
	upstreams, err := u.GetUpstreams(httptest.NewRequest(http.MethodGet, "https://app.example.com", nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(upstreams) != 1 || upstreams[0].Dial != "secure:443" {
		t.Fatalf("mixed upstreams = %+v, want only secure:443", upstreams)
	}
}

func TestWildcardDoesNotMatchApex(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/org/default/resources", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success":true,"data":{"resources":[
			{"resourceId":1,"fullDomain":"*.example.com","wildcard":true,"enabled":true,"mode":"http","targets":[
				{"targetId":10,"ip":"wild","port":80,"enabled":true,"siteName":"Home"}
			]}
		],"pagination":{"total":1,"page":1,"pageSize":100}}}`)
	})
	mux.HandleFunc("/v1/resource/1/targets", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success":true,"data":{"targets":[{"targetId":10,"method":"http"}]}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newPoller(Config{Endpoint: srv.URL, APIKey: "id.secret", OrgID: "default"}, zap.NewNop())
	snap, err := p.fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snap.lookup("example.com"); ok {
		t.Fatal("wildcard matched the apex")
	}
	if _, ok := snap.lookup("sub.example.com"); !ok {
		t.Fatal("wildcard did not match a single-level subdomain")
	}
}

func TestIPv6TargetDialAddress(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/org/default/resources", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success":true,"data":{"resources":[
			{"resourceId":1,"fullDomain":"app.example.com","enabled":true,"mode":"http","targets":[
				{"targetId":10,"ip":"2001:db8::1","port":443,"enabled":true,"siteName":"Home"}
			]}
		],"pagination":{"total":1,"page":1,"pageSize":100}}}`)
	})
	mux.HandleFunc("/v1/resource/1/targets", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success":true,"data":{"targets":[{"targetId":10,"method":"https"}]}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newPoller(Config{Endpoint: srv.URL, APIKey: "id.secret", OrgID: "default"}, zap.NewNop())
	snap, err := p.fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.exact["app.example.com"].Backends[0].Dial; got != "[2001:db8::1]:443" {
		t.Fatalf("IPv6 dial address = %q", got)
	}
}

func TestLocalMatcher(t *testing.T) {
	p := &poller{snap: &snapshot{
		exact: map[string]resourceEntry{
			"local.example.com":  {Backends: []backend{{Dial: "local:80"}}},
			"remote.example.com": {Remote: true},
		},
		updated: time.Now(),
	}}
	m := LocalMatcher{ModuleConfig: ModuleConfig{poller: p}}
	if !m.Match(httptest.NewRequest(http.MethodGet, "https://local.example.com", nil)) {
		t.Fatal("local resource did not match")
	}
	if m.Match(httptest.NewRequest(http.MethodGet, "https://remote.example.com", nil)) {
		t.Fatal("remote resource matched as local")
	}
	if m.Match(httptest.NewRequest(http.MethodGet, "https://missing.example.com", nil)) {
		t.Fatal("unknown resource matched as local")
	}
}

func TestMaxStaleSnapshot(t *testing.T) {
	p := &poller{
		cfg: Config{MaxStale: time.Minute},
		snap: &snapshot{
			exact:   map[string]resourceEntry{},
			updated: time.Now().Add(-2 * time.Minute),
		},
	}
	if p.current() != nil {
		t.Fatal("expired snapshot remained active")
	}
}

func TestConcurrentCacheWritesRemainValid(t *testing.T) {
	path := t.TempDir() + "/cache.json"
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Go(func() {
			snap := &snapshot{
				exact: map[string]resourceEntry{
					fmt.Sprintf("app-%d.example.com", i): {Backends: []backend{{Dial: "app:80"}}},
				},
				wildcard: map[string]resourceEntry{},
				updated:  time.Now(),
			}
			if err := saveSnapshotToDisk(path, snap); err != nil {
				t.Errorf("saving cache: %v", err)
			}
		})
	}
	wg.Wait()
	if _, err := loadSnapshotFromDisk(path); err != nil {
		t.Fatalf("loading concurrently written cache: %v", err)
	}
}

func TestResolvedConfigValidationAndCanonicalization(t *testing.T) {
	m := ModuleConfig{
		Endpoint:  "https://pangolin.example.com/",
		APIKey:    "id.secret",
		OrgID:     "default",
		Sites:     []string{"Home", "home", " Other "},
		Resolvers: []string{"8.8.8.8", "1.1.1.1", "8.8.8.8:53"},
	}
	cfg, err := m.resolvedConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "https://pangolin.example.com" {
		t.Fatalf("endpoint = %q", cfg.Endpoint)
	}
	if !slices.Equal(cfg.Sites, []string{"home", "other"}) {
		t.Fatalf("sites = %v", cfg.Sites)
	}
	if !slices.Equal(cfg.Resolvers, []string{"1.1.1.1:53", "8.8.8.8:53"}) {
		t.Fatalf("resolvers = %v", cfg.Resolvers)
	}
	if cfg.Refresh != time.Minute || cfg.MethodRefresh != 5*time.Minute {
		t.Fatalf("unexpected defaults: refresh=%s method_refresh=%s", cfg.Refresh, cfg.MethodRefresh)
	}

	m.Endpoint = "http://pangolin.example.com"
	if _, err := m.resolvedConfig(); err == nil {
		t.Fatal("http endpoint accepted without allow_http")
	}
	m.AllowHTTP = true
	if _, err := m.resolvedConfig(); err != nil {
		t.Fatalf("http endpoint rejected with allow_http: %v", err)
	}
}

func TestUnmarshalCaddyfileOptions(t *testing.T) {
	d := caddyfile.NewTestDispenser(`pangolin {
		endpoint https://pangolin.example.com
		api_key id.secret
		org_id default
		refresh 30s
		method_refresh 2m
		max_stale 24h
		initial_timeout 5s
		sites Home Other
		resolvers 1.1.1.1 8.8.8.8
		allow_http
	}`)
	var m ModuleConfig
	if err := m.unmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if time.Duration(m.Refresh) != 30*time.Second || time.Duration(m.MethodRefresh) != 2*time.Minute || time.Duration(m.MaxStale) != 24*time.Hour || time.Duration(m.InitialTimeout) != 5*time.Second {
		t.Fatalf("unexpected durations: refresh=%s method_refresh=%s max_stale=%s initial_timeout=%s", time.Duration(m.Refresh), time.Duration(m.MethodRefresh), time.Duration(m.MaxStale), time.Duration(m.InitialTimeout))
	}
	if !m.AllowHTTP || len(m.Sites) != 2 || len(m.Resolvers) != 2 {
		t.Fatalf("unexpected parsed config: %+v", m)
	}
}

func TestWaitReady(t *testing.T) {
	p := newPoller(Config{}, zap.NewNop())
	if err := p.waitReady(time.Millisecond); err == nil {
		t.Fatal("waitReady succeeded before a snapshot was available")
	}
	p.markReady()
	if err := p.waitReady(time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestResolverFailover(t *testing.T) {
	startDNS := func(handler dns.Handler) (string, func()) {
		packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server := &dns.Server{PacketConn: packetConn, Handler: handler}
		go func() {
			_ = server.ActivateAndServe()
		}()
		return packetConn.LocalAddr().String(), func() { _ = server.Shutdown() }
	}

	failing, stopFailing := startDNS(dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		response := new(dns.Msg)
		response.SetRcode(r, dns.RcodeServerFailure)
		_ = w.WriteMsg(response)
	}))
	defer stopFailing()
	working, stopWorking := startDNS(dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		response := new(dns.Msg)
		response.SetReply(r)
		if r.Question[0].Qtype == dns.TypeA {
			response.Answer = append(response.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   net.ParseIP("127.0.0.1"),
			})
		}
		_ = w.WriteMsg(response)
	}))
	defer stopWorking()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialWithResolvers(context.Background(), "tcp", net.JoinHostPort("service.example.com", port), []string{failing, working})
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

func TestDNSRetriesTruncatedResponseOverTCP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	packetConn, err := net.ListenPacket("udp", listener.Addr().String())
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		response := new(dns.Msg)
		response.SetReply(r)
		if w.RemoteAddr().Network() == "udp" {
			response.Truncated = true
		} else if r.Question[0].Qtype == dns.TypeA {
			response.Answer = append(response.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   net.ParseIP("127.0.0.1"),
			})
		}
		_ = w.WriteMsg(response)
	})
	tcpServer := &dns.Server{Listener: listener, Handler: handler}
	udpServer := &dns.Server{PacketConn: packetConn, Handler: handler}
	go func() { _ = tcpServer.ActivateAndServe() }()
	go func() { _ = udpServer.ActivateAndServe() }()
	defer func() { _ = tcpServer.Shutdown() }()
	defer func() { _ = udpServer.Shutdown() }()

	ips, err := lookupIPs(context.Background(), listener.Addr().String(), "service.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 1 || !ips[0].Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("resolved addresses = %v", ips)
	}
}

func TestAPIRedirectRejected(t *testing.T) {
	var redirected atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Store(true)
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	p := newPoller(Config{Endpoint: source.URL, APIKey: "id.secret", OrgID: "default"}, zap.NewNop())
	var out apiEnvelope[any]
	if err := p.get(context.Background(), "/v1/org/default/resources", &out); err == nil {
		t.Fatal("API redirect was accepted")
	}
	if redirected.Load() {
		t.Fatal("API authorization request followed a redirect")
	}
}

func TestMetricIDDistinguishesPollers(t *testing.T) {
	base := Config{Endpoint: "https://pangolin.example.com", APIKey: "one", OrgID: "default"}
	other := base
	other.APIKey = "two"
	if base.metricID() == other.metricID() {
		t.Fatal("distinct pollers share a metric ID")
	}
}

func TestRequestUsesOneSnapshotAcrossMatchersAndUpstreams(t *testing.T) {
	p := &poller{
		cfg: Config{Endpoint: "https://pangolin.example.com", APIKey: "id.secret", OrgID: "default"},
		snap: &snapshot{exact: map[string]resourceEntry{
			"app.example.com": {Backends: []backend{{Dial: "plain:80"}}},
		}},
	}
	request := httptest.NewRequest(http.MethodGet, "https://app.example.com", nil)
	request = request.WithContext(context.WithValue(request.Context(), caddyhttp.VarsCtxKey, map[string]any{}))
	httpsMatcher := HTTPSBackendMatcher{ModuleConfig: ModuleConfig{poller: p}}
	if httpsMatcher.Match(request) {
		t.Fatal("HTTP snapshot matched the HTTPS route")
	}
	p.snap = &snapshot{exact: map[string]resourceEntry{
		"app.example.com": {Backends: []backend{{Dial: "secure:443", HTTPS: true}}},
	}}
	localMatcher := LocalMatcher{ModuleConfig: ModuleConfig{poller: p}}
	if !localMatcher.Match(request) {
		t.Fatal("cached local snapshot did not match")
	}
	u := Upstreams{ModuleConfig: ModuleConfig{poller: p}}
	upstreams, err := u.GetUpstreams(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(upstreams) != 1 || upstreams[0].Dial != "plain:80" {
		t.Fatalf("upstreams changed snapshots during request: %+v", upstreams)
	}
}

func TestSiteChangeInvalidatesMethodCache(t *testing.T) {
	var local atomic.Bool
	var targetCalls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/org/default/resources", func(w http.ResponseWriter, _ *http.Request) {
		site := "Remote"
		if local.Load() {
			site = "Home"
		}
		fmt.Fprintf(w, `{"success":true,"data":{"resources":[
			{"resourceId":1,"fullDomain":"app.example.com","enabled":true,"mode":"http","targets":[
				{"targetId":10,"ip":"app","port":443,"enabled":true,"siteName":%q}
			]}
		],"pagination":{"total":1,"page":1,"pageSize":100}}}`, site)
	})
	mux.HandleFunc("/v1/resource/1/targets", func(w http.ResponseWriter, _ *http.Request) {
		targetCalls.Add(1)
		fmt.Fprint(w, `{"success":true,"data":{"targets":[{"targetId":10,"method":"https"}]}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newPoller(Config{
		Endpoint: srv.URL,
		APIKey:   "id.secret",
		OrgID:    "default",
		Sites:    []string{"home"},
	}, zap.NewNop())
	first, err := p.fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !first.exact["app.example.com"].Remote || targetCalls.Load() != 0 {
		t.Fatalf("unexpected initial remote snapshot: %+v calls=%d", first.exact["app.example.com"], targetCalls.Load())
	}
	local.Store(true)
	second, err := p.fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !second.exact["app.example.com"].Backends[0].HTTPS || targetCalls.Load() != 1 {
		t.Fatalf("site change did not refresh methods: %+v calls=%d", second.exact["app.example.com"], targetCalls.Load())
	}
}

func TestCacheTimestampTracksSuccessfulValidation(t *testing.T) {
	path := t.TempDir() + "/cache.json"
	old := time.Now().Add(-24 * time.Hour).UTC()
	snap := &snapshot{
		exact:    map[string]resourceEntry{},
		wildcard: map[string]resourceEntry{},
		updated:  old,
	}
	if err := saveSnapshotToDisk(path, snap); err != nil {
		t.Fatal(err)
	}
	validated := time.Now().UTC()
	if err := touchSnapshot(path, validated); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSnapshotFromDisk(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.updated.Before(validated.Add(-time.Second)) {
		t.Fatalf("cache timestamp = %s, want approximately %s", loaded.updated, validated)
	}
}
