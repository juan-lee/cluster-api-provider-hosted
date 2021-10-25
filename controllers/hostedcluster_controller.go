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
	"time"

	"github.com/go-logr/logr"
	infrav1 "github.com/juan-lee/cluster-api-provider-hosted/api/v1alpha4"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/intstr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha4"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// HostedClusterReconciler reconciles a HostedCluster object
type HostedClusterReconciler struct {
	client.Client
	Log logr.Logger
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hostedclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hostedclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hostedclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

// SetupWithManager sets up the controller with the Manager.
func (r *HostedClusterReconciler) SetupWithManager(mgr ctrl.Manager, options controller.Options) error {
	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.HostedCluster{}).
		WithOptions(options).
		WithEventFilter(predicates.ResourceNotPaused(r.Log)).
		Build(r)
	if err != nil {
		return err
	}
	return c.Watch(
		&source.Kind{Type: &clusterv1.Cluster{}},
		handler.EnqueueRequestsFromMapFunc(util.ClusterToInfrastructureMapFunc(infrav1.GroupVersion.WithKind("HostedCluster"))),
		predicates.ClusterUnpaused(r.Log),
	)
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the HostedCluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *HostedClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, reterr error) {
	log := ctrl.LoggerFrom(ctx)

	// Fetch the HostedCluster instance.
	hc := &infrav1.HostedCluster{}
	if err := r.Client.Get(ctx, req.NamespacedName, hc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Fetch the Cluster.
	cluster, err := util.GetOwnerCluster(ctx, r.Client, hc.ObjectMeta)
	if err != nil {
		log.Error(err, "Failed to retrieve owner Cluster from the API Server")
		return ctrl.Result{}, err
	}
	if cluster == nil {
		log.Info("Cluster Controller has not yet set OwnerRef")
		return ctrl.Result{}, nil
	}
	log = log.WithValues("cluster", cluster.Name)

	if annotations.IsPaused(cluster, hc) {
		log.Info("Reconciliation is paused for this object")
		return ctrl.Result{}, nil
	}

	// Initialize the patch helper.
	patchHelper, err := patch.NewHelper(hc, r.Client)
	if err != nil {
		log.Error(err, "Failed to configure the patch helper")
		return ctrl.Result{Requeue: true}, nil
	}

	// Add finalizer first if not exist to avoid the race condition between init and delete
	if !controllerutil.ContainsFinalizer(hc, infrav1.HostedClusterFinalizer) {
		controllerutil.AddFinalizer(hc, infrav1.HostedClusterFinalizer)

		// patch and return right away instead of reusing the main defer,
		// because the main defer may take too much time to get cluster status
		// Patch ObservedGeneration only if the reconciliation completed successfully
		patchOpts := []patch.Option{patch.WithStatusObservedGeneration{}}
		if err := patchHelper.Patch(ctx, hc, patchOpts...); err != nil {
			log.Error(err, "Failed to patch HostedCluster to add finalizer")
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	defer func() {
		// Always attempt to Patch the HostedCluster object and status after each reconciliation.
		if err := patchHostedCluster(ctx, patchHelper, hc); err != nil {
			log.Error(err, "Failed to patch HostedCluster")
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}

		// TODO: remove this as soon as we have a proper remote cluster cache in place.
		// Make hc to requeue in case status is not ready, so we can check for node status without waiting for a full resync (by default 10 minutes).
		// Only requeue if we are not going in exponential backoff due to error, or if we are not already re-queueing, or if the object has a deletion timestamp.
		if reterr == nil && !res.Requeue && !(res.RequeueAfter > 0) && hc.ObjectMeta.DeletionTimestamp.IsZero() {
			if !hc.Status.Ready {
				res = ctrl.Result{RequeueAfter: 20 * time.Second}
			}
		}
	}()

	if !hc.ObjectMeta.DeletionTimestamp.IsZero() {
		// Handle deletion reconciliation loop.
		return r.reconcileDelete(ctx, cluster, hc)
	}

	// Handle normal reconciliation loop.
	return r.reconcile(ctx, cluster, hc)
}

func (r *HostedClusterReconciler) reconcile(ctx context.Context, cluster *clusterv1.Cluster, hc *infrav1.HostedCluster) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx, "cluster", cluster.Name)
	log.Info("Reconcile HostedCluster")

	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      hc.Name,
			Namespace: hc.Namespace,
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

	existing := corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, &existing); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Control Plane service not found, creating...")

			if err := r.Create(ctx, desired); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if len(existing.Status.LoadBalancer.Ingress) > 0 && len(existing.Spec.Ports) > 0 {
		hc.Spec.ControlPlaneEndpoint.Host = existing.Status.LoadBalancer.Ingress[0].IP
		hc.Spec.ControlPlaneEndpoint.Port = existing.Spec.Ports[0].Port
		hc.Status.Ready = true
	}

	// if err := r.Update(ctx, hc); err != nil {
	// 	return ctrl.Result{}, err
	// }

	// if err := r.Status().Update(ctx, hc); err != nil {
	// 	return ctrl.Result{}, err
	// }

	return ctrl.Result{}, nil
}

func (r *HostedClusterReconciler) reconcileDelete(ctx context.Context, cluster *clusterv1.Cluster, hc *infrav1.HostedCluster) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx, "cluster", cluster.Name)
	log.Info("Delete HostedCluster")

	service := corev1.Service{}
	name := types.NamespacedName{Namespace: hc.Namespace, Name: hc.Name}
	if err := r.Get(ctx, name, &service); err == nil {
		if err := r.Delete(ctx, &service); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("Deleted Service", "name", name)
	}

	controllerutil.RemoveFinalizer(hc, infrav1.HostedClusterFinalizer)

	return ctrl.Result{}, nil
}

func patchHostedCluster(ctx context.Context, patchHelper *patch.Helper, hc *infrav1.HostedCluster) error {
	// Always update the readyCondition by summarizing the state of other conditions.
	// conditions.SetSummary(hcp,
	// 	conditions.WithConditions(),
	// )

	// Patch the object, ignoring conflicts on the conditions owned by this controller.
	return patchHelper.Patch(
		ctx,
		hc,
		patch.WithOwnedConditions{Conditions: []clusterv1.ConditionType{}},
		patch.WithStatusObservedGeneration{},
	)
}
