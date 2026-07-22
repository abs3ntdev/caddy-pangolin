package caddypangolin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

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
			{"resourceId":1,"name":"Plex","fullDomain":"plex.asdf.cafe","enabled":true,"mode":"http",
			 "targets":[{"targetId":10,"ip":"plex","port":32400,"enabled":true,"siteName":"Home","siteNiceId":"home-nice"}]},
			{"resourceId":2,"name":"Cloud","fullDomain":"cloud.asdf.cafe","enabled":true,"mode":"http",
			 "targets":[{"targetId":20,"ip":"nextcloud","port":443,"enabled":true,"siteName":"Home","siteNiceId":"home-nice"}]},
			{"resourceId":3,"name":"Far","fullDomain":"far.asdf.cafe","enabled":true,"mode":"http",
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
	if e, ok := snap.lookup("plex.asdf.cafe"); !ok || e.Backends[0].Dial != "plex:32400" {
		t.Fatalf("plex not served from cache: %+v ok=%v", e, ok)
	}
	if e, ok := snap.lookup("cloud.asdf.cafe"); !ok || !e.Backends[0].HTTPS {
		t.Fatalf("cloud https flag lost in cache: %+v ok=%v", e, ok)
	}
	if e, ok := snap.lookup("far.asdf.cafe"); !ok || !e.Remote {
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
			{"resourceId":1,"name":"Plex","fullDomain":"plex.asdf.cafe","enabled":true,"mode":"http",
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
			{"resourceId":1,"name":"App","fullDomain":"app.asdf.cafe","enabled":true,"mode":"http","targets":[
				{"targetId":10,"ip":"app1","port":80,"enabled":true,"siteName":"Home","siteNiceId":"home","hcEnabled":true,"healthStatus":"unhealthy"},
				{"targetId":11,"ip":"app2","port":80,"enabled":true,"siteName":"Home","siteNiceId":"home","hcEnabled":true,"healthStatus":"healthy"}
			]},
			{"resourceId":2,"name":"Down","fullDomain":"down.asdf.cafe","enabled":true,"mode":"http","targets":[
				{"targetId":20,"ip":"down1","port":80,"enabled":true,"siteName":"Home","siteNiceId":"home","hcEnabled":true,"healthStatus":"unhealthy"}
			]}
		],"pagination":{"total":2,"page":1,"pageSize":100}}}`)
	})
	mux.HandleFunc("/v1/resource/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success":true,"data":{"targets":[]}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newPoller(Config{Endpoint: srv.URL, APIKey: "id.secret", OrgID: "default", Refresh: time.Hour}, zap.NewNop())
	snap, err := p.fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e, ok := snap.lookup("app.asdf.cafe")
	if !ok || len(e.Backends) != 1 || e.Backends[0].Dial != "app2:80" {
		t.Fatalf("unhealthy target not filtered: %+v", e)
	}
	e, ok = snap.lookup("down.asdf.cafe")
	if !ok || len(e.Backends) != 1 {
		t.Fatalf("all-unhealthy resource should keep its backends: %+v ok=%v", e, ok)
	}
}
