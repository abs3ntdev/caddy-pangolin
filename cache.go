package caddypangolin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/caddyserver/caddy/v2"
)

type cachedEntry struct {
	Backends []backend `json:"backends,omitempty"`
	Remote   bool      `json:"remote,omitempty"`
}

type cacheFile struct {
	Exact    map[string]cachedEntry `json:"exact"`
	Wildcard map[string]cachedEntry `json:"wildcard"`
	Updated  time.Time              `json:"updated"`
}

func (c Config) cachePath() string {
	h := sha256.Sum256([]byte(c.dataKey()))
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
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if cf.Updated.IsZero() || info.ModTime().After(cf.Updated) {
		cf.Updated = info.ModTime().UTC()
	}
	snap := &snapshot{
		exact:    make(map[string]resourceEntry, len(cf.Exact)),
		wildcard: make(map[string]resourceEntry, len(cf.Wildcard)),
		updated:  cf.Updated,
	}
	for host, e := range cf.Exact {
		snap.exact[host] = resourceEntry(e)
	}
	for host, e := range cf.Wildcard {
		snap.wildcard[host] = resourceEntry(e)
	}
	return snap, nil
}

func touchSnapshot(path string, updated time.Time) error {
	return os.Chtimes(path, updated, updated)
}

func saveSnapshotToDisk(path string, snap *snapshot) error {
	cf := cacheFile{
		Exact:    make(map[string]cachedEntry, len(snap.exact)),
		Wildcard: make(map[string]cachedEntry, len(snap.wildcard)),
		Updated:  snap.updated,
	}
	for host, e := range snap.exact {
		cf.Exact[host] = cachedEntry(e)
	}
	for host, e := range snap.wildcard {
		cf.Wildcard[host] = cachedEntry(e)
	}
	data, err := json.Marshal(cf)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
