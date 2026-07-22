package caddypangolin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/caddyserver/caddy/v2"
)

type cachedEntry struct {
	Backends []backend `json:"backends,omitempty"`
	Remote   bool      `json:"remote,omitempty"`
}

type cacheFile struct {
	Exact    map[string]cachedEntry `json:"exact"`
	Wildcard map[string]cachedEntry `json:"wildcard"`
}

func (c Config) cachePath() string {
	h := sha256.Sum256(fmt.Appendf(nil, "%s|%s|%s", c.Endpoint, c.OrgID, strings.Join(c.Sites, ",")))
	return filepath.Join(caddy.AppDataDir(), "pangolin", hex.EncodeToString(h[:8])+".json")
}

func loadSnapshotFromDisk(path string) (*snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cf cacheFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil, err
	}
	snap := &snapshot{
		exact:    make(map[string]resourceEntry, len(cf.Exact)),
		wildcard: make(map[string]resourceEntry, len(cf.Wildcard)),
	}
	for host, e := range cf.Exact {
		snap.exact[host] = resourceEntry{Backends: e.Backends, Remote: e.Remote}
	}
	for host, e := range cf.Wildcard {
		snap.wildcard[host] = resourceEntry{Backends: e.Backends, Remote: e.Remote}
	}
	return snap, nil
}

func saveSnapshotToDisk(path string, snap *snapshot) error {
	cf := cacheFile{
		Exact:    make(map[string]cachedEntry, len(snap.exact)),
		Wildcard: make(map[string]cachedEntry, len(snap.wildcard)),
	}
	for host, e := range snap.exact {
		cf.Exact[host] = cachedEntry{Backends: e.Backends, Remote: e.Remote}
	}
	for host, e := range snap.wildcard {
		cf.Wildcard[host] = cachedEntry{Backends: e.Backends, Remote: e.Remote}
	}
	data, err := json.Marshal(cf)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
