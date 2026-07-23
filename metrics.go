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
}{}

func initMetrics(registry prometheus.Registerer) error {
	const ns, sub = "caddy", "pangolin"
	pangolinMetrics.once.Do(func() {
		pangolinMetrics.refreshTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns,
			Subsystem: sub,
			Name:      "refresh_total",
			Help:      "Number of Pangolin resource refresh attempts, by outcome.",
		}, []string{"org", "outcome"})
		pangolinMetrics.lastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns,
			Subsystem: sub,
			Name:      "last_refresh_success_timestamp_seconds",
			Help:      "Unix timestamp of the last successful Pangolin resource refresh.",
		}, []string{"org"})
		pangolinMetrics.mappedHosts = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns,
			Subsystem: sub,
			Name:      "mapped_hosts",
			Help:      "Number of hosts in the Pangolin resource map, by kind (exact or wildcard).",
		}, []string{"org", "kind"})
	})
	for _, c := range []prometheus.Collector{
		pangolinMetrics.refreshTotal,
		pangolinMetrics.lastSuccess,
		pangolinMetrics.mappedHosts,
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

func recordRefresh(org string, snap *snapshot) {
	if pangolinMetrics.refreshTotal == nil {
		return
	}
	if snap == nil {
		pangolinMetrics.refreshTotal.WithLabelValues(org, "error").Inc()
		return
	}
	pangolinMetrics.refreshTotal.WithLabelValues(org, "success").Inc()
	pangolinMetrics.lastSuccess.WithLabelValues(org).SetToCurrentTime()
	pangolinMetrics.mappedHosts.WithLabelValues(org, "exact").Set(float64(len(snap.exact)))
	pangolinMetrics.mappedHosts.WithLabelValues(org, "wildcard").Set(float64(len(snap.wildcard)))
}
