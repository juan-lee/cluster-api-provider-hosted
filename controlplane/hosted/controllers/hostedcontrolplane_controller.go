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

package controllers

import (
	"context"
	"fmt"
	"time"

	controlplanev1 "github.com/juan-lee/cluster-api-provider-hosted/controlplane/hosted/api/v1alpha4"
	"github.com/juan-lee/cluster-api-provider-hosted/internal/kubeadm"
	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha4"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1alpha4"
	"sigs.k8s.io/cluster-api/controllers/remote"
	internal "sigs.k8s.io/cluster-api/controlplane/kubeadm/util"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/certs"
	"sigs.k8s.io/cluster-api/util/kubeconfig"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	"sigs.k8s.io/cluster-api/util/secret"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// HostedControlPlaneReconciler reconciles a HostedControlPlane object
type HostedControlPlaneReconciler struct {
	client.Client
	controller       controller.Controller
	recorder         record.EventRecorder
	Tracker          *remote.ClusterCacheTracker
	WatchFilterValue string

	managementCluster         internal.ManagementCluster
	managementClusterUncached internal.ManagementCluster
}

//+kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=hostedcontrolplanes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=hostedcontrolplanes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=hostedcontrolplanes/finalizers,verbs=update
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete

// SetupWithManager sets up the controller with the Manager.
func (r *HostedControlPlaneReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&controlplanev1.HostedControlPlane{}).
		// Owns(&clusterv1.Machine{}).
		WithOptions(options).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(ctrl.LoggerFrom(ctx), r.WatchFilterValue)).
		Build(r)
	if err != nil {
		return errors.Wrap(err, "failed setting up with a controller manager")
	}

	err = c.Watch(
		&source.Kind{Type: &clusterv1.Cluster{}},
		handler.EnqueueRequestsFromMapFunc(r.ClusterToHostedControlPlane),
		predicates.All(ctrl.LoggerFrom(ctx),
			predicates.ResourceHasFilterLabel(ctrl.LoggerFrom(ctx), r.WatchFilterValue),
			predicates.ClusterUnpausedAndInfrastructureReady(ctrl.LoggerFrom(ctx)),
		),
	)
	if err != nil {
		return errors.Wrap(err, "failed adding Watch for Clusters to controller manager")
	}

	r.controller = c
	r.recorder = mgr.GetEventRecorderFor("hosted-control-plane-controller")

	if r.managementCluster == nil {
		// TODO: figure out the tracker
		// if r.Tracker == nil {
		// 	return errors.New("cluster cache tracker is nil, cannot create the internal management cluster resource")
		// }
		r.managementCluster = &internal.Management{Client: r.Client, Tracker: r.Tracker}
	}

	if r.managementClusterUncached == nil {
		r.managementClusterUncached = &internal.Management{Client: mgr.GetAPIReader()}
	}

	return nil
}

// ClusterToHostedControlPlane is a handler.ToRequestsFunc to be used to enqueue requests for reconciliation
// for HostedControlPlane based on updates to a Cluster.
func (r *HostedControlPlaneReconciler) ClusterToHostedControlPlane(o client.Object) []ctrl.Request {
	c, ok := o.(*clusterv1.Cluster)
	if !ok {
		panic(fmt.Sprintf("Expected a Cluster but got a %T", o))
	}

	controlPlaneRef := c.Spec.ControlPlaneRef
	if controlPlaneRef != nil && controlPlaneRef.Kind == "HostedControlPlane" {
		return []ctrl.Request{{NamespacedName: client.ObjectKey{Namespace: controlPlaneRef.Namespace, Name: controlPlaneRef.Name}}}
	}

	return nil
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *HostedControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, reterr error) {
	log := ctrl.LoggerFrom(ctx)

	// Fetch the HostedControlPlane instance.
	hcp := &controlplanev1.HostedControlPlane{}
	if err := r.Client.Get(ctx, req.NamespacedName, hcp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Fetch the Cluster.
	cluster, err := util.GetOwnerCluster(ctx, r.Client, hcp.ObjectMeta)
	if err != nil {
		log.Error(err, "Failed to retrieve owner Cluster from the API Server")
		return ctrl.Result{}, err
	}
	if cluster == nil {
		log.Info("Cluster Controller has not yet set OwnerRef")
		return ctrl.Result{}, nil
	}
	log = log.WithValues("cluster", cluster.Name)

	if annotations.IsPaused(cluster, hcp) {
		log.Info("Reconciliation is paused for this object")
		return ctrl.Result{}, nil
	}

	// Wait for the cluster infrastructure to be ready before creating machines
	if !cluster.Status.InfrastructureReady {
		return ctrl.Result{}, nil
	}

	// Initialize the patch helper.
	patchHelper, err := patch.NewHelper(hcp, r.Client)
	if err != nil {
		log.Error(err, "Failed to configure the patch helper")
		return ctrl.Result{Requeue: true}, nil
	}

	// Add finalizer first if not exist to avoid the race condition between init and delete
	if !controllerutil.ContainsFinalizer(hcp, controlplanev1.HostedControlPlaneFinalizer) {
		controllerutil.AddFinalizer(hcp, controlplanev1.HostedControlPlaneFinalizer)

		// patch and return right away instead of reusing the main defer,
		// because the main defer may take too much time to get cluster status
		// Patch ObservedGeneration only if the reconciliation completed successfully
		patchOpts := []patch.Option{patch.WithStatusObservedGeneration{}}
		if err := patchHelper.Patch(ctx, hcp, patchOpts...); err != nil {
			log.Error(err, "Failed to patch HostedControlPlane to add finalizer")
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	defer func() {
		// Always attempt to Patch the HostedControlPlane object and status after each reconciliation.
		if err := patchHostedControlPlane(ctx, patchHelper, hcp); err != nil {
			log.Error(err, "Failed to patch HostedControlPlane")
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}

		// TODO: remove this as soon as we have a proper remote cluster cache in place.
		// Make hcp to requeue in case status is not ready, so we can check for node status without waiting for a full resync (by default 10 minutes).
		// Only requeue if we are not going in exponential backoff due to error, or if we are not already re-queueing, or if the object has a deletion timestamp.
		if reterr == nil && !res.Requeue && !(res.RequeueAfter > 0) && hcp.ObjectMeta.DeletionTimestamp.IsZero() {
			if !hcp.Status.Ready {
				res = ctrl.Result{RequeueAfter: 20 * time.Second}
			}
		}
	}()

	if !hcp.ObjectMeta.DeletionTimestamp.IsZero() {
		// Handle deletion reconciliation loop.
		return r.reconcileDelete(ctx, cluster, hcp)
	}

	// Handle normal reconciliation loop.
	return r.reconcile(ctx, cluster, hcp)
}

func (r *HostedControlPlaneReconciler) reconcile(ctx context.Context, cluster *clusterv1.Cluster, hcp *controlplanev1.HostedControlPlane) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx, "cluster", cluster.Name)
	log.Info("Reconcile HostedControlPlane")

	if result, err := r.reconcileSecrets(ctx, cluster, hcp); err != nil {
		return result, err
	}
	if result, err := r.reconcileDeployment(ctx, cluster, hcp); err != nil {
		return result, err
	}
	if result, err := r.reconcileKubeconfig(ctx, cluster, hcp); err != nil {
		return result, err
	}

	return ctrl.Result{}, nil
}

func (r *HostedControlPlaneReconciler) reconcileSecrets(ctx context.Context, cluster *clusterv1.Cluster, hcp *controlplanev1.HostedControlPlane) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx, "cluster", cluster.Name)
	log.Info("Reconcile HostedControlPlane Secrets")

	// Generate Cluster Certificates if needed
	config := hcp.Spec.KubeadmConfigSpec.DeepCopy()
	config.JoinConfiguration = nil
	if config.ClusterConfiguration == nil {
		config.ClusterConfiguration = &bootstrapv1.ClusterConfiguration{}
	}
	config.ClusterConfiguration.ControlPlaneEndpoint = cluster.Spec.ControlPlaneEndpoint.Host
	certificates := secret.NewCertificatesForInitialControlPlane(config.ClusterConfiguration)
	controllerRef := metav1.NewControllerRef(hcp, controlplanev1.GroupVersion.WithKind("HostedControlPlane"))
	if err := certificates.LookupOrGenerate(ctx, r.Client, util.ObjectKey(cluster), *controllerRef); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "unable to lookup or create cluster certificates")
	}

	kcfg, err := kubeadm.New(cluster.Name, config.InitConfiguration, config.ClusterConfiguration)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to get kubeadm deployment builder")
	}

	secret, err := kcfg.GenerateSecret(config.InitConfiguration, config.ClusterConfiguration)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to kubeadm config secret")
	}
	secret.Namespace = hcp.Namespace
	existingSecret := corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}, &existingSecret); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("kubeadm config not found, creating...")

			if err := r.Create(ctx, secret); err != nil {
				return ctrl.Result{}, errors.Wrap(err, "failed to create kubeadm config secret")
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, errors.Wrap(err, "failed to get kubeadm config secret")
	}

	patchHelper, err := patch.NewHelper(&existingSecret, r.Client)
	if err != nil {
		return ctrl.Result{Requeue: true}, errors.Wrap(err, "failed to configure patch helper")
	}
	existingSecret.Data = secret.Data
	if err := patchHelper.Patch(ctx, &existingSecret); err != nil {
		log.Info("patch failed", "secret", *secret)
		return ctrl.Result{}, errors.Wrap(err, "failed to patch kubeadm config secret")
	}
	return ctrl.Result{}, nil
}

func (r *HostedControlPlaneReconciler) reconcileDeployment(ctx context.Context, cluster *clusterv1.Cluster, hcp *controlplanev1.HostedControlPlane) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx, "cluster", cluster.Name)
	log.Info("Reconcile HostedControlPlane Deployment")

	config := hcp.Spec.KubeadmConfigSpec.DeepCopy()
	config.JoinConfiguration = nil
	if config.ClusterConfiguration == nil {
		config.ClusterConfiguration = &bootstrapv1.ClusterConfiguration{}
	}
	config.ClusterConfiguration.ControlPlaneEndpoint = cluster.Spec.ControlPlaneEndpoint.Host

	kcfg, err := kubeadm.New(cluster.Name, config.InitConfiguration, config.ClusterConfiguration)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to get kubeadm deployment builder")
	}

	desired := kcfg.ControlPlaneDeploymentSpec()
	desired.Namespace = hcp.Namespace
	if err := controllerutil.SetOwnerReference(hcp, desired, r.Client.Scheme()); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to set owner ref")
	}

	existing := appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, &existing); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Control Plane pod not found, creating...")

			if err := r.Create(ctx, desired); err != nil {
				return ctrl.Result{}, errors.Wrap(err, "failed to create deployment")
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, errors.Wrap(err, "failed to get deployment")
	}

	patchHelper, err := patch.NewHelper(&existing, r.Client)
	if err != nil {
		return ctrl.Result{Requeue: true}, errors.Wrap(err, "failed to configure patch helper")
	}
	existing.Spec.Template.Spec = desired.Spec.Template.Spec
	log.Info("Existing vs desired", "existing", existing, "desired", *desired)
	patchOpts := []patch.Option{patch.WithStatusObservedGeneration{}}
	if err := patchHelper.Patch(ctx, &existing, patchOpts...); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to patch deployment")
	}

	if existing.Status.ReadyReplicas == 0 {
		return ctrl.Result{}, errors.New("Control Plane pod isn't ready")
	}

	return ctrl.Result{}, nil
}

func (r *HostedControlPlaneReconciler) reconcileKubeconfig(ctx context.Context, cluster *clusterv1.Cluster, hcp *controlplanev1.HostedControlPlane) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	endpoint := cluster.Spec.ControlPlaneEndpoint
	if endpoint.IsZero() {
		return ctrl.Result{}, nil
	}
	controllerOwnerRef := *metav1.NewControllerRef(hcp, controlplanev1.GroupVersion.WithKind("HostedControlPlane"))
	clusterName := util.ObjectKey(cluster)
	log.Info("reconcileKubeconfig", "clusterName", clusterName)
	configSecret, err := secret.GetFromNamespacedName(ctx, r.Client, clusterName, secret.Kubeconfig)
	switch {
	case apierrors.IsNotFound(err):
		log.Info("kubeconfig secret not found, creating...")
		createErr := kubeconfig.CreateSecretWithOwner(
			ctx,
			r.Client,
			clusterName,
			endpoint.String(),
			controllerOwnerRef,
		)
		if errors.Is(createErr, kubeconfig.ErrDependentCertificateNotFound) {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		// always return if we have just created in order to skip rotation checks
		return ctrl.Result{}, createErr
	case err != nil:
		return ctrl.Result{}, errors.Wrap(err, "failed to retrieve kubeconfig Secret")
	}

	// only do rotation on owned secrets
	if !util.IsControlledBy(configSecret, hcp) {
		return ctrl.Result{}, nil
	}

	needsRotation, err := kubeconfig.NeedsClientCertRotation(configSecret, certs.ClientCertificateRenewalDuration)
	if err != nil {
		return ctrl.Result{}, err
	}

	if needsRotation {
		log.Info("rotating kubeconfig secret")
		if err := kubeconfig.RegenerateSecret(ctx, r.Client, configSecret); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to regenerate kubeconfig")
		}
	}

	return ctrl.Result{}, nil
}

func (r *HostedControlPlaneReconciler) reconcileDelete(ctx context.Context, cluster *clusterv1.Cluster, hcp *controlplanev1.HostedControlPlane) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx, "cluster", cluster.Name)
	log.Info("Delete HostedControlPlane")

	secrets := []string{"kubeconfig", "ca", "etcd", "proxy", "sa", "config"}
	for _, suffix := range secrets {
		name := types.NamespacedName{Namespace: hcp.Namespace, Name: fmt.Sprintf("%s-%s", cluster.Name, suffix)}
		secret := corev1.Secret{}
		if err := r.Get(ctx, name, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				log.Info("Secret not found, skipping", "name", name)
				continue
			}
			return ctrl.Result{}, err
		}
		if err := r.Client.Delete(ctx, &secret); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("Deleted Secret", "name", name)
	}

	name := types.NamespacedName{Namespace: hcp.Namespace, Name: fmt.Sprintf("%s-hcp", cluster.Name)}
	deploy := appsv1.Deployment{}
	if err := r.Get(ctx, name, &deploy); err == nil {
		if err := r.Client.Delete(ctx, &deploy); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("Deleted Deployment", "name", name)
	}

	controllerutil.RemoveFinalizer(hcp, controlplanev1.HostedControlPlaneFinalizer)

	return ctrl.Result{}, nil
}

func patchHostedControlPlane(ctx context.Context, patchHelper *patch.Helper, hcp *controlplanev1.HostedControlPlane) error {
	// Always update the readyCondition by summarizing the state of other conditions.
	// conditions.SetSummary(hcp,
	// 	conditions.WithConditions(),
	// )

	// Patch the object, ignoring conflicts on the conditions owned by this controller.
	return patchHelper.Patch(
		ctx,
		hcp,
		patch.WithOwnedConditions{Conditions: []clusterv1.ConditionType{}},
		patch.WithStatusObservedGeneration{},
	)
}
