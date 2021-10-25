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
	cabpkv1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1alpha4"
)

// HostedControlPlaneFinalizer is the finalizer applied to HostedControlPlane resources
// by its managing controller.
const HostedControlPlaneFinalizer = "hosted.controlplane.cluster.x-k8s.io"

// HostedControlPlaneSpec defines the desired state of HostedControlPlane
type HostedControlPlaneSpec struct {
	// KubeadmConfigSpec is a KubeadmConfigSpec
	// to use for initializing and joining machines to the control plane.
	KubeadmConfigSpec cabpkv1.KubeadmConfigSpec `json:"kubeadmConfigSpec"`
}

// HostedControlPlaneStatus defines the observed state of HostedControlPlane
type HostedControlPlaneStatus struct {
	// Ready denotes that the HostedControlPlane API Server is ready to
	// receive requests.
	// +optional
	Ready bool `json:"ready"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// HostedControlPlane is the Schema for the hostedcontrolplanes API
type HostedControlPlane struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HostedControlPlaneSpec   `json:"spec,omitempty"`
	Status HostedControlPlaneStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// HostedControlPlaneList contains a list of HostedControlPlane
type HostedControlPlaneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HostedControlPlane `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HostedControlPlane{}, &HostedControlPlaneList{})
}
