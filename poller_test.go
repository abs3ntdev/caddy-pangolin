package caddypangolin

import "testing"

func TestSnapshotLookup(t *testing.T) {
	snap := &snapshot{
		exact: map[string]resourceEntry{
			"plex.asdf.cafe": {Backends: []backend{{Dial: "plex:32400"}}},
			"abs3nt.dev":     {Backends: []backend{{Dial: "abs3nt:80"}}},
		},
		wildcard: map[string]resourceEntry{
			"wild.asdf.cafe": {Backends: []backend{{Dial: "wild:80"}}},
		},
	}

	cases := []struct {
		host string
		want string
		ok   bool
	}{
		{"plex.asdf.cafe", "plex:32400", true},
		{"PLEX.asdf.cafe:8443", "plex:32400", true},
		{"plex.asdf.cafe.", "plex:32400", true},
		{"abs3nt.dev", "abs3nt:80", true},
		{"foo.wild.asdf.cafe", "wild:80", true},
		{"missing.asdf.cafe", "", false},
	}
	for _, c := range cases {
		e, ok := snap.lookup(c.host)
		if ok != c.ok {
			t.Fatalf("lookup(%q) ok=%v want %v", c.host, ok, c.ok)
		}
		if ok && e.Backends[0].Dial != c.want {
			t.Fatalf("lookup(%q) = %q want %q", c.host, e.Backends[0].Dial, c.want)
		}
	}
}

func TestSiteAllowed(t *testing.T) {
	c := Config{Sites: []string{"Home"}}
	if !c.siteAllowed("Home", "parallel-giant-pangolin") {
		t.Fatal("name match failed")
	}
	if !c.siteAllowed("Other", "home") {
		t.Fatal("niceId match should be case-insensitive")
	}
	if c.siteAllowed("Remote", "other-site") {
		t.Fatal("should not match")
	}
	if !(Config{}).siteAllowed("Any", "any") {
		t.Fatal("empty filter should allow all")
	}
}

func TestCacheRoundTrip(t *testing.T) {
	snap := &snapshot{
		exact: map[string]resourceEntry{
			"plex.asdf.cafe":  {Backends: []backend{{Dial: "plex:32400"}}},
			"cloud.asdf.cafe": {Backends: []backend{{Dial: "nextcloud:443", HTTPS: true}}},
			"far.asdf.cafe":   {Remote: true},
		},
		wildcard: map[string]resourceEntry{
			"wild.asdf.cafe": {Backends: []backend{{Dial: "wild:80"}}},
		},
	}
	path := t.TempDir() + "/cache.json"
	if err := saveSnapshotToDisk(path, snap); err != nil {
		t.Fatal(err)
	}
	got, err := loadSnapshotFromDisk(path)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := got.lookup("cloud.asdf.cafe")
	if !ok || !e.Backends[0].HTTPS || e.Backends[0].Dial != "nextcloud:443" {
		t.Fatalf("bad https entry: %+v ok=%v", e, ok)
	}
	e, ok = got.lookup("far.asdf.cafe")
	if !ok || !e.Remote || len(e.Backends) != 0 {
		t.Fatalf("bad remote entry: %+v ok=%v", e, ok)
	}
	if e, ok = got.lookup("sub.wild.asdf.cafe"); !ok || e.Backends[0].Dial != "wild:80" {
		t.Fatalf("bad wildcard entry: %+v ok=%v", e, ok)
	}
}
