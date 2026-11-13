/*
Copyright 2026 The Cozystack Authors.

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

package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	localv1alpha1 "github.com/cozystack/address-controller/api/v1alpha1"
	"github.com/cozystack/metallb-iad/internal/driver"
)

// ClassReconciler checks each IPAddressClass served by this driver against
// the MetalLB pool it names and reports, as events on the class, why the
// class cannot back new reservations. The pool and its advertisements are
// the admin's; the driver only reads them.
type ClassReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// MetalLBNamespace is where MetalLB's configuration resources live.
	MetalLBNamespace string
	// LoadBalancerClass is that of the MetalLB instance this driver fronts;
	// empty for the cluster default.
	LoadBalancerClass string
}

// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddressclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=metallb.io,resources=ipaddresspools,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile checks one class.
func (r *ClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	class := &localv1alpha1.IPAddressClass{}
	if err := r.Get(ctx, req.NamespacedName, class); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !class.DeletionTimestamp.IsZero() || class.Spec.Provisioner != driver.ProvisionerName {
		return ctrl.Result{}, nil
	}

	pool, problem, err := classPool(ctx, r.Client, r.MetalLBNamespace, class)
	if err != nil {
		return ctrl.Result{}, err
	}
	if problem != "" {
		r.Recorder.Eventf(class, "Warning", "PoolRefused", "%s; claims of this class are not provisioned", problem)
	} else {
		logger.V(1).Info("class pool usable", "pool", pool)
	}

	// An auto-claimed Service carries no loadBalancerClass, so only the
	// cluster's default implementation can serve it.
	if class.Annotations[localv1alpha1.IsAutoClaimClassAnnotation] == "true" && r.LoadBalancerClass != "" {
		r.Recorder.Eventf(class, "Warning", "AutoClaimUnservable",
			"class is marked for auto-claim, but this driver fronts loadBalancerClass %q, which auto-claimed Services do not carry",
			r.LoadBalancerClass)
	}
	return ctrl.Result{}, nil
}

// classesOfOurs maps any event to every class this driver serves: a pool
// change can make a class usable or unusable, and one class naming a pool
// can make another class's use of it ambiguous.
func (r *ClassReconciler) classesOfOurs(ctx context.Context, _ client.Object) []reconcile.Request {
	classes := &localv1alpha1.IPAddressClassList{}
	if err := r.List(ctx, classes); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, c := range classes.Items {
		if c.Spec.Provisioner == driver.ProvisionerName {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: c.Name}})
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	pool := &unstructured.Unstructured{}
	pool.SetGroupVersionKind(ipAddressPoolGVK)
	return ctrl.NewControllerManagedBy(mgr).
		For(&localv1alpha1.IPAddressClass{}).
		Watches(pool, handler.EnqueueRequestsFromMapFunc(r.classesOfOurs)).
		Watches(&localv1alpha1.IPAddressClass{}, handler.EnqueueRequestsFromMapFunc(r.classesOfOurs)).
		Named("iad-class").
		Complete(r)
}
