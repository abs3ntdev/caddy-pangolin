package caddypangolin

import "testing"

func TestSnapshotLookup(t *testing.T) {
	snap := &snapshot{
		exact: map[string]resourceEntry{
			"plex.example.com": {Backends: []backend{{Dial: "plex:32400"}}},
			"example.org":      {Backends: []backend{{Dial: "apex:80"}}},
		},
		wildcard: map[string]resourceEntry{
			"wild.example.com": {Backends: []backend{{Dial: "wild:80"}}},
		},
	}

	cases := []struct {
		host string
		want string
		ok   bool
	}{
		{"plex.example.com", "plex:32400", true},
		{"PLEX.example.com:8443", "plex:32400", true},
		{"plex.example.com.", "plex:32400", true},
		{"example.org", "apex:80", true},
		{"foo.wild.example.com", "wild:80", true},
		{"missing.example.com", "", false},
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

func TestNormalizeResolver(t *testing.T) {
	cases := map[string]string{
		"1.1.1.1":              "1.1.1.1:53",
		"1.1.1.1:5353":         "1.1.1.1:5353",
		"2606:4700:4700::1111": "[2606:4700:4700::1111]:53",
		"dns.example.com":      "dns.example.com:53",
	}
	for input, want := range cases {
		got, err := normalizeResolver(input)
		if err != nil {
			t.Fatalf("normalizeResolver(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("normalizeResolver(%q) = %q, want %q", input, got, want)
		}
	}
	for _, input := range []string{"", " ", "1.1.1.1:0", "1.1.1.1:bad"} {
		if _, err := normalizeResolver(input); err == nil {
			t.Fatalf("normalizeResolver(%q) succeeded", input)
		}
	}
}

func TestCacheRoundTrip(t *testing.T) {
	snap := &snapshot{
		exact: map[string]resourceEntry{
			"plex.example.com":  {Backends: []backend{{Dial: "plex:32400"}}},
			"cloud.example.com": {Backends: []backend{{Dial: "nextcloud:443", HTTPS: true}}},
			"far.example.com":   {Remote: true},
		},
		wildcard: map[string]resourceEntry{
			"wild.example.com": {Backends: []backend{{Dial: "wild:80"}}},
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
	e, ok := got.lookup("cloud.example.com")
	if !ok || !e.Backends[0].HTTPS || e.Backends[0].Dial != "nextcloud:443" {
		t.Fatalf("bad https entry: %+v ok=%v", e, ok)
	}
	e, ok = got.lookup("far.example.com")
	if !ok || !e.Remote || len(e.Backends) != 0 {
		t.Fatalf("bad remote entry: %+v ok=%v", e, ok)
	}
	if e, ok = got.lookup("sub.wild.example.com"); !ok || e.Backends[0].Dial != "wild:80" {
		t.Fatalf("bad wildcard entry: %+v ok=%v", e, ok)
	}
}
