package caddypangolin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/zap"
)

func fakePangolin(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/org/default/resources", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer id.secret" {
			http.Error(w, `{"success":false}`, http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"success":true,"data":{"resources":[
			{"resourceId":1,"name":"Plex","fullDomain":"plex.example.com","enabled":true,"mode":"http",
			 "targets":[{"targetId":10,"ip":"plex","port":32400,"enabled":true,"siteName":"Home","siteNiceId":"home-nice"}]},
			{"resourceId":2,"name":"Cloud","fullDomain":"cloud.example.com","enabled":true,"mode":"http",
			 "targets":[{"targetId":20,"ip":"nextcloud","port":443,"enabled":true,"siteName":"Home","siteNiceId":"home-nice"}]},
			{"resourceId":3,"name":"Far","fullDomain":"far.example.com","enabled":true,"mode":"http",
			 "targets":[{"targetId":30,"ip":"far","port":80,"enabled":true,"siteName":"Other","siteNiceId":"other-nice"}]}
		],"pagination":{"total":3,"page":1,"pageSize":100}}}`)
	})
	mux.HandleFunc("/v1/resource/1/targets", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success":true,"data":{"targets":[{"targetId":10,"ip":"plex","port":32400,"method":"http","enabled":true}]}}`)
	})
	mux.HandleFunc("/v1/resource/2/targets", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success":true,"data":{"targets":[{"targetId":20,"ip":"nextcloud","port":443,"method":"https","enabled":true}]}}`)
	})
	mux.HandleFunc("/v1/resource/3/targets", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success":true,"data":{"targets":[{"targetId":30,"ip":"far","port":80,"method":"http","enabled":true}]}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestOfflineStartFromCache(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	srv := fakePangolin(t)

	cfg := Config{
		Endpoint: srv.URL,
		APIKey:   "id.secret",
		OrgID:    "default",
		Refresh:  time.Hour,
		Sites:    []string{"Home"},
	}

	p1 := newPoller(cfg, zap.NewNop())
	p1.start()
	deadline := time.Now().Add(5 * time.Second)
	for p1.current() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if p1.current() == nil {
		t.Fatal("initial sync never completed")
	}
	p1.Destruct()

	srv.Close()

	p2 := newPoller(cfg, zap.NewNop())
	p2.start()
	defer p2.Destruct()

	snap := p2.current()
	if snap == nil {
		t.Fatal("no snapshot loaded from cache with API offline")
	}
	if e, ok := snap.lookup("plex.example.com"); !ok || e.Backends[0].Dial != "plex:32400" {
		t.Fatalf("plex not served from cache: %+v ok=%v", e, ok)
	}
	if e, ok := snap.lookup("cloud.example.com"); !ok || !e.Backends[0].HTTPS {
		t.Fatalf("cloud https flag lost in cache: %+v ok=%v", e, ok)
	}
	if e, ok := snap.lookup("far.example.com"); !ok || !e.Remote {
		t.Fatalf("remote flag lost in cache: %+v ok=%v", e, ok)
	}
}

func TestCustomResolversDisableEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	cfg := Config{
		Endpoint:  "https://one.one.one.one",
		APIKey:    "id.secret",
		OrgID:     "default",
		Refresh:   time.Hour,
		Resolvers: []string{"1.1.1.1:53"},
	}
	p := newPoller(cfg, zap.NewNop())
	transport := p.client.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("environment proxy remains enabled with custom resolvers")
	}
}

func TestTargetFetchSkippedWhenUnchanged(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	var targetCalls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/org/default/resources", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success":true,"data":{"resources":[
			{"resourceId":1,"name":"Plex","fullDomain":"plex.example.com","enabled":true,"mode":"http",
			 "targets":[{"targetId":10,"ip":"plex","port":32400,"enabled":true,"siteName":"Home","siteNiceId":"home"}]}
		],"pagination":{"total":1,"page":1,"pageSize":100}}}`)
	})
	mux.HandleFunc("/v1/resource/1/targets", func(w http.ResponseWriter, _ *http.Request) {
		targetCalls.Add(1)
		fmt.Fprint(w, `{"success":true,"data":{"targets":[{"targetId":10,"ip":"plex","port":32400,"method":"http","enabled":true}]}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newPoller(Config{Endpoint: srv.URL, APIKey: "id.secret", OrgID: "default", Refresh: time.Hour}, zap.NewNop())
	ctx := context.Background()
	p.refresh(ctx)
	p.refresh(ctx)
	p.refresh(ctx)
	if got := targetCalls.Load(); got != 1 {
		t.Fatalf("target endpoint called %d times, want 1", got)
	}
}

func TestUnhealthyTargetsFiltered(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/org/default/resources", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success":true,"data":{"resources":[
			{"resourceId":1,"name":"App","fullDomain":"app.example.com","enabled":true,"mode":"http","targets":[
				{"targetId":10,"ip":"app1","port":80,"enabled":true,"siteName":"Home","siteNiceId":"home","hcEnabled":true,"healthStatus":"unhealthy"},
				{"targetId":11,"ip":"app2","port":80,"enabled":true,"siteName":"Home","siteNiceId":"home","hcEnabled":true,"healthStatus":"healthy"}
			]},
			{"resourceId":2,"name":"Down","fullDomain":"down.example.com","enabled":true,"mode":"http","targets":[
				{"targetId":20,"ip":"down1","port":80,"enabled":true,"siteName":"Home","siteNiceId":"home","hcEnabled":true,"healthStatus":"unhealthy"}
			]}
		],"pagination":{"total":2,"page":1,"pageSize":100}}}`)
	})
	mux.HandleFunc("/v1/resource/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success":true,"data":{"targets":[
			{"targetId":10,"method":"http"},{"targetId":11,"method":"http"},
			{"targetId":20,"method":"http"}
		]}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newPoller(Config{Endpoint: srv.URL, APIKey: "id.secret", OrgID: "default", Refresh: time.Hour}, zap.NewNop())
	snap, err := p.fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e, ok := snap.lookup("app.example.com")
	if !ok || len(e.Backends) != 1 || e.Backends[0].Dial != "app2:80" {
		t.Fatalf("unhealthy target not filtered: %+v", e)
	}
	e, ok = snap.lookup("down.example.com")
	if !ok || len(e.Backends) != 1 {
		t.Fatalf("all-unhealthy resource should keep its backends: %+v ok=%v", e, ok)
	}
}

func wildcardOrderServer(t *testing.T, wildcardFirst bool) *httptest.Server {
	wildcard := `{"resourceId":1,"name":"Wild","fullDomain":"*.example.com","enabled":true,"mode":"http","wildcard":true,
	 "targets":[{"targetId":10,"ip":"wild","port":80,"enabled":true,"siteName":"Home","siteNiceId":"home"}]}`
	exact := `{"resourceId":2,"name":"Apex","fullDomain":"example.com","enabled":true,"mode":"http",
	 "targets":[{"targetId":20,"ip":"apex","port":80,"enabled":true,"siteName":"Home","siteNiceId":"home"}]}`
	resources := exact + "," + wildcard
	if wildcardFirst {
		resources = wildcard + "," + exact
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/org/default/resources", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"success":true,"data":{"resources":[%s],"pagination":{"total":2,"page":1,"pageSize":100}}}`, resources)
	})
	mux.HandleFunc("/v1/resource/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success":true,"data":{"targets":[
			{"targetId":10,"method":"http"},{"targetId":20,"method":"http"}
		]}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestExplicitResourceBeatsWildcard(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	for _, wildcardFirst := range []bool{false, true} {
		srv := wildcardOrderServer(t, wildcardFirst)
		p := newPoller(Config{Endpoint: srv.URL, APIKey: "id.secret", OrgID: "default", Refresh: time.Hour}, zap.NewNop())
		snap, err := p.fetch(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if e, ok := snap.lookup("example.com"); !ok || e.Backends[0].Dial != "apex:80" {
			t.Fatalf("wildcardFirst=%v: apex resolved to %+v, want apex:80", wildcardFirst, e)
		}
		if e, ok := snap.lookup("sub.example.com"); !ok || e.Backends[0].Dial != "wild:80" {
			t.Fatalf("wildcardFirst=%v: subdomain resolved to %+v, want wild:80", wildcardFirst, e)
		}
	}
}

func TestBackendOrderStable(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	var calls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/org/default/resources", func(w http.ResponseWriter, _ *http.Request) {
		targets := `{"targetId":10,"ip":"a","port":80,"enabled":true,"siteName":"Home","siteNiceId":"home"},
			{"targetId":11,"ip":"b","port":80,"enabled":true,"siteName":"Home","siteNiceId":"home"}`
		if calls.Add(1)%2 == 0 {
			targets = `{"targetId":11,"ip":"b","port":80,"enabled":true,"siteName":"Home","siteNiceId":"home"},
				{"targetId":10,"ip":"a","port":80,"enabled":true,"siteName":"Home","siteNiceId":"home"}`
		}
		fmt.Fprintf(w, `{"success":true,"data":{"resources":[
			{"resourceId":1,"name":"App","fullDomain":"app.example.com","enabled":true,"mode":"http","targets":[%s]}
		],"pagination":{"total":1,"page":1,"pageSize":100}}}`, targets)
	})
	mux.HandleFunc("/v1/resource/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success":true,"data":{"targets":[
			{"targetId":10,"method":"http"},{"targetId":11,"method":"http"}
		]}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newPoller(Config{Endpoint: srv.URL, APIKey: "id.secret", OrgID: "default", Refresh: time.Hour}, zap.NewNop())
	first, err := p.fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !first.equal(second) {
		t.Fatalf("snapshots differ on API target reordering: %+v vs %+v",
			first.exact["app.example.com"], second.exact["app.example.com"])
	}
}

func TestMetricsRecorded(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	registry := prometheus.NewPedanticRegistry()
	if err := initMetrics(registry); err != nil {
		t.Fatal(err)
	}
	if err := initMetrics(registry); err != nil {
		t.Fatalf("re-registration should be tolerated: %v", err)
	}

	srv := fakePangolin(t)
	p := newPoller(Config{Endpoint: srv.URL, APIKey: "id.secret", OrgID: "default", Refresh: time.Hour}, zap.NewNop())
	p.refresh(context.Background())
	srv.Close()
	p.refresh(context.Background())

	configID := p.cfg.metricID()
	success := testutil.ToFloat64(pangolinMetrics.refreshTotal.WithLabelValues("default", configID, "success"))
	errors := testutil.ToFloat64(pangolinMetrics.refreshTotal.WithLabelValues("default", configID, "error"))
	if success < 1 {
		t.Fatalf("success counter = %v, want >= 1", success)
	}
	if errors < 1 {
		t.Fatalf("error counter = %v, want >= 1", errors)
	}
	hosts := testutil.ToFloat64(pangolinMetrics.mappedHosts.WithLabelValues("default", configID, "exact"))
	if hosts != 3 {
		t.Fatalf("mapped_hosts exact = %v, want 3", hosts)
	}
}
