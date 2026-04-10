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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LocalDiskState represents the lifecycle state of a discovered disk.
// +kubebuilder:validation:Enum=Empty;PendingFormat;Formatting;Ready;Missing;Unmanaged;FormatFailed
type LocalDiskState string

const (
	// LocalDiskStateEmpty is a disk detected without any filesystem.
	// It is waiting for spec.format to be set to true.
	LocalDiskStateEmpty LocalDiskState = "Empty"

	// LocalDiskStatePendingFormat means spec.format=true has been set but
	// formatting has not started yet.
	LocalDiskStatePendingFormat LocalDiskState = "PendingFormat"

	// LocalDiskStateFormatting means mkfs is in progress.
	LocalDiskStateFormatting LocalDiskState = "Formatting"

	// LocalDiskStateReady means the disk is formatted, mounted and a PV
	// has been created by local-static-provisioner.
	LocalDiskStateReady LocalDiskState = "Ready"

	// LocalDiskStateMissing means the disk was previously detected but can
	// no longer be found on the node.
	LocalDiskStateMissing LocalDiskState = "Missing"

	// LocalDiskStateUnmanaged means the disk already had a filesystem when
	// first detected. The agent will not touch it.
	LocalDiskStateUnmanaged LocalDiskState = "Unmanaged"

	// LocalDiskStateFormatFailed means mkfs failed. See status.message.
	LocalDiskStateFormatFailed LocalDiskState = "FormatFailed"
)

// LocalDiskSpec defines the desired state of LocalDisk.
type LocalDiskSpec struct {
	// Format triggers formatting of the disk when set to true.
	// Once set to true this field is effectively immutable: resetting it to
	// false after formatting has started will have no effect.
	// +optional
	Format bool `json:"format,omitempty"`

	// FSType is the filesystem type to use when formatting the disk.
	// Supported values: xfs, ext4.
	// Defaults to xfs if not specified.
	// +kubebuilder:validation:Enum=xfs;ext4
	// +kubebuilder:default=xfs
	// +optional
	FSType string `json:"fsType,omitempty"`
}

// LocalDiskStatus defines the observed state of LocalDisk.
type LocalDiskStatus struct {
	// Node is the Kubernetes node name where the disk resides.
	Node string `json:"node,omitempty"`

	// Device is the current block device path (e.g. /dev/sdb).
	// This may change across reboots; use UUID to track the disk reliably.
	Device string `json:"device,omitempty"`

	// Serial is the disk serial number as reported by the kernel, if available.
	Serial string `json:"serial,omitempty"`

	// SizeBytes is the total capacity of the disk in bytes.
	SizeBytes int64 `json:"sizeBytes,omitempty"`

	// SizeHumanReadable is the human-readable representation of SizeBytes.
	SizeHumanReadable string `json:"sizeHumanReadable,omitempty"`

	// FSType is the filesystem type detected on the disk (empty if unformatted).
	FSType string `json:"fsType,omitempty"`

	// UUID is the filesystem UUID assigned after formatting.
	// This is the stable identifier used as the PersistentVolume name.
	UUID string `json:"uuid,omitempty"`

	// MountPath is the directory where the disk is bind-mounted for
	// local-static-provisioner to discover.
	MountPath string `json:"mountPath,omitempty"`

	// State is the current lifecycle state of the disk.
	// +kubebuilder:default=Empty
	State LocalDiskState `json:"state,omitempty"`

	// Message contains human-readable details about the current state,
	// especially useful when State is FormatFailed.
	Message string `json:"message,omitempty"`

	// LastUpdated is the timestamp of the last status update by the agent.
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`

	// Conditions holds standard Kubernetes conditions for this disk.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster,shortName=ld
//+kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.status.node`
//+kubebuilder:printcolumn:name="Device",type=string,JSONPath=`.status.device`
//+kubebuilder:printcolumn:name="Size",type=string,JSONPath=`.status.sizeHumanReadable`
//+kubebuilder:printcolumn:name="FSType",type=string,JSONPath=`.status.fsType`
//+kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
//+kubebuilder:printcolumn:name="UUID",type=string,JSONPath=`.status.uuid`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// LocalDisk represents a physical or virtual block device discovered on a
// Kubernetes node. The disk-agent DaemonSet creates and updates these
// resources automatically. An operator sets spec.format=true to trigger
// formatting, after which the controller creates a PersistentVolume for
// the disk.
type LocalDisk struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LocalDiskSpec   `json:"spec,omitempty"`
	Status LocalDiskStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// LocalDiskList contains a list of LocalDisk
type LocalDiskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LocalDisk `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LocalDisk{}, &LocalDiskList{})
}
