package caddypangolin

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

type backend struct {
	Dial  string
	HTTPS bool
}

type resourceEntry struct {
	Backends []backend
	// Remote is true when at least one enabled target exists on a site
	// not included in the configured sites filter (i.e. not locally
	// reachable; should be routed through Pangolin instead).
	Remote bool
}

type snapshot struct {
	exact    map[string]resourceEntry
	wildcard map[string]resourceEntry
}

func (s *snapshot) equal(other *snapshot) bool {
	if other == nil {
		return false
	}
	return maps.EqualFunc(s.exact, other.exact, entryEqual) &&
		maps.EqualFunc(s.wildcard, other.wildcard, entryEqual)
}

func entryEqual(a, b resourceEntry) bool {
	return a.Remote == b.Remote && slices.Equal(a.Backends, b.Backends)
}

func (s *snapshot) lookup(host string) (resourceEntry, bool) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if h, _, err := splitHostPort(host); err == nil {
		host = h
	}
	if e, ok := s.exact[host]; ok {
		return e, true
	}
	if i := strings.IndexByte(host, '.'); i >= 0 {
		if e, ok := s.wildcard[host[i+1:]]; ok {
			return e, true
		}
	}
	return resourceEntry{}, false
}

func splitHostPort(hostport string) (string, string, error) {
	i := strings.LastIndexByte(hostport, ':')
	if i < 0 || strings.Contains(hostport[i:], "]") {
		return hostport, "", fmt.Errorf("no port")
	}
	if strings.HasPrefix(hostport, "[") {
		return strings.Trim(hostport[:i], "[]"), hostport[i+1:], nil
	}
	return hostport[:i], hostport[i+1:], nil
}

var pollers = caddy.NewUsagePool()

type poller struct {
	cfg    Config
	client *http.Client
	logger *zap.Logger

	mu   sync.RWMutex
	snap *snapshot

	lastResourceHash string
	lastMethods      map[int]string

	cancel context.CancelFunc
	done   chan struct{}
}

type Config struct {
	Endpoint           string
	APIKey             string
	OrgID              string
	Refresh            time.Duration
	InsecureSkipVerify bool
	// Sites restricts which Pangolin sites' targets are considered locally
	// reachable. Matches site name or niceId, case-insensitive. Empty means
	// all sites are considered local.
	Sites []string
	// Resolvers are DNS server addresses used to resolve the Pangolin
	// endpoint, bypassing the system resolver (useful with split-horizon
	// DNS where the endpoint hostname would resolve back to this Caddy).
	// Port 53 is assumed if not specified. Empty means system resolver.
	Resolvers []string
}

func (c Config) siteAllowed(name, niceID string) bool {
	if len(c.Sites) == 0 {
		return true
	}
	for _, s := range c.Sites {
		if strings.EqualFold(s, name) || strings.EqualFold(s, niceID) {
			return true
		}
	}
	return false
}

func (c Config) key() string {
	return fmt.Sprintf("%s|%s|%s|%s|%v|%s|%s", c.Endpoint, c.APIKey, c.OrgID, c.Refresh, c.InsecureSkipVerify, strings.Join(c.Sites, ","), strings.Join(c.Resolvers, ","))
}

func getPoller(ctx caddy.Context, cfg Config) (*poller, error) {
	val, _, err := pollers.LoadOrNew(cfg.key(), func() (caddy.Destructor, error) {
		p := newPoller(cfg, ctx.Logger())
		p.start()
		return p, nil
	})
	if err != nil {
		return nil, err
	}
	return val.(*poller), nil
}

func releasePoller(cfg Config) {
	pollers.Delete(cfg.key())
}

func newPoller(cfg Config, logger *zap.Logger) *poller {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.InsecureSkipVerify {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{}
		}
		transport.TLSClientConfig.InsecureSkipVerify = true
	}
	if len(cfg.Resolvers) > 0 {
		transport.Proxy = nil
		resolver := &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer
				var lastErr error
				for _, addr := range cfg.Resolvers {
					conn, err := d.DialContext(ctx, network, addr)
					if err == nil {
						return conn, nil
					}
					lastErr = err
				}
				return nil, lastErr
			},
		}
		dialer := &net.Dialer{Timeout: 10 * time.Second, Resolver: resolver}
		transport.DialContext = dialer.DialContext
	}
	return &poller{
		cfg:    cfg,
		client: &http.Client{Timeout: 30 * time.Second, Transport: transport},
		logger: logger.Named("pangolin"),
		done:   make(chan struct{}),
	}
}

func (p *poller) start() {
	if snap, err := loadSnapshotFromDisk(p.cfg.cachePath()); err == nil {
		p.mu.Lock()
		p.snap = snap
		p.mu.Unlock()
		p.logger.Info("loaded cached pangolin resources from disk",
			zap.String("path", p.cfg.cachePath()),
			zap.Int("hosts", len(snap.exact)),
			zap.Int("wildcards", len(snap.wildcard)))
	} else if !os.IsNotExist(err) {
		p.logger.Warn("failed to load pangolin resource cache", zap.Error(err))
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	go func() {
		defer close(p.done)
		p.refresh(ctx)
		ticker := time.NewTicker(p.cfg.Refresh)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.refresh(ctx)
			}
		}
	}()
}

func (p *poller) Destruct() error {
	if p.cancel != nil {
		p.cancel()
		<-p.done
	}
	return nil
}

func (p *poller) current() *snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.snap
}

func (p *poller) refresh(ctx context.Context) {
	snap, err := p.fetch(ctx)
	if err != nil {
		p.logger.Error("failed to refresh resources from pangolin", zap.Error(err))
		return
	}
	p.mu.Lock()
	changed := !snap.equal(p.snap)
	p.snap = snap
	p.mu.Unlock()
	logFn := p.logger.Debug
	if changed {
		logFn = p.logger.Info
	}
	logFn("refreshed pangolin resources",
		zap.Int("hosts", len(snap.exact)),
		zap.Int("wildcards", len(snap.wildcard)),
		zap.Bool("changed", changed))
	if !changed {
		return
	}
	if err := saveSnapshotToDisk(p.cfg.cachePath(), snap); err != nil {
		p.logger.Warn("failed to persist pangolin resource cache", zap.Error(err))
	}
}

type apiEnvelope[T any] struct {
	Data    T      `json:"data"`
	Success bool   `json:"success"`
	Message string `json:"message"`
}

type apiResource struct {
	ResourceID int    `json:"resourceId"`
	Name       string `json:"name"`
	FullDomain string `json:"fullDomain"`
	Enabled    bool   `json:"enabled"`
	Wildcard   bool   `json:"wildcard"`
	Mode       string `json:"mode"`
	Targets    []struct {
		TargetID     int    `json:"targetId"`
		IP           string `json:"ip"`
		Port         int    `json:"port"`
		Enabled      bool   `json:"enabled"`
		SiteName     string `json:"siteName"`
		SiteNiceID   string `json:"siteNiceId"`
		HealthStatus string `json:"healthStatus"`
		HcEnabled    bool   `json:"hcEnabled"`
	} `json:"targets"`
}

type apiTarget struct {
	TargetID int    `json:"targetId"`
	IP       string `json:"ip"`
	Port     int    `json:"port"`
	Method   string `json:"method"`
	Enabled  bool   `json:"enabled"`
}

func (p *poller) fetch(ctx context.Context) (*snapshot, error) {
	var all []apiResource
	page := 1
	for {
		var out apiEnvelope[struct {
			Resources  []apiResource `json:"resources"`
			Pagination struct {
				Total    int `json:"total"`
				Page     int `json:"page"`
				PageSize int `json:"pageSize"`
			} `json:"pagination"`
		}]
		q := url.Values{"page": {fmt.Sprint(page)}, "pageSize": {"100"}, "enabled": {"true"}}
		path := fmt.Sprintf("/v1/org/%s/resources?%s", url.PathEscape(p.cfg.OrgID), q.Encode())
		if err := p.get(ctx, path, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Data.Resources...)
		if len(out.Data.Resources) < 100 || len(all) >= out.Data.Pagination.Total {
			break
		}
		page++
	}

	methods := p.targetMethods(ctx, all)

	snap := &snapshot{
		exact:    make(map[string]resourceEntry),
		wildcard: make(map[string]resourceEntry),
	}
	for _, r := range all {
		if !r.Enabled || r.FullDomain == "" {
			continue
		}
		if r.Mode != "" && r.Mode != "http" {
			continue
		}
		var entry resourceEntry
		var healthy []backend
		for _, t := range r.Targets {
			if !t.Enabled || t.IP == "" || t.Port == 0 {
				continue
			}
			if !p.cfg.siteAllowed(t.SiteName, t.SiteNiceID) {
				entry.Remote = true
				continue
			}
			b := backend{
				Dial:  fmt.Sprintf("%s:%d", t.IP, t.Port),
				HTTPS: methods[t.TargetID] == "https",
			}
			entry.Backends = append(entry.Backends, b)
			if !t.HcEnabled || t.HealthStatus != "unhealthy" {
				healthy = append(healthy, b)
			}
		}
		if len(healthy) > 0 && len(healthy) < len(entry.Backends) {
			entry.Backends = healthy
		}
		if len(entry.Backends) == 0 && !entry.Remote {
			continue
		}
		host := strings.ToLower(r.FullDomain)
		if r.Wildcard || strings.HasPrefix(host, "*.") {
			snap.wildcard[strings.TrimPrefix(host, "*.")] = entry
		}
		snap.exact[strings.TrimPrefix(host, "*.")] = entry
		if !strings.HasPrefix(host, "*.") {
			snap.exact[host] = entry
		}
	}
	return snap, nil
}

// targetMethods returns the http/https method per target, fetching from the
// API only when the set of targets changed since the last poll. Health-status
// changes alone do not trigger a refetch.
func (p *poller) targetMethods(ctx context.Context, resources []apiResource) map[int]string {
	h := sha256.New()
	for _, r := range resources {
		fmt.Fprintf(h, "%d|", r.ResourceID)
		for _, t := range r.Targets {
			fmt.Fprintf(h, "%d:%s:%d:%v|", t.TargetID, t.IP, t.Port, t.Enabled)
		}
	}
	hash := hex.EncodeToString(h.Sum(nil))
	if hash == p.lastResourceHash && p.lastMethods != nil {
		return p.lastMethods
	}
	methods, complete := p.fetchTargetMethods(ctx, resources)
	if complete {
		p.lastResourceHash = hash
		p.lastMethods = methods
	}
	return methods
}

func (p *poller) fetchTargetMethods(ctx context.Context, resources []apiResource) (map[int]string, bool) {
	methods := make(map[int]string)
	var failed atomic.Bool
	var mu sync.Mutex
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, r := range resources {
		if !r.Enabled || len(r.Targets) == 0 {
			continue
		}
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var out apiEnvelope[struct {
				Targets []apiTarget `json:"targets"`
			}]
			path := fmt.Sprintf("/v1/resource/%d/targets?limit=1000", id)
			if err := p.get(ctx, path, &out); err != nil {
				p.logger.Warn("failed to fetch targets", zap.Int("resourceId", id), zap.Error(err))
				failed.Store(true)
				return
			}
			mu.Lock()
			for _, t := range out.Data.Targets {
				methods[t.TargetID] = strings.ToLower(t.Method)
			}
			mu.Unlock()
		}(r.ResourceID)
	}
	wg.Wait()
	return methods, !failed.Load()
}

func (p *poller) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(p.cfg.Endpoint, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pangolin api %s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
