package daemon

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const meterName = "github.com/jonbaldie/gastown/daemon"

// daemonMetrics holds OTel instruments for the daemon.
// All methods are nil-safe so callers don't need to guard against disabled telemetry.
type daemonMetrics struct {
	*daemonMetricState
}

type daemonMetricState struct {
	// heartbeatTotal counts daemon heartbeat cycles.
	heartbeatTotal metric.Int64Counter

	// restartTotal counts agent session restarts, labeled by agent type.
	restartTotal metric.Int64Counter

	// polecatSpawns counts polecat session spawns, labeled by rig name.
	polecatSpawns metric.Int64Counter

	// doltMu protects dolt gauge values written by the health check goroutine.
	doltMu             sync.RWMutex
	doltConnections    int64
	doltMaxConnections int64
	doltLatencyMs      float64
	doltDiskBytes      int64
	doltHealthy        int64 // 1 = healthy, 0 = unhealthy
}

// newDaemonMetrics registers all daemon OTel instruments against the global
// MeterProvider. Must be called after telemetry.Init so the provider is set.
// Returns a no-op struct if no provider is configured.
func newDaemonMetrics() (*daemonMetrics, error) {
	m := otel.GetMeterProvider().Meter(meterName)
	state := &daemonMetricState{}
	dm := &daemonMetrics{daemonMetricState: state}

	counters, err := registerDaemonCounters(m)
	if err != nil {
		return nil, err
	}
	dm.heartbeatTotal = counters.heartbeat
	dm.restartTotal = counters.restart
	dm.polecatSpawns = counters.polecatSpawns

	gauges, err := registerDoltGauges(m)
	if err != nil {
		return nil, err
	}

	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		state.doltMu.RLock()
		defer state.doltMu.RUnlock()
		o.ObserveInt64(gauges.connections, state.doltConnections)
		o.ObserveInt64(gauges.maxConnections, state.doltMaxConnections)
		o.ObserveFloat64(gauges.latency, state.doltLatencyMs)
		o.ObserveInt64(gauges.disk, state.doltDiskBytes)
		o.ObserveInt64(gauges.healthy, state.doltHealthy)
		return nil
	}, gauges.connections, gauges.maxConnections, gauges.latency, gauges.disk, gauges.healthy)
	if err != nil {
		return nil, err
	}

	return dm, nil
}

type daemonCounters struct {
	heartbeat     metric.Int64Counter
	restart       metric.Int64Counter
	polecatSpawns metric.Int64Counter
}

func registerDaemonCounters(m metric.Meter) (daemonCounters, error) {
	heartbeat, err := m.Int64Counter("gastown.daemon.heartbeat.total",
		metric.WithDescription("Total number of daemon heartbeat cycles"),
	)
	if err != nil {
		return daemonCounters{}, err
	}

	restart, err := m.Int64Counter("gastown.daemon.restart.total",
		metric.WithDescription("Total number of agent session restarts"),
	)
	if err != nil {
		return daemonCounters{}, err
	}

	polecatSpawns, err := m.Int64Counter("gastown.polecat.spawns.total",
		metric.WithDescription("Total number of polecat session spawns"),
	)
	if err != nil {
		return daemonCounters{}, err
	}
	return daemonCounters{heartbeat: heartbeat, restart: restart, polecatSpawns: polecatSpawns}, nil
}

type doltGauges struct {
	connections    metric.Int64ObservableGauge
	maxConnections metric.Int64ObservableGauge
	latency        metric.Float64ObservableGauge
	disk           metric.Int64ObservableGauge
	healthy        metric.Int64ObservableGauge
}

func registerDoltGauges(m metric.Meter) (doltGauges, error) {
	connections, err := m.Int64ObservableGauge("gastown.dolt.connections",
		metric.WithDescription("Active Dolt server connections"),
	)
	if err != nil {
		return doltGauges{}, err
	}

	maxConnections, err := m.Int64ObservableGauge("gastown.dolt.max_connections",
		metric.WithDescription("Configured maximum Dolt server connections"),
	)
	if err != nil {
		return doltGauges{}, err
	}

	latency, err := m.Float64ObservableGauge("gastown.dolt.query_latency_ms",
		metric.WithDescription("Dolt health probe round-trip latency in milliseconds"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return doltGauges{}, err
	}

	disk, err := m.Int64ObservableGauge("gastown.dolt.disk_usage_bytes",
		metric.WithDescription("Dolt data directory disk usage"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return doltGauges{}, err
	}

	healthy, err := m.Int64ObservableGauge("gastown.dolt.healthy",
		metric.WithDescription("Dolt server health (1=healthy, 0=unhealthy)"),
	)
	if err != nil {
		return doltGauges{}, err
	}
	return doltGauges{
		connections:    connections,
		maxConnections: maxConnections,
		latency:        latency,
		disk:           disk,
		healthy:        healthy,
	}, nil
}

// RecordHeartbeat increments the heartbeat counter.
func (dm *daemonMetrics) RecordHeartbeat(ctx context.Context) {
	if dm == nil || dm.daemonMetricState == nil {
		return
	}
	dm.heartbeatTotal.Add(ctx, 1)
}

// RecordRestart increments the restart counter, labeled with the agent type
// (e.g. "deacon", "witness", "refinery", "polecat").
func (dm *daemonMetrics) RecordRestart(ctx context.Context, agentType string) {
	if dm == nil || dm.daemonMetricState == nil {
		return
	}
	dm.restartTotal.Add(ctx, 1,
		metric.WithAttributes(attribute.String("agent.type", agentType)),
	)
}

// RecordPolecatSpawn increments the polecat spawn counter, labeled with the rig name.
func (dm *daemonMetrics) RecordPolecatSpawn(ctx context.Context, rigName string) {
	if dm == nil || dm.daemonMetricState == nil {
		return
	}
	dm.polecatSpawns.Add(ctx, 1,
		metric.WithAttributes(attribute.String("rig", rigName)),
	)
}

// UpdateDoltHealth stores the latest Dolt health snapshot for observable gauges.
func (dm *daemonMetrics) UpdateDoltHealth(conns, maxConns int64, latencyMs float64, diskBytes int64, healthy bool) {
	if dm == nil || dm.daemonMetricState == nil {
		return
	}
	var healthyInt int64
	if healthy {
		healthyInt = 1
	}
	dm.doltMu.Lock()
	defer dm.doltMu.Unlock()
	dm.doltConnections = conns
	dm.doltMaxConnections = maxConns
	dm.doltLatencyMs = latencyMs
	dm.doltDiskBytes = diskBytes
	dm.doltHealthy = healthyInt
}
