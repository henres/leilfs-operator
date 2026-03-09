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

// SaunaFSClusterSpec defines the desired state of SaunaFSCluster
type SaunaFSClusterSpec struct {
	// Master configures the master daemonset.
	Master MasterSpec `json:"master,omitempty"`
	// Chunk holds defaults shared by all chunk servers and the per-server list.
	Chunk ChunkSpec `json:"chunk,omitempty"`
	// Goals defines custom storage goals written to sfsgoals.cfg on the master.
	// Each entry produces one line in the file; the master reads it at startup.
	// If omitted, SaunaFS uses its built-in defaults (goals 1–9).
	// Goals with IDs 1–9 override the built-in ones; IDs 10–20 are purely
	// custom. The first entry flagged with Default=true is applied as the
	// cluster-wide default goal via the master configuration.
	Goals []GoalSpec `json:"goals,omitempty"`
	// CSI controls the optional CSI driver deployment.
	CSI CSISpec `json:"csi,omitempty"`
	// WebUI controls the optional CGI web interface (saunafs-cgiserver).
	WebUI WebUISpec `json:"interface,omitempty"`
	// Expose controls optional NodePort exposure so that external SaunaFS
	// clients (saunafs-client) can mount the filesystem from outside the cluster.
	Expose ExposeSpec `json:"expose,omitempty"`
	// NFS controls an optional NFS-Ganesha gateway that re-exports the SaunaFS
	// filesystem over standard NFS (port 2049). Any NFS client can then mount
	// the filesystem without installing saunafs-client.
	NFS NFSSpec `json:"nfs,omitempty"`
}

// Condition type constants for SaunaFSCluster.
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

// SaunaFSClusterStatus defines the observed state of SaunaFSCluster.
type SaunaFSClusterStatus struct {
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
}

// GoalSpec defines one SaunaFS storage goal written to sfsgoals.cfg.
// Exactly one of Replication or EC must be set.
//
// Built-in SaunaFS goals use IDs 1–9; custom goals should use 10–20.
//
// Examples:
//
//	{ id: 2, name: "two_copies",  replication: 2 }
//	{ id: 10, name: "ec_4_2", ec: { dataParts: 4, parityParts: 2 }, default: true }
type GoalSpec struct {
	// ID is the numeric identifier for this goal (1–20).
	// SaunaFS built-in goals occupy IDs 1–9.
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

// MasterSpec defines the master component settings.
type MasterSpec struct {
	Image        string                      `json:"image,omitempty"`
	NodeSelector map[string]string           `json:"nodeSelector,omitempty"`
	Tolerations  []corev1.Toleration         `json:"tolerations,omitempty"`
	Resources    corev1.ResourceRequirements `json:"resources,omitempty"`
	ServiceType  corev1.ServiceType          `json:"serviceType,omitempty"`
	Ports        []NamedPort                 `json:"ports,omitempty"`
}

// ChunkSpec holds defaults shared by all chunk servers and the individual server list.
type ChunkSpec struct {
	// Image is the default container image used for all chunk servers.
	Image string `json:"image,omitempty"`
	// Ports exposed by each chunk server container.
	Ports []NamedPort `json:"ports,omitempty"`
	// Servers is the list of individually configured chunk servers.
	// Each entry results in a dedicated Pod (or StatefulSet) on the target node.
	// +kubebuilder:validation:MinItems=1
	Servers []ChunkServerSpec `json:"servers"`
}

// ChunkServerSpec describes a single chunk server instance.
type ChunkServerSpec struct {
	// Name is a unique identifier for this chunk server (used as pod/sts suffix).
	Name string `json:"name"`
	// Image overrides the default chunk image for this server only.
	Image string `json:"image,omitempty"`
	// NodeName directly schedules this chunk server onto a named node.
	NodeName string `json:"nodeName"`
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
	// Image overrides the default saunafs-cgiserver:latest image.
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
	// Must include the SaunaFS FSAL (saunafs-nfs-ganesha package).
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
	// ExportPath is the SaunaFS path to export (default "/").
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

// ExposeSpec controls optional NodePort exposure for external SaunaFS clients.
type ExposeSpec struct {
	// Enabled controls whether a NodePort Service is created so that external
	// SaunaFS clients can connect to the master and mount the filesystem.
	Enabled *bool `json:"enabled,omitempty"`
	// ClientNodePort is the NodePort assigned to the SaunaFS client port (9421).
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
//+kubebuilder:printcolumn:name="ChunkServers",type="integer",JSONPath=".status.readyChunkServers",description="Ready chunk server count"
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// SaunaFSCluster is the Schema for the saunafsclusters API
type SaunaFSCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SaunaFSClusterSpec   `json:"spec,omitempty"`
	Status SaunaFSClusterStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// SaunaFSClusterList contains a list of SaunaFSCluster
type SaunaFSClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SaunaFSCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SaunaFSCluster{}, &SaunaFSClusterList{})
}
