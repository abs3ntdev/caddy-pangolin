package caddypangolin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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
