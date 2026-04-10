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

// LocalDiskConfigSpec defines the desired configuration for the localdisk-operator.
type LocalDiskConfigSpec struct {
	// Agent configures the disk-agent DaemonSet managed by the controller.
	// +optional
	Agent AgentSpec `json:"agent,omitempty"`
}

// AgentSpec defines the desired configuration for the disk-agent DaemonSet.
type AgentSpec struct {
	// NodeSelector is a label selector that restricts which nodes the agent
	// DaemonSet runs on. If empty, the agent runs on all nodes.
	// Example: {"node-role.kubernetes.io/storage": ""}
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations are applied to the agent DaemonSet pods.
	// By default the agent tolerates all taints so it can run on any node
	// (including control-plane). Set this to override that behaviour.
	// +optional
	Tolerations []Toleration `json:"tolerations,omitempty"`

	// Image is the container image for the agent.
	// Defaults to the image bundled with the operator release.
	// +optional
	Image string `json:"image,omitempty"`

	// IncludeLoopDevices makes the agent include loop devices in discovery.
	// Useful for Kind / test environments where loop devices simulate disks.
	// +optional
	IncludeLoopDevices bool `json:"includeLoopDevices,omitempty"`

	// ScanInterval overrides how often the agent rescans block devices.
	// Specified as a duration string (e.g. "10s", "1m"). Defaults to 60s.
	// +optional
	ScanInterval string `json:"scanInterval,omitempty"`
}

// Toleration mirrors corev1.Toleration but is defined here to avoid
// importing the full core/v1 package in the API types. The controller
// converts these to corev1.Toleration when building the DaemonSet.
type Toleration struct {
	// Key is the taint key that the toleration applies to.
	// +optional
	Key string `json:"key,omitempty"`
	// Operator represents a key's relationship to the value.
	// Valid values are Exists and Equal. Defaults to Equal.
	// +optional
	// +kubebuilder:validation:Enum=Exists;Equal
	Operator string `json:"operator,omitempty"`
	// Value is the taint value the toleration matches to.
	// +optional
	Value string `json:"value,omitempty"`
	// Effect indicates the taint effect to match.
	// Empty means match all taint effects.
	// +optional
	// +kubebuilder:validation:Enum="";NoSchedule;PreferNoSchedule;NoExecute
	Effect string `json:"effect,omitempty"`
}

// LocalDiskConfigStatus defines the observed state of LocalDiskConfig.
type LocalDiskConfigStatus struct {
	// AgentDaemonSetReady indicates whether the agent DaemonSet is up-to-date
	// and has the desired number of ready pods.
	AgentDaemonSetReady bool `json:"agentDaemonSetReady,omitempty"`

	// AgentDaemonSetName is the name of the managed DaemonSet.
	AgentDaemonSetName string `json:"agentDaemonSetName,omitempty"`

	// ObservedGeneration is the most recent generation observed by the controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds standard Kubernetes conditions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster,shortName=ldc
//+kubebuilder:printcolumn:name="DaemonSet",type=string,JSONPath=`.status.agentDaemonSetName`
//+kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.agentDaemonSetReady`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// LocalDiskConfig is the singleton configuration resource for the
// localdisk-operator. It controls the agent DaemonSet deployment,
// including node selection and tolerations.
// Only one LocalDiskConfig named "default" should exist in the cluster.
type LocalDiskConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LocalDiskConfigSpec   `json:"spec,omitempty"`
	Status LocalDiskConfigStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// LocalDiskConfigList contains a list of LocalDiskConfig
type LocalDiskConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LocalDiskConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LocalDiskConfig{}, &LocalDiskConfigList{})
}
