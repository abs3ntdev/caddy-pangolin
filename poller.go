package caddypangolin

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/miekg/dns"
	"go.uber.org/zap"
)

const (
	maxResponseBytes = 16 << 20
	maxResourcePages = 1000
	targetWorkers    = 8
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
	updated  time.Time
}

type requestLookup struct {
	entry  resourceEntry
	ok     bool
	loaded bool
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
	if h, _, err := splitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
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
	if strings.HasPrefix(hostport, "[") {
		return net.SplitHostPort(hostport)
	}
	if strings.Count(hostport, ":") != 1 {
		return hostport, "", fmt.Errorf("no port")
	}
	i := strings.LastIndexByte(hostport, ':')
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
	lastMethodsAt    time.Time
	ready            chan struct{}
	readyOnce        sync.Once

	cancel context.CancelFunc
	done   chan struct{}
}

// Config is the resolved configuration for a shared Pangolin poller.
// Modules with identical configs share a single poller instance.
type Config struct {
	// Endpoint is the base URL of the Pangolin integration API.
	Endpoint string
	// APIKey is the bearer token in the form "<id>.<secret>".
	APIKey string
	// OrgID is the Pangolin organization to list resources from.
	OrgID string
	// Refresh is the poll interval.
	Refresh        time.Duration
	MethodRefresh  time.Duration
	MaxStale       time.Duration
	InitialTimeout time.Duration
	// InsecureSkipVerify disables TLS verification for API requests.
	InsecureSkipVerify bool
	AllowHTTP          bool
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
	h := sha256.Sum256(fmt.Appendf(nil, "%s|%s|%s|%s|%s|%s|%v|%v|%s|%s", c.Endpoint, c.APIKey, c.OrgID, c.Refresh, c.MethodRefresh, c.MaxStale, c.InsecureSkipVerify, c.AllowHTTP, strings.Join(c.Sites, ","), strings.Join(c.Resolvers, ",")))
	return hex.EncodeToString(h[:])
}

func (c Config) dataKey() string {
	h := sha256.Sum256(fmt.Appendf(nil, "%s|%s|%s|%s|%s", c.Endpoint, c.APIKey, c.OrgID, strings.Join(c.Sites, ","), strings.Join(c.Resolvers, ",")))
	return hex.EncodeToString(h[:])
}

func (c Config) metricID() string {
	return c.key()[:8]
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
	_, _ = pollers.Delete(cfg.key())
}

func newPoller(cfg Config, logger *zap.Logger) *poller {
	if cfg.Refresh <= 0 {
		cfg.Refresh = 60 * time.Second
	}
	if cfg.MethodRefresh <= 0 {
		cfg.MethodRefresh = 5 * time.Minute
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.InsecureSkipVerify {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{}
		}
		transport.TLSClientConfig.InsecureSkipVerify = true
	}
	if len(cfg.Resolvers) > 0 {
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialWithResolvers(ctx, network, address, cfg.Resolvers)
		}
	}
	return &poller{
		cfg: cfg,
		client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logger: logger.Named("pangolin"),
		done:   make(chan struct{}),
		ready:  make(chan struct{}),
	}
}

func (p *poller) start() {
	if snap, err := loadSnapshotFromDisk(p.cfg.cachePath()); err == nil {
		if p.cfg.MaxStale == 0 || (!snap.updated.IsZero() && time.Since(snap.updated) <= p.cfg.MaxStale) {
			p.mu.Lock()
			p.snap = snap
			p.mu.Unlock()
			p.markReady()
			recordSnapshot(p.cfg, snap, "cache")
			p.logger.Info("loaded cached pangolin resources from disk",
				zap.String("path", p.cfg.cachePath()),
				zap.Int("hosts", len(snap.exact)),
				zap.Int("wildcards", len(snap.wildcard)))
		} else {
			p.logger.Warn("ignored stale pangolin resource cache", zap.String("path", p.cfg.cachePath()))
		}
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
	deleteMetricLabels(p.cfg)
	return nil
}

func (p *poller) current() *snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.snap != nil && p.cfg.MaxStale > 0 && (p.snap.updated.IsZero() || time.Since(p.snap.updated) > p.cfg.MaxStale) {
		return nil
	}
	return p.snap
}

func (p *poller) lookupRequest(r *http.Request) (resourceEntry, bool, bool) {
	key := "caddy_pangolin.lookup." + p.cfg.key()
	if cached, ok := caddyhttp.GetVar(r.Context(), key).(requestLookup); ok {
		return cached.entry, cached.ok, cached.loaded
	}
	snap := p.current()
	if snap == nil {
		caddyhttp.SetVar(r.Context(), key, requestLookup{})
		return resourceEntry{}, false, false
	}
	entry, ok := snap.lookup(r.Host)
	caddyhttp.SetVar(r.Context(), key, requestLookup{entry: entry, ok: ok, loaded: true})
	return entry, ok, true
}

func (p *poller) refresh(ctx context.Context) {
	snap, err := p.fetch(ctx)
	if err != nil {
		recordRefresh(p.cfg, nil)
		p.logger.Error("failed to refresh resources from pangolin", zap.Error(err))
		return
	}
	recordRefresh(p.cfg, snap)
	p.mu.Lock()
	changed := !snap.equal(p.snap)
	p.snap = snap
	p.mu.Unlock()
	p.markReady()
	logFn := p.logger.Debug
	if changed {
		logFn = p.logger.Info
	}
	logFn("refreshed pangolin resources",
		zap.Int("hosts", len(snap.exact)),
		zap.Int("wildcards", len(snap.wildcard)),
		zap.Bool("changed", changed))
	if !changed {
		if err := touchSnapshot(p.cfg.cachePath(), snap.updated); err != nil {
			if err := saveSnapshotToDisk(p.cfg.cachePath(), snap); err != nil {
				p.logger.Warn("failed to persist pangolin resource cache", zap.Error(err))
			}
		}
		return
	}
	if err := saveSnapshotToDisk(p.cfg.cachePath(), snap); err != nil {
		p.logger.Warn("failed to persist pangolin resource cache", zap.Error(err))
	}
}

func (p *poller) markReady() {
	p.readyOnce.Do(func() { close(p.ready) })
}

func (p *poller) waitReady(timeout time.Duration) error {
	if timeout <= 0 {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.ready:
		return nil
	case <-timer.C:
		return fmt.Errorf("initial pangolin resource sync did not complete within %s", timeout)
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
		if page > maxResourcePages {
			return nil, fmt.Errorf("pangolin api exceeded %d resource pages", maxResourcePages)
		}
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
		if out.Data.Pagination.Page != 0 && out.Data.Pagination.Page != page {
			return nil, fmt.Errorf("pangolin api returned page %d while requesting page %d", out.Data.Pagination.Page, page)
		}
		if out.Data.Pagination.Total > maxResourcePages*100 {
			return nil, fmt.Errorf("pangolin api reported too many resources: %d", out.Data.Pagination.Total)
		}
		all = append(all, out.Data.Resources...)
		if len(out.Data.Resources) < 100 || len(all) >= out.Data.Pagination.Total {
			break
		}
		page++
	}

	methods, err := p.targetMethods(ctx, all)
	if err != nil {
		return nil, err
	}

	type hostEntry struct {
		host     string
		wildcard bool
		entry    resourceEntry
	}
	var entries []hostEntry
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
			if t.Port < 1 || t.Port > 65535 {
				return nil, fmt.Errorf("resource %d target %d has invalid port %d", r.ResourceID, t.TargetID, t.Port)
			}
			method, ok := methods[t.TargetID]
			if !ok {
				return nil, fmt.Errorf("resource %d target %d has no method", r.ResourceID, t.TargetID)
			}
			if method != "http" && method != "https" {
				return nil, fmt.Errorf("resource %d target %d has unsupported method %q", r.ResourceID, t.TargetID, method)
			}
			b := backend{
				Dial:  net.JoinHostPort(t.IP, strconv.Itoa(t.Port)),
				HTTPS: method == "https",
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
		slices.SortFunc(entry.Backends, compareBackends)
		host := strings.ToLower(r.FullDomain)
		entries = append(entries, hostEntry{
			host:     strings.TrimPrefix(host, "*."),
			wildcard: r.Wildcard || strings.HasPrefix(host, "*."),
			entry:    entry,
		})
	}

	snap := &snapshot{
		exact:    make(map[string]resourceEntry),
		wildcard: make(map[string]resourceEntry),
		updated:  time.Now().UTC(),
	}
	for _, e := range entries {
		if e.wildcard {
			snap.wildcard[e.host] = e.entry
		}
	}
	for _, e := range entries {
		if !e.wildcard {
			snap.exact[e.host] = e.entry
		}
	}
	return snap, nil
}

func compareBackends(a, b backend) int {
	if c := strings.Compare(a.Dial, b.Dial); c != 0 {
		return c
	}
	switch {
	case a.HTTPS == b.HTTPS:
		return 0
	case b.HTTPS:
		return -1
	default:
		return 1
	}
}

func (p *poller) targetMethods(ctx context.Context, resources []apiResource) (map[int]string, error) {
	h := sha256.New()
	for _, r := range resources {
		_, _ = h.Write(fmt.Appendf(nil, "%d|", r.ResourceID))
		for _, t := range r.Targets {
			_, _ = h.Write(fmt.Appendf(nil, "%d:%s:%d:%v:%s:%s|", t.TargetID, t.IP, t.Port, t.Enabled, t.SiteName, t.SiteNiceID))
		}
	}
	hash := hex.EncodeToString(h.Sum(nil))
	if hash == p.lastResourceHash && p.lastMethods != nil && time.Since(p.lastMethodsAt) < p.cfg.MethodRefresh {
		return p.lastMethods, nil
	}
	methods, err := p.fetchTargetMethods(ctx, resources)
	if err != nil {
		return nil, err
	}
	p.lastResourceHash = hash
	p.lastMethods = methods
	p.lastMethodsAt = time.Now()
	return methods, nil
}

func (p *poller) fetchTargetMethods(ctx context.Context, resources []apiResource) (map[int]string, error) {
	methods := make(map[int]string)
	var mu sync.Mutex
	var firstErr error
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range targetWorkers {
		wg.Go(func() {
			for id := range jobs {
				var out apiEnvelope[struct {
					Targets []apiTarget `json:"targets"`
				}]
				path := fmt.Sprintf("/v1/resource/%d/targets?limit=1000", id)
				if err := p.get(ctx, path, &out); err != nil {
					p.logger.Warn("failed to fetch targets", zap.Int("resourceId", id), zap.Error(err))
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("fetching targets for resource %d: %w", id, err)
					}
					mu.Unlock()
					continue
				}
				mu.Lock()
				for _, t := range out.Data.Targets {
					methods[t.TargetID] = strings.ToLower(t.Method)
				}
				mu.Unlock()
			}
		})
	}
	for _, r := range resources {
		if !r.Enabled || !p.hasLocalTarget(r) {
			continue
		}
		jobs <- r.ResourceID
	}
	close(jobs)
	wg.Wait()
	return methods, firstErr
}

func (p *poller) hasLocalTarget(resource apiResource) bool {
	for _, target := range resource.Targets {
		if target.Enabled && target.IP != "" && target.Port != 0 && p.cfg.siteAllowed(target.SiteName, target.SiteNiceID) {
			return true
		}
	}
	return false
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pangolin api %s: status %d", path, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxResponseBytes {
		return fmt.Errorf("pangolin api %s: response exceeds %d bytes", path, maxResponseBytes)
	}
	var envelope struct {
		Success *bool  `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if envelope.Success == nil || !*envelope.Success {
		return fmt.Errorf("pangolin api %s: %s", path, envelope.Message)
	}
	return json.Unmarshal(data, out)
}

func dialWithResolvers(ctx context.Context, network, address string, resolvers []string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if net.ParseIP(host) != nil {
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, address)
	}
	var lastErr error
	for _, resolver := range resolvers {
		ips, err := lookupIPs(ctx, resolver, host)
		if err != nil {
			lastErr = err
			continue
		}
		for _, ip := range ips {
			conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
	}
	return nil, lastErr
}

func lookupIPs(ctx context.Context, resolver, host string) ([]net.IP, error) {
	client := &dns.Client{Timeout: 5 * time.Second}
	var ips []net.IP
	var lastErr error
	for _, queryType := range []uint16{dns.TypeA, dns.TypeAAAA} {
		message := new(dns.Msg)
		message.SetQuestion(dns.Fqdn(host), queryType)
		response, err := exchangeDNS(ctx, client, message, resolver)
		if err != nil {
			lastErr = err
			continue
		}
		if response.Rcode != dns.RcodeSuccess {
			lastErr = fmt.Errorf("resolver %s returned %s", resolver, dns.RcodeToString[response.Rcode])
			continue
		}
		for _, answer := range response.Answer {
			switch record := answer.(type) {
			case *dns.A:
				ips = append(ips, record.A)
			case *dns.AAAA:
				ips = append(ips, record.AAAA)
			}
		}
	}
	if len(ips) == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("resolver %s returned no addresses for %s", resolver, host)
		}
		return nil, lastErr
	}
	return ips, nil
}

func exchangeDNS(ctx context.Context, client *dns.Client, message *dns.Msg, resolver string) (*dns.Msg, error) {
	response, _, err := client.ExchangeContext(ctx, message, resolver)
	if err != nil {
		return response, err
	}
	if response == nil {
		return nil, fmt.Errorf("resolver %s returned no response", resolver)
	}
	if !response.Truncated {
		return response, nil
	}
	tcpClient := *client
	tcpClient.Net = "tcp"
	response, _, err = tcpClient.ExchangeContext(ctx, message, resolver)
	return response, err
}
