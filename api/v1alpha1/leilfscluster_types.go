/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1 "k8s.io/api/core/v1"
)

// LeilFSClusterSpec defines the desired state of LeilFSCluster
type LeilFSClusterSpec struct {
	// Master configures the master daemonset.
	Master MasterSpec `json:"master,omitempty"`
	// Shadow configures optional shadow master instances. Shadow masters run
	// with PERSONALITY=shadow and stay fully in sync with the active master via
	// the metadata changelog stream. The operator manages a Kubernetes Lease to
	// perform automatic leader election: if the active master pod becomes
	// unavailable the operator promotes the most up-to-date shadow to master,
	// updates the master Service selector, and demotes the recovered pod to
	// shadow — preventing split-brain.
	Shadow *ShadowSpec `json:"shadow,omitempty"`
	// Chunk holds defaults shared by all chunk servers and the per-server list.
	Chunk ChunkSpec `json:"chunk,omitempty"`
	// Metaloggers configures optional metalogger instances that shadow the
	// master metadata (warm standby). Each metalogger connects to the master
	// and maintains an up-to-date copy of the metadata journal.
	Metalogger *MetaloggerSpec `json:"metalogger,omitempty"`
	// Goals defines custom storage goals written to sfsgoals.cfg on the master.
	// Each entry produces one line in the file; the master reads it at startup.
	// If omitted, LeilFS uses its built-in defaults (goals 1–9).
	// Goals with IDs 1–9 override the built-in ones; IDs 10–20 are purely
	// custom. The first entry flagged with Default=true is applied as the
	// cluster-wide default goal via the master configuration.
	Goals []GoalSpec `json:"goals,omitempty"`
	// CSI controls the optional CSI driver deployment.
	CSI CSISpec `json:"csi,omitempty"`
	// WebUI controls the optional CGI web interface (saunafs-cgiserver).
	WebUI WebUISpec `json:"interface,omitempty"`
	// Expose controls optional NodePort exposure so that external LeilFS
	// clients (saunafs-client) can mount the filesystem from outside the cluster.
	Expose ExposeSpec `json:"expose,omitempty"`
	// NFS controls an optional NFS-Ganesha gateway that re-exports the LeilFS
	// filesystem over standard NFS (port 2049). Any NFS client can then mount
	// the filesystem without installing saunafs-client.
	NFS NFSSpec `json:"nfs,omitempty"`
}

// Condition type constants for LeilFSCluster.
const (
	// ConditionReady indicates that all cluster components have been
	// successfully reconciled and are expected to be running.
	ConditionReady = "Ready"

	// ReasonReconciling is used while a reconciliation loop is in progress.
	ReasonReconciling = "Reconciling"
	// ReasonReconcileError is used when a reconciliation step returns an error.
	ReasonReconcileError = "ReconcileError"
	// ReasonReady is used when all components have been reconciled without error.
	ReasonReady = "Ready"
)

// LeilFSClusterStatus defines the observed state of LeilFSCluster.
type LeilFSClusterStatus struct {
	// Conditions holds the latest observed conditions of the cluster.
	// The "Ready" condition indicates whether all components have been
	// successfully reconciled.
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	// ReadyChunkServers is the number of chunk-server StatefulSets whose
	// desired replicas are all ready.
	ReadyChunkServers int32 `json:"readyChunkServers,omitempty"`
	// TotalChunkServers is the total number of chunk-server StatefulSets
	// configured in spec.chunk.servers.
	TotalChunkServers int32 `json:"totalChunkServers,omitempty"`
	// ReadyMetaloggers is the number of ready metalogger replicas.
	ReadyMetaloggers int32 `json:"readyMetaloggers,omitempty"`
	// ActiveMaster is the name of the pod currently holding the master role
	// (either the primary master pod or a promoted shadow pod).
	// Empty if no active master is known.
	ActiveMaster string `json:"activeMaster,omitempty"`
	// ReadyShadows is the number of shadow master replicas that are Ready and
	// connected to the active master.
	ReadyShadows int32 `json:"readyShadows,omitempty"`
}

// GoalSpec defines one LeilFS storage goal written to sfsgoals.cfg.
// Exactly one of Replication or EC must be set.
//
// Built-in LeilFS goals use IDs 1–9; custom goals should use 10–20.
//
// Examples:
//
//	{ id: 2, name: "two_copies",  replication: 2 }
//	{ id: 10, name: "ec_4_2", ec: { dataParts: 4, parityParts: 2 }, default: true }
//	{ id: 11, name: "node_spread", replication: 3, nodeLabels: ["node1", "node2", "node3"] }
type GoalSpec struct {
	// ID is the numeric identifier for this goal (1–20).
	// LeilFS built-in goals occupy IDs 1–9.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	ID int32 `json:"id"`
	// Name is the human-readable label used in sfsgoals.cfg.
	// Must contain only alphanumeric characters, hyphens, or underscores.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_-]+$`
	Name string `json:"name"`
	// Replication is the number of copies to maintain.
	// Mutually exclusive with EC.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	Replication *int32 `json:"replication,omitempty"`
	// EC defines an erasure-coding goal.
	// Mutually exclusive with Replication.
	EC *ECSpec `json:"ec,omitempty"`
	// NodeLabels constrains replication copies to specific chunkserver labels.
	// When set, each element corresponds to one copy and must match the LABEL
	// set in sfschunkserver.cfg on the target chunkserver. The pattern becomes
	// "{ label1 _ } { label2 _ } …" instead of anonymous underscores.
	// The number of NodeLabels entries must equal Replication when both are set.
	// Ignored when EC is set.
	NodeLabels []string `json:"nodeLabels,omitempty"`
	// Default marks this goal as the cluster-wide default.
	// At most one goal should set Default=true; the operator writes
	// SFSMASTER_DEFAULT_GOAL=<Name> in sfsmaster.cfg.
	Default bool `json:"default,omitempty"`
}

// ECSpec parameterises an erasure-coding goal of the form ec(dataParts, parityParts).
//
// Requirements:
//   - dataParts + parityParts chunk servers must be available.
//   - The cluster tolerates up to parityParts simultaneous failures without
//     data loss (provided a sufficient number of chunk servers exist).
//
// Common setups:
//
//	ec(4,2): 6 CS required, 50 % overhead, tolerates 2 failures.
//	ec(8,2): 10 CS required, 25 % overhead, tolerates 2 failures.
//	ec(4,1): 5 CS required, 25 % overhead, tolerates 1 failure.
type ECSpec struct {
	// DataParts is the number of data fragments (k in ec(k,m)).
	// +kubebuilder:validation:Minimum=2
	DataParts int32 `json:"dataParts"`
	// ParityParts is the number of parity/redundancy fragments (m in ec(k,m)).
	// +kubebuilder:validation:Minimum=1
	ParityParts int32 `json:"parityParts"`
}

// MasterStorageSpec configures the PersistentVolumeClaim used to persist
// the LeilFS master metadata directory (/var/lib/saunafs).
// If omitted, the operator creates a 1Gi PVC using the cluster's default
// StorageClass (typically "standard" in Kind / local-path-provisioner).
type MasterStorageSpec struct {
	// StorageClassName is the name of the StorageClass to use for the metadata PVC.
	// Defaults to "" (cluster default StorageClass).
	StorageClassName string `json:"storageClassName,omitempty"`
	// Size is the storage capacity requested for the metadata PVC.
	// Defaults to "1Gi".
	// +kubebuilder:default="1Gi"
	Size resource.Quantity `json:"size,omitempty"`
}

// MasterSpec defines the master component settings.
type MasterSpec struct {
	Image        string                      `json:"image,omitempty"`
	NodeSelector map[string]string           `json:"nodeSelector,omitempty"`
	Tolerations  []corev1.Toleration         `json:"tolerations,omitempty"`
	Resources    corev1.ResourceRequirements `json:"resources,omitempty"`
	ServiceType  corev1.ServiceType          `json:"serviceType,omitempty"`
	Ports        []NamedPort                 `json:"ports,omitempty"`
	// MetadataStorage configures the PVC used to persist /var/lib/saunafs.
	// If omitted, a default 1Gi PVC is created automatically.
	MetadataStorage *MasterStorageSpec `json:"metadataStorage,omitempty"`
	// StartupGracePeriod is how long the ha-sidecar waits after pod start
	// before it begins monitoring sfsmaster health via pgrep. This covers
	// the time needed to load metadata (proportional to filesystem size).
	// Defaults to 30s. Increase for large filesystems with many inodes.
	// +optional
	StartupGracePeriod *metav1.Duration `json:"startupGracePeriod,omitempty"`
}

// ShadowSpec configures the shadow master StatefulSet.
// Shadow masters run with PERSONALITY=shadow: they stay fully in sync with
// the active master via the changelog stream and can be promoted instantly
// without requiring sfsmetarestore. The operator uses a Kubernetes Lease for
// leader election, ensuring only one pod holds the master role at any time.
type ShadowSpec struct {
	// Replicas is the number of shadow instances (default 1).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	Replicas *int32 `json:"replicas,omitempty"`
	// Image overrides the container image (defaults to the same image as Master).
	Image string `json:"image,omitempty"`
	// MetadataStorage configures the PVC template used by each shadow replica
	// to persist its local copy of the metadata. Each replica gets its own PVC.
	MetadataStorage *MasterStorageSpec `json:"metadataStorage,omitempty"`
	// NodeSelector for shadow pods.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Tolerations for shadow pods.
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// Resources for the shadow container.
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// MetaloggerSpec configures the optional metalogger StatefulSet.// Metaloggers connect to the master and maintain a local copy of the metadata
// journal, providing warm-standby redundancy without additional licensing.
// Each replica runs an independent sfsmetal process.
type MetaloggerSpec struct {
	// Replicas is the number of metalogger instances (default 0 = disabled).
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	Replicas *int32 `json:"replicas,omitempty"`
	// Image overrides the default saunafs-metalogger container image.
	Image string `json:"image,omitempty"`
	// MetadataStorage configures the PVC template used by the metalogger
	// StatefulSet to persist its copy of the metadata journal.
	// Each replica gets its own PVC.
	MetadataStorage *MasterStorageSpec `json:"metadataStorage,omitempty"`
	// NodeSelector for metalogger pods.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Tolerations for metalogger pods.
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// Resources for the metalogger container.
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// ChunkSpec holds defaults shared by all chunk servers and the individual server list.
type ChunkSpec struct {
	// Image is the default container image used for all chunk servers.
	Image string `json:"image,omitempty"`
	// Ports exposed by each chunk server container.
	Ports []NamedPort `json:"ports,omitempty"`
	// Servers is the list of individually configured chunk servers.
	// Each entry results in a dedicated Pod (or StatefulSet) on the target node.
	// +optional
	Servers []ChunkServerSpec `json:"servers,omitempty"`
	// AutoDiscover enables automatic discovery of PersistentVolumes created by
	// the localdisk-operator. When enabled, the controller watches PVs matching
	// the given label selector and automatically creates one chunkserver
	// StatefulSet per discovered disk (PV). This is additive with Servers.
	// +optional
	AutoDiscover *ChunkAutoDiscoverSpec `json:"autoDiscover,omitempty"`
}

// ChunkAutoDiscoverSpec configures automatic chunkserver creation from PVs.
type ChunkAutoDiscoverSpec struct {
	// Enabled turns auto-discovery on or off.
	Enabled bool `json:"enabled"`
	// PVLabelSelector selects PersistentVolumes to use as chunk storage.
	// Example: {"localdisk-operator.io/node": ""} matches all localdisk PVs.
	// If empty and Enabled is true, defaults to {"localdisk-operator.io/disk": ""}.
	// +optional
	PVLabelSelector map[string]string `json:"pvLabelSelector,omitempty"`
	// Tolerations applied to all auto-discovered chunkserver pods.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// Resources overrides the default resource requirements for auto-discovered chunkservers.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// ChunkServerSpec describes a single chunk server instance.
type ChunkServerSpec struct {
	// Name is a unique identifier for this chunk server (used as pod/sts suffix).
	Name string `json:"name"`
	// Image overrides the default chunk image for this server only.
	Image string `json:"image,omitempty"`
	// NodeName directly schedules this chunk server onto a named node.
	NodeName string `json:"nodeName"`
	// Label is the LABEL value written into sfschunkserver.cfg.
	// It is used by LeilFS storage goals to pin chunks to specific servers
	// (e.g. one label per physical node ensures node-level data spreading).
	// When empty, no LABEL line is written and the chunkserver is anonymous.
	Label string `json:"label,omitempty"`
	// MountPaths lists the host paths or PVC mount points used as chunk storage.
	MountPaths []MountPath `json:"mountPaths,omitempty"`
	// Tolerations for this chunk server instance.
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// Resources overrides the default resource requirements for this server.
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// MountPath describes a single storage mount for a chunk server.
type MountPath struct {
	// Path is the mount point inside the container (e.g. /mnt/hdd001).
	Path string `json:"path"`
	// HostPath uses a directory from the host node directly (for local disks).
	HostPath string `json:"hostPath,omitempty"`
	// ClaimName references an existing PersistentVolumeClaim to mount.
	ClaimName string `json:"claimName,omitempty"`
	// StorageClassName and Size are used to dynamically provision a PVC if
	// neither HostPath nor ClaimName is set.
	StorageClassName string            `json:"storageClassName,omitempty"`
	Size             resource.Quantity `json:"size,omitempty"`
}

// CSISpec defines CSI driver deployment settings.
type CSISpec struct {
	Enabled            *bool                       `json:"enabled,omitempty"`
	DriverName         string                      `json:"driverName,omitempty"`
	Image              string                      `json:"image,omitempty"`
	ControllerReplicas *int32                      `json:"controllerReplicas,omitempty"`
	NodeSelector       map[string]string           `json:"nodeSelector,omitempty"`
	Tolerations        []corev1.Toleration         `json:"tolerations,omitempty"`
	Resources          corev1.ResourceRequirements `json:"resources,omitempty"`
}

// WebUISpec defines the optional CGI web interface (saunafs-cgiserver) settings.
type WebUISpec struct {
	// Enabled controls whether the CGI interface Deployment is created.
	Enabled *bool `json:"enabled,omitempty"`
	// Image overrides the default saunafs-cgiserver image (ghcr.io/henres/saunafs-container/saunafs-cgiserver:latest).
	Image string `json:"image,omitempty"`
	// Replicas is the number of cgiserver pods (defaults to 1).
	Replicas *int32 `json:"replicas,omitempty"`
	// Port is the HTTP port exposed by the cgiserver (defaults to 9425).
	Port int32 `json:"port,omitempty"`
	// ServiceType controls how the Service is exposed (default: ClusterIP).
	ServiceType  corev1.ServiceType          `json:"serviceType,omitempty"`
	NodeSelector map[string]string           `json:"nodeSelector,omitempty"`
	Tolerations  []corev1.Toleration         `json:"tolerations,omitempty"`
	Resources    corev1.ResourceRequirements `json:"resources,omitempty"`
}

// NFSSpec controls the optional NFS-Ganesha gateway deployment.
type NFSSpec struct {
	// Enabled controls whether the NFS-Ganesha Deployment and Service are created.
	Enabled *bool `json:"enabled,omitempty"`
	// Image is the NFS-Ganesha container image to use.
	// Must include the LeilFS FSAL (saunafs-nfs-ganesha package).
	Image string `json:"image,omitempty"`
	// Replicas is the number of NFS gateway pods (default 1).
	Replicas *int32 `json:"replicas,omitempty"`
	// NodePort is the NodePort assigned to NFS port 2049.
	// When 0 Kubernetes auto-assigns a port in the NodePort range.
	// +kubebuilder:validation:Minimum=30000
	// +kubebuilder:validation:Maximum=32767
	NodePort int32 `json:"nodePort,omitempty"`
	// ServiceType controls how the NFS Service is exposed.
	// Defaults to NodePort.
	ServiceType corev1.ServiceType `json:"serviceType,omitempty"`
	// ExportPath is the LeilFS path to export (default "/").
	ExportPath string `json:"exportPath,omitempty"`
	// Squash controls NFS root squashing (default "No_Root_Squash").
	Squash string `json:"squash,omitempty"`
	// NodeSelector for the NFS gateway pod.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Tolerations for the NFS gateway pod.
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// Resources for the NFS gateway container.
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// ExposeSpec controls optional NodePort exposure for external LeilFS clients.
type ExposeSpec struct {
	// Enabled controls whether a NodePort Service is created so that external
	// LeilFS clients can connect to the master and mount the filesystem.
	Enabled *bool `json:"enabled,omitempty"`
	// ClientNodePort is the NodePort assigned to the LeilFS client port (9421).
	// When 0 (the default) Kubernetes assigns a random port in the NodePort range.
	// +kubebuilder:validation:Minimum=30000
	// +kubebuilder:validation:Maximum=32767
	ClientNodePort int32 `json:"clientNodePort,omitempty"`
	// AdminNodePort is the optional NodePort for the admin port (9419).
	// +kubebuilder:validation:Minimum=30000
	// +kubebuilder:validation:Maximum=32767
	AdminNodePort int32 `json:"adminNodePort,omitempty"`
}

// NamedPort is a simple named container port.
type NamedPort struct {
	Name          string `json:"name,omitempty"`
	ContainerPort int32  `json:"containerPort,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="Whether all cluster components are reconciled"
//+kubebuilder:printcolumn:name="Reason",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].reason",description="Reason for the current Ready status"
//+kubebuilder:printcolumn:name="ActiveMaster",type="string",JSONPath=".status.activeMaster",description="Pod currently holding the master Lease"
//+kubebuilder:printcolumn:name="Shadows",type="integer",JSONPath=".status.readyShadows",description="Number of ready shadow replicas"
//+kubebuilder:printcolumn:name="ChunkServers",type="integer",JSONPath=".status.readyChunkServers",description="Ready chunk server count"
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// LeilFSCluster is the Schema for the saunafsclusters API
type LeilFSCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LeilFSClusterSpec   `json:"spec,omitempty"`
	Status LeilFSClusterStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// LeilFSClusterList contains a list of LeilFSCluster
type LeilFSClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LeilFSCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LeilFSCluster{}, &LeilFSClusterList{})
}
