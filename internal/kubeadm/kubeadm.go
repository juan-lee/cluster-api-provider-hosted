/*
Copyright 2020 Juan-Lee Pang.

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
package kubeadm

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/blang/semver"
	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	bootstrapapi "k8s.io/cluster-bootstrap/token/api"
	"k8s.io/klog"
	kubeadmapi "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm"
	"k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm/scheme"
	kubeadmv1 "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm/v1beta3"
	"k8s.io/kubernetes/cmd/kubeadm/app/componentconfigs"
	addondns "k8s.io/kubernetes/cmd/kubeadm/app/phases/addons/dns"
	addonproxy "k8s.io/kubernetes/cmd/kubeadm/app/phases/addons/proxy"
	"k8s.io/kubernetes/cmd/kubeadm/app/phases/bootstraptoken/clusterinfo"
	bootstraptokennode "k8s.io/kubernetes/cmd/kubeadm/app/phases/bootstraptoken/node"
	"k8s.io/kubernetes/cmd/kubeadm/app/phases/controlplane"
	"k8s.io/kubernetes/cmd/kubeadm/app/phases/etcd"
	"k8s.io/kubernetes/cmd/kubeadm/app/phases/kubelet"
	"k8s.io/kubernetes/cmd/kubeadm/app/phases/uploadconfig"
	"k8s.io/kubernetes/cmd/kubeadm/app/util/apiclient"
	etcdutil "k8s.io/kubernetes/cmd/kubeadm/app/util/etcd"
	cabpkv1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1alpha4"
	kubeadmtypes "sigs.k8s.io/cluster-api/bootstrap/kubeadm/types"
)

type Configuration struct {
	ClusterName          string
	InitConfiguration    kubeadmv1.InitConfiguration
	ClusterConfiguration kubeadmv1.ClusterConfiguration
}

func Defaults() *Configuration {
	config := Configuration{}
	scheme.Scheme.Default(&config.InitConfiguration)
	scheme.Scheme.Default(&config.ClusterConfiguration)
	return &config
}

func DefaultCluster() *kubeadmv1.ClusterConfiguration {
	cc := kubeadmv1.ClusterConfiguration{}
	scheme.Scheme.Default(&cc)
	return &cc
}

func DefaultInit() *kubeadmv1.InitConfiguration {
	ic := kubeadmv1.InitConfiguration{}
	scheme.Scheme.Default(&ic)
	return &ic
}

func New(clusterName string, init *cabpkv1.InitConfiguration, clusterConfig *cabpkv1.ClusterConfiguration) (*Configuration, error) {
	config := Configuration{}
	config.ClusterName = clusterName
	if init != nil {
		initJSON, err := json.Marshal(*init)
		if err != nil {
			return nil, err
		}
		err = json.Unmarshal(initJSON, &config.InitConfiguration)
		if err != nil {
			return nil, err
		}
	}
	if clusterConfig != nil {
		clusterConfigJSON, err := json.Marshal(*clusterConfig)
		if err != nil {
			return nil, err
		}
		err = json.Unmarshal(clusterConfigJSON, &config.ClusterConfiguration)
		if err != nil {
			return nil, err
		}
	}
	return &config, nil
}

func (c *Configuration) GenerateSecret(init *cabpkv1.InitConfiguration, clusterConfig *cabpkv1.ClusterConfiguration) (*corev1.Secret, error) {
	parsedVersion, err := semver.ParseTolerant(clusterConfig.KubernetesVersion)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse kubernetes version %q", clusterConfig.KubernetesVersion)
	}
	initdata, err := kubeadmtypes.MarshalInitConfigurationForVersion(init, parsedVersion)
	if err != nil {
		return nil, err
	}
	clusterdata, err := kubeadmtypes.MarshalClusterConfigurationForVersion(clusterConfig, parsedVersion)
	if err != nil {
		return nil, err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-config", c.ClusterName),
		},
		Data: map[string][]byte{
			"config.yaml": []byte(fmt.Sprintf("%s\n---\n%s", initdata, clusterdata)),
		},
	}
	return secret, nil
}

func (c *Configuration) ControlPlaneDeploymentSpec() *appsv1.Deployment {
	initConfig := kubeadmapi.InitConfiguration{}
	scheme.Scheme.Default(&c.InitConfiguration)
	scheme.Scheme.Default(&c.ClusterConfiguration)
	scheme.Scheme.Convert(&c.InitConfiguration, &initConfig, nil)
	scheme.Scheme.Convert(&c.ClusterConfiguration, &initConfig.ClusterConfiguration, nil)

	initConfig.LocalAPIEndpoint.AdvertiseAddress = "$(POD_IP)"
	pods := controlplane.GetStaticPodSpecs(&initConfig.ClusterConfiguration, &initConfig.LocalAPIEndpoint)

	combined := corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{
					Name:            "kubeadm-init",
					Image:           "juanlee/kubeadm:latest",
					ImagePullPolicy: "Always",
					Command:         []string{"sh", "-c", "mkdir -p /etc/kubernetes/pki && cp -R /tmp/kubernetes/pki/* /etc/kubernetes/pki && sed \"s/POD_IP/$(POD_IP)/g\" /var/run/kubeadm/config.sed.yaml > /var/run/kubeadm/config.yaml && kubeadm-init"},
					Env: []corev1.EnvVar{
						{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "k8s-ca", MountPath: "/tmp/kubernetes/pki/ca.key", SubPath: "tls.key"},
						{Name: "k8s-ca", MountPath: "/tmp/kubernetes/pki/ca.crt", SubPath: "tls.crt"},
						{Name: "k8s-proxy", MountPath: "/tmp/kubernetes/pki/front-proxy-ca.key", SubPath: "tls.key"},
						{Name: "k8s-proxy", MountPath: "/tmp/kubernetes/pki/front-proxy-ca.crt", SubPath: "tls.crt"},
						{Name: "k8s-sa", MountPath: "/tmp/kubernetes/pki/sa.key", SubPath: "tls.key"},
						{Name: "k8s-sa", MountPath: "/tmp/kubernetes/pki/sa.pub", SubPath: "tls.crt"},
						{Name: "etcd-certs", MountPath: "/tmp/kubernetes/pki/etcd/ca.key", SubPath: "tls.key"},
						{Name: "etcd-certs", MountPath: "/tmp/kubernetes/pki/etcd/ca.crt", SubPath: "tls.crt"},
						{Name: "etc-kubernetes", MountPath: "/etc/kubernetes"},
						{Name: "var-run-kubeadm", MountPath: "/var/run/kubeadm/config.sed.yaml", SubPath: "config.yaml"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{Name: "k8s-ca", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: fmt.Sprintf("%s-ca", c.ClusterName)}}},
				{Name: "k8s-proxy", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: fmt.Sprintf("%s-proxy", c.ClusterName)}}},
				{Name: "k8s-sa", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: fmt.Sprintf("%s-sa", c.ClusterName)}}},
				{Name: "etcd-certs", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: fmt.Sprintf("%s-etcd", c.ClusterName)}}},
				{Name: "var-run-kubeadm", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: fmt.Sprintf("%s-config", c.ClusterName)}}},
				{Name: "etcd-data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "etc-kubernetes", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "flexvolume-dir", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "ca-certs", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/etc/ssl/certs", Type: hostPathTypePtr(corev1.HostPathDirectoryOrCreate)}}},
			},
		},
	}

	for _, pod := range pods {
		switch pod.Name {
		case "kube-apiserver":
			mutateAPIServer(pod.Spec.Containers)
		default:
			mutate(pod.Spec.Containers)
		}
		combined.Spec.Containers = append(combined.Spec.Containers, pod.Spec.Containers...)
	}

	etcdPod := etcd.GetEtcdPodSpec(&initConfig.ClusterConfiguration, &initConfig.LocalAPIEndpoint, "controlplane", []etcdutil.Member{})
	mutateEtcd(etcdPod.Spec.Containers)
	combined.Spec.Containers = append(combined.Spec.Containers, etcdPod.Spec.Containers...)

	sort.Slice(combined.Spec.Containers, func(i, j int) bool {
		return combined.Spec.Containers[i].Name < combined.Spec.Containers[j].Name
	})

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-hcp", c.ClusterName),
			Labels: map[string]string{
				"app": "controlplane",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "controlplane",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name: "controlplane",
					Labels: map[string]string{
						"app": "controlplane",
					},
				},
				Spec: combined.Spec,
			},
		},
	}
}

func mutateAPIServer(containers []corev1.Container) {
	replaceVolumeMounts(containers)
	injectEnvVars(containers)
	fixupProbes(containers)
}

func mutateEtcd(containers []corev1.Container) {
	replaceVolumeMounts(containers)
	injectEnvVars(containers)
	fixupProbes(containers)
	fixupListenMetricUrls(containers)
}

func mutate(containers []corev1.Container) {
	replaceVolumeMounts(containers)
	fixupProbes(containers)
	fixupBindAddress(containers)
}

func replaceVolumeMounts(containers []corev1.Container) {
	for n := range containers {
		containers[n].VolumeMounts = []corev1.VolumeMount{
			{Name: "etc-kubernetes", MountPath: "/etc/kubernetes", ReadOnly: true},
			{Name: "ca-certs", MountPath: "/etc/ssl/certs", ReadOnly: true},
		}
	}
}

func injectEnvVars(containers []corev1.Container) {
	for n := range containers {
		containers[n].Env = append(containers[n].Env, corev1.EnvVar{
			Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}},
		})
	}
}

func fixupProbes(containers []corev1.Container) {
	for n := range containers {
		if containers[n].LivenessProbe != nil && containers[n].LivenessProbe.HTTPGet != nil {
			containers[n].LivenessProbe.HTTPGet.Host = ""
		}
		if containers[n].ReadinessProbe != nil && containers[n].ReadinessProbe.HTTPGet != nil {
			containers[n].ReadinessProbe.HTTPGet.Host = ""
		}
		if containers[n].StartupProbe != nil && containers[n].StartupProbe.HTTPGet != nil {
			containers[n].StartupProbe.HTTPGet.Host = ""
		}
	}
}

func fixupBindAddress(containers []corev1.Container) {
	for n := range containers {
		for i := range containers[n].Command {
			containers[n].Command[i] = strings.ReplaceAll(containers[n].Command[i], "--bind-address=127.0.0.1", "--bind-address=0.0.0.0")
		}
	}
}

func fixupListenMetricUrls(containers []corev1.Container) {
	for n := range containers {
		for i := range containers[n].Command {
			containers[n].Command[i] = strings.ReplaceAll(containers[n].Command[i], "--listen-metrics-urls=http://127.0.0.1:2381", "--listen-metrics-urls=http://0.0.0.0:2381")
		}
	}
}

func (c *Configuration) UploadConfig(client clientset.Interface) error {
	initConfig := kubeadmapi.InitConfiguration{}
	scheme.Scheme.Default(&c.InitConfiguration)
	scheme.Scheme.Default(&c.ClusterConfiguration)
	scheme.Scheme.Convert(&c.InitConfiguration, &initConfig, nil)
	scheme.Scheme.Convert(&c.ClusterConfiguration, &initConfig.ClusterConfiguration, nil)
	componentconfigs.Default(&initConfig.ClusterConfiguration, &initConfig.LocalAPIEndpoint, &initConfig.NodeRegistration)
	klog.Infof("initConfig: %+v", initConfig)
	if err := uploadconfig.UploadConfiguration(&initConfig, client); err != nil {
		return fmt.Errorf("failed to UploadConfiguration: %w", err)
	}
	if err := kubelet.CreateConfigMap(&initConfig.ClusterConfiguration, client); err != nil {
		return fmt.Errorf("failed to CreateConfigMap: %w", err)
	}
	return nil
}

func (c *Configuration) EnsureAddons(client clientset.Interface) error {
	initConfig := kubeadmapi.InitConfiguration{}
	scheme.Scheme.Default(&c.InitConfiguration)
	scheme.Scheme.Default(&c.ClusterConfiguration)
	scheme.Scheme.Convert(&c.InitConfiguration, &initConfig, nil)
	scheme.Scheme.Convert(&c.ClusterConfiguration, &initConfig.ClusterConfiguration, nil)
	componentconfigs.Default(&initConfig.ClusterConfiguration, &initConfig.LocalAPIEndpoint, &initConfig.NodeRegistration)
	if err := addonproxy.EnsureProxyAddon(&initConfig.ClusterConfiguration, &initConfig.LocalAPIEndpoint, client); err != nil {
		return err
	}
	return addondns.EnsureDNSAddon(&initConfig.ClusterConfiguration, client)
}

func ControlPlaneServiceSpec() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "controlplane",
			Labels: map[string]string{
				"app": "controlplane",
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
			Selector: map[string]string{
				"app": "controlplane",
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "kube-apiserver",
					Protocol:   corev1.ProtocolTCP,
					Port:       6443,
					TargetPort: intstr.FromInt(6443),
				},
			},
		},
	}
}

func ProvisionBootstrapToken(client clientset.Interface, kubeconfig []byte) error {
	// Create RBAC rules that makes the bootstrap tokens able to post CSRs
	if err := bootstraptokennode.AllowBootstrapTokensToPostCSRs(client); err != nil {
		return errors.Wrap(err, "error allowing bootstrap tokens to post CSRs")
	}
	// Create RBAC rules that makes the bootstrap tokens able to get their CSRs approved automatically
	if err := bootstraptokennode.AutoApproveNodeBootstrapTokens(client); err != nil {
		return errors.Wrap(err, "error auto-approving node bootstrap tokens")
	}

	// Create/update RBAC rules that makes the nodes to rotate certificates and get their CSRs approved automatically
	if err := bootstraptokennode.AutoApproveNodeCertificateRotation(client); err != nil {
		return err
	}

	// Create the cluster-info ConfigMap with the associated RBAC rules
	if err := createBootstrapConfigMapIfNotExists(client, kubeconfig); err != nil {
		return errors.Wrap(err, "error creating bootstrap ConfigMap")
	}
	if err := clusterinfo.CreateClusterInfoRBACRules(client); err != nil {
		return errors.Wrap(err, "error creating clusterinfo RBAC rules")
	}
	return nil
}

func createBootstrapConfigMapIfNotExists(client clientset.Interface, kubeconfig []byte) error {
	adminConfig, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return errors.Wrap(err, "failed to load admin kubeconfig")
	}

	adminCluster := adminConfig.Contexts[adminConfig.CurrentContext].Cluster
	bootstrapConfig := &clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			"": adminConfig.Clusters[adminCluster],
		},
	}
	bootstrapBytes, err := clientcmd.Write(*bootstrapConfig)
	if err != nil {
		return err
	}

	return apiclient.CreateOrUpdateConfigMap(client, &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bootstrapapi.ConfigMapClusterInfo,
			Namespace: metav1.NamespacePublic,
		},
		Data: map[string]string{
			bootstrapapi.KubeConfigKey: string(bootstrapBytes),
		},
	})
}

func hostPathTypePtr(h corev1.HostPathType) *corev1.HostPathType {
	return &h
}
