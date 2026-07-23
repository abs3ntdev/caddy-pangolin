package caddypangolin

import (
	"errors"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var pangolinMetrics = struct {
	once         sync.Once
	refreshTotal *prometheus.CounterVec
	lastSuccess  *prometheus.GaugeVec
	mappedHosts  *prometheus.GaugeVec
	snapshotTime *prometheus.GaugeVec
	targets      *prometheus.GaugeVec
}{}

func initMetrics(registry prometheus.Registerer) error {
	const ns, sub = "caddy", "pangolin"
	pangolinMetrics.once.Do(func() {
		pangolinMetrics.refreshTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Subsystem: sub,
			Name:      "refresh_total",
			Help:      "Number of Pangolin resource refresh attempts, by outcome.",
		}, []string{"org", "config", "outcome"})
		pangolinMetrics.lastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns,
			Subsystem: sub,
			Name:      "last_refresh_success_timestamp_seconds",
			Help:      "Unix timestamp of the last successful Pangolin resource refresh.",
		}, []string{"org", "config"})
		pangolinMetrics.mappedHosts = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns,
			Subsystem: sub,
			Name:      "mapped_hosts",
			Help:      "Number of hosts in the Pangolin resource map, by kind (exact or wildcard).",
		}, []string{"org", "config", "kind"})
		pangolinMetrics.snapshotTime = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns,
			Subsystem: sub,
			Name:      "snapshot_timestamp_seconds",
			Help:      "Unix timestamp of the active Pangolin resource snapshot, by source.",
		}, []string{"org", "config", "source"})
		pangolinMetrics.targets = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns,
			Subsystem: sub,
			Name:      "targets",
			Help:      "Number of targets or remote-only hosts in the active Pangolin resource snapshot.",
		}, []string{"org", "config", "kind"})
	})
	for _, c := range []prometheus.Collector{
		pangolinMetrics.refreshTotal,
		pangolinMetrics.lastSuccess,
		pangolinMetrics.mappedHosts,
		pangolinMetrics.snapshotTime,
		pangolinMetrics.targets,
	} {
		if err := registry.Register(c); err != nil {
			var are prometheus.AlreadyRegisteredError
			if !errors.As(err, &are) {
				return err
			}
		}
	}
	return nil
}

func recordRefresh(cfg Config, snap *snapshot) {
	if pangolinMetrics.refreshTotal == nil {
		return
	}
	configID := cfg.metricID()
	if snap == nil {
		pangolinMetrics.refreshTotal.WithLabelValues(cfg.OrgID, configID, "error").Inc()
		return
	}
	pangolinMetrics.refreshTotal.WithLabelValues(cfg.OrgID, configID, "success").Inc()
	pangolinMetrics.lastSuccess.WithLabelValues(cfg.OrgID, configID).SetToCurrentTime()
	recordSnapshot(cfg, snap, "api")
}

func recordSnapshot(cfg Config, snap *snapshot, source string) {
	if pangolinMetrics.snapshotTime == nil {
		return
	}
	configID := cfg.metricID()
	pangolinMetrics.mappedHosts.WithLabelValues(cfg.OrgID, configID, "exact").Set(float64(len(snap.exact)))
	pangolinMetrics.mappedHosts.WithLabelValues(cfg.OrgID, configID, "wildcard").Set(float64(len(snap.wildcard)))
	for _, candidate := range []string{"api", "cache"} {
		if candidate != source {
			pangolinMetrics.snapshotTime.WithLabelValues(cfg.OrgID, configID, candidate).Set(0)
		}
	}
	pangolinMetrics.snapshotTime.WithLabelValues(cfg.OrgID, configID, source).Set(float64(snap.updated.Unix()))
	var localTargets, remoteHosts int
	for _, entries := range []map[string]resourceEntry{snap.exact, snap.wildcard} {
		for _, entry := range entries {
			localTargets += len(entry.Backends)
			if entry.Remote && len(entry.Backends) == 0 {
				remoteHosts++
			}
		}
	}
	pangolinMetrics.targets.WithLabelValues(cfg.OrgID, configID, "local").Set(float64(localTargets))
	pangolinMetrics.targets.WithLabelValues(cfg.OrgID, configID, "remote_hosts").Set(float64(remoteHosts))
}

func deleteMetricLabels(cfg Config) {
	if pangolinMetrics.refreshTotal == nil {
		return
	}
	configID := cfg.metricID()
	for _, outcome := range []string{"success", "error"} {
		pangolinMetrics.refreshTotal.DeleteLabelValues(cfg.OrgID, configID, outcome)
	}
	pangolinMetrics.lastSuccess.DeleteLabelValues(cfg.OrgID, configID)
	for _, kind := range []string{"exact", "wildcard"} {
		pangolinMetrics.mappedHosts.DeleteLabelValues(cfg.OrgID, configID, kind)
	}
	for _, source := range []string{"api", "cache"} {
		pangolinMetrics.snapshotTime.DeleteLabelValues(cfg.OrgID, configID, source)
	}
	for _, kind := range []string{"local", "remote_hosts"} {
		pangolinMetrics.targets.DeleteLabelValues(cfg.OrgID, configID, kind)
	}
}
