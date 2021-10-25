/*
Copyright 2021.

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

package v1alpha4

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha4"
)

// HostedClusterFinalizer is the finalizer applied to HostedCluster resources
// by its managing controller.
const HostedClusterFinalizer = "hostedcluster.infrastructure.cluster.x-k8s.io"

// HostedClusterSpec defines the desired state of HostedCluster
type HostedClusterSpec struct {
	// ControlPlaneEndpoint represents the endpoint used to communicate with the control plane.
	// +optional
	ControlPlaneEndpoint clusterv1.APIEndpoint `json:"controlPlaneEndpoint,omitempty"`
}

// HostedClusterStatus defines the observed state of HostedCluster
type HostedClusterStatus struct {
	// Ready is true when the provider resource is ready.
	// +optional
	Ready bool `json:"ready,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// HostedCluster is the Schema for the hostedclusters API
type HostedCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HostedClusterSpec   `json:"spec,omitempty"`
	Status HostedClusterStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// HostedClusterList contains a list of HostedCluster
type HostedClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HostedCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HostedCluster{}, &HostedClusterList{})
}
