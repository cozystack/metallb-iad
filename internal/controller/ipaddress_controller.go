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

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	localv1alpha1 "github.com/lllamnyp/address-controller/api/v1alpha1"
	"github.com/lllamnyp/metallb-iad/internal/driver"
)

// IPAddressReconciler is the teardown half of the driver contract, plus
// conflict recovery. On deletion of an IPAddress carrying this driver's
// finalizer it withdraws any live pin and lets the object go — MetalLB has
// no per-address backend object, so there is nothing else to deallocate.
// For addresses the association layer drove to Conflict, it clears the
// phase once no Service wrongfully holds the address anymore, handing the
// object back to the core controller's bookkeeping.
type IPAddressReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddresses,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddresses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile handles teardown and conflict recovery for one IPAddress.
func (r *IPAddressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	addr := &localv1alpha1.IPAddress{}
	if err := r.Get(ctx, req.NamespacedName, addr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !addr.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, addr)
	}

	if addr.Status.Phase == localv1alpha1.IPAddressConflict {
		return ctrl.Result{}, r.maybeClearConflict(ctx, addr)
	}
	return ctrl.Result{}, nil
}

// finalize withdraws a live pin and releases the teardown finalizer.
func (r *IPAddressReconciler) finalize(ctx context.Context, addr *localv1alpha1.IPAddress) error {
	if !controllerutil.ContainsFinalizer(addr, driver.TeardownFinalizer) {
		return nil
	}
	if holder := addr.Status.AssociatedTo; holder != nil && holder.Kind == "Service" {
		svc := &corev1.Service{}
		err := r.Get(ctx, types.NamespacedName{Namespace: holder.Namespace, Name: holder.Name}, svc)
		if err == nil && svc.Annotations[driver.PinnedAnnotation] == "true" {
			delete(svc.Annotations, driver.MetalLBPinAnnotation)
			delete(svc.Annotations, driver.PinnedAnnotation)
			if err := r.Update(ctx, svc); err != nil {
				return err
			}
			r.Recorder.Eventf(svc, "Normal", "Disassociated",
				"withdrew MetalLB pin: IPAddress %s is being deleted", addr.Name)
		} else if client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	controllerutil.RemoveFinalizer(addr, driver.TeardownFinalizer)
	return r.Update(ctx, addr)
}

// maybeClearConflict returns a conflicted address to core-owned bookkeeping
// once no Service wrongfully holds it.
func (r *IPAddressReconciler) maybeClearConflict(ctx context.Context, addr *localv1alpha1.IPAddress) error {
	services := &corev1.ServiceList{}
	if err := r.List(ctx, services); err != nil {
		return err
	}
	for i := range services.Items {
		svc := &services.Items[i]
		for _, ingress := range svc.Status.LoadBalancer.Ingress {
			if ingress.IP == addr.Spec.Address && !authorizes(addr, svc) {
				return nil // still conflicted
			}
		}
	}
	// Hand back to the core controller: Bound if the binding survived,
	// Available otherwise; the core reconciler settles the exact phase.
	if addr.Spec.ClaimRef != nil {
		addr.Status.Phase = localv1alpha1.IPAddressBound
	} else {
		addr.Status.Phase = localv1alpha1.IPAddressAvailable
	}
	if err := r.Status().Update(ctx, addr); err != nil {
		return err
	}
	r.Recorder.Event(addr, "Normal", "ConflictResolved",
		"no Service wrongfully holds this address anymore")
	return nil
}

// addressesForService requeues the IPAddress objects matching a Service's
// live assignments, so a withdrawn wrongful assignment clears Conflict.
func (r *IPAddressReconciler) addressesForService(ctx context.Context, o client.Object) []reconcile.Request {
	svc := o.(*corev1.Service)
	var reqs []reconcile.Request
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if ingress.IP == "" {
			continue
		}
		addresses := &localv1alpha1.IPAddressList{}
		if err := r.List(ctx, addresses, client.MatchingFields{IPAddressByAddressIndex: ingress.IP}); err != nil {
			continue
		}
		for _, addr := range addresses.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: addr.Name}})
		}
	}
	// A Service deletion may also resolve a conflict; requeue every
	// conflicted address when a Service goes away is covered by the list
	// above only while status still carries the IP, so also requeue
	// conflicted addresses cheaply.
	conflicted := &localv1alpha1.IPAddressList{}
	if err := r.List(ctx, conflicted); err == nil {
		for _, addr := range conflicted.Items {
			if addr.Status.Phase == localv1alpha1.IPAddressConflict {
				reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: addr.Name}})
			}
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *IPAddressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&localv1alpha1.IPAddress{}).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(r.addressesForService)).
		Named("iad-ipaddress").
		Complete(r)
}
