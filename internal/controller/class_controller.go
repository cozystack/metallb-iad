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
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
)

// ClassReconciler checks each IPAddressClass this driver serves
// (servesClass) against the MetalLB pool it names, and reports the result in
// the class's status: the loadBalancerClass of the MetalLB the driver fronts,
// and an Accepted condition that is False, with an event, when the pool
// cannot back new reservations. The pool and its advertisements are the
// admin's; the driver only reads them.
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
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddressclasses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=metallb.io,resources=ipaddresspools,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile checks one class.
func (r *ClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	class := &localv1alpha1.IPAddressClass{}
	if err := r.Get(ctx, req.NamespacedName, class); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !class.DeletionTimestamp.IsZero() || !servesClass(class) {
		return ctrl.Result{}, nil
	}

	pool, problem, err := classPool(ctx, r.Client, r.MetalLBNamespace, class)
	if err != nil {
		return ctrl.Result{}, err
	}
	accepted := metav1.Condition{
		Type:               localv1alpha1.ConditionAccepted,
		Status:             metav1.ConditionTrue,
		Reason:             localv1alpha1.ReasonAccepted,
		Message:            fmt.Sprintf("IPAddressPool %s/%s backs this class", r.MetalLBNamespace, pool),
		ObservedGeneration: class.Generation,
	}
	if problem != "" {
		r.Recorder.Eventf(class, "Warning", ReasonPoolRefused, "%s; claims of this class are not provisioned", problem)
		accepted.Status = metav1.ConditionFalse
		accepted.Reason = ReasonPoolRefused
		accepted.Message = problem
	} else {
		logger.V(1).Info("class pool usable", "pool", pool)
	}
	return ctrl.Result{}, r.reportStatus(ctx, class, accepted)
}

// reportStatus writes the class's status: the loadBalancerClass of the
// MetalLB this driver fronts, which consumers of the class's claims must set
// on their Services, and the Accepted condition.
func (r *ClassReconciler) reportStatus(ctx context.Context, class *localv1alpha1.IPAddressClass, accepted metav1.Condition) error {
	base := class.DeepCopy()
	changed := meta.SetStatusCondition(&class.Status.Conditions, accepted)
	if class.Status.LoadBalancerClass != r.LoadBalancerClass {
		class.Status.LoadBalancerClass = r.LoadBalancerClass
		changed = true
	}
	if !changed {
		return nil
	}
	return r.Status().Patch(ctx, class, client.MergeFrom(base))
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
		if servesClass(&c) {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: c.Name}})
		}
	}
	return reqs
}

// ReasonPoolRefused is the reason of the event, and of the Accepted=False
// condition, on a class whose pool cannot back new reservations.
const ReasonPoolRefused = "PoolRefused"

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
