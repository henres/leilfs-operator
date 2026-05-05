// Package metrics defines Prometheus collectors exposed by the
// saunafs-operator controller-manager. These metrics complement the
// default controller-runtime metrics with cluster-level observability
// derived from the LeilFSCluster resources reconciled by this operator.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	// SourceManual identifies chunkservers declared in spec.chunk.servers.
	SourceManual = "manual"
	// SourceAutoDiscover identifies chunkservers materialised from PVs
	// labelled localdisk-operator.io/disk via spec.chunk.autoDiscover.
	SourceAutoDiscover = "autodiscover"
)

// Cluster-level gauges. We use one timeseries per cluster identified by
// (namespace, cluster) labels. Phase metrics use the gauge-per-state
// pattern (one gauge with phase label, value 0/1) for easy "current
// state" panels in Grafana.
var (
	ClusterInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "saunafs_cluster_info",
			Help: "Constant 1 gauge per LeilFSCluster, with version information as labels.",
		},
		[]string{"namespace", "cluster", "version"},
	)

	ClusterPhase = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "saunafs_cluster_phase",
			Help: "Current phase of the LeilFSCluster (1 for the active phase, 0 otherwise).",
		},
		[]string{"namespace", "cluster", "phase"},
	)

	MasterReplicasDesired = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "saunafs_cluster_master_replicas_desired",
			Help: "Desired number of master replicas (1 for single, 2+ for HA).",
		},
		[]string{"namespace", "cluster"},
	)

	MasterReplicasReady = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "saunafs_cluster_master_replicas_ready",
			Help: "Number of master replicas currently Ready (active + shadows).",
		},
		[]string{"namespace", "cluster"},
	)

	ShadowReplicasReady = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "saunafs_cluster_shadow_replicas_ready",
			Help: "Number of shadow master replicas currently Ready.",
		},
		[]string{"namespace", "cluster"},
	)

	ChunkServersDesired = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "saunafs_cluster_chunkservers_desired",
			Help: "Desired number of chunkserver StatefulSets, partitioned by source (manual or autodiscover).",
		},
		[]string{"namespace", "cluster", "source"},
	)

	ChunkServersReady = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "saunafs_cluster_chunkservers_ready",
			Help: "Number of chunkserver StatefulSets whose desired replicas are all Ready.",
		},
		[]string{"namespace", "cluster"},
	)

	MetaloggersReady = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "saunafs_cluster_metaloggers_ready",
			Help: "Number of metalogger replicas currently Ready.",
		},
		[]string{"namespace", "cluster"},
	)

	AutoDiscoverPVsMatched = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "saunafs_cluster_autodiscover_pvs_matched",
			Help: "Number of PersistentVolumes currently matching the autoDiscover selector for this cluster.",
		},
		[]string{"namespace", "cluster"},
	)

	ReconcileErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "saunafs_cluster_reconcile_errors_total",
			Help: "Total number of reconcile errors observed for this cluster.",
		},
		[]string{"namespace", "cluster"},
	)
)

// Phases enumerates the phase label values we report. Keep this list in
// sync with the phase strings emitted by the controller so dashboards
// can rely on a closed enum.
var Phases = []string{"Pending", "Reconciling", "Ready", "Failed"}

// MustRegister registers all collectors with the controller-runtime
// metrics registry. Call this once from main() before manager.Start().
func MustRegister() {
	ctrlmetrics.Registry.MustRegister(
		ClusterInfo,
		ClusterPhase,
		MasterReplicasDesired,
		MasterReplicasReady,
		ShadowReplicasReady,
		ChunkServersDesired,
		ChunkServersReady,
		MetaloggersReady,
		AutoDiscoverPVsMatched,
		ReconcileErrorsTotal,
	)
}

// SetPhase sets the phase gauges so that exactly one phase value is 1
// and all others are 0 for a given cluster.
func SetPhase(namespace, cluster, phase string) {
	for _, p := range Phases {
		v := 0.0
		if p == phase {
			v = 1.0
		}
		ClusterPhase.WithLabelValues(namespace, cluster, p).Set(v)
	}
}

// DeleteCluster removes all timeseries associated with a deleted
// cluster to avoid stale metrics lingering in Prometheus.
func DeleteCluster(namespace, cluster string) {
	labels := prometheus.Labels{"namespace": namespace, "cluster": cluster}
	ClusterInfo.DeletePartialMatch(labels)
	ClusterPhase.DeletePartialMatch(labels)
	MasterReplicasDesired.DeletePartialMatch(labels)
	MasterReplicasReady.DeletePartialMatch(labels)
	ShadowReplicasReady.DeletePartialMatch(labels)
	ChunkServersDesired.DeletePartialMatch(labels)
	ChunkServersReady.DeletePartialMatch(labels)
	MetaloggersReady.DeletePartialMatch(labels)
	AutoDiscoverPVsMatched.DeletePartialMatch(labels)
	ReconcileErrorsTotal.DeletePartialMatch(labels)
}
