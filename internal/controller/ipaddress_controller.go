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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// IPAddressReconciler enforces the reservation-hold invariant — a
// placeholder Service holds an address in MetalLB's books exactly while
// the address exists and is not associated to a workload — and owns
// teardown and conflict recovery. It also clears associations whose
// holder Service no longer exists, which is what re-arms the hold after
// a workload is deleted.
type IPAddressReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// PlaceholderNamespace is where reservation placeholders live.
	PlaceholderNamespace string
}

// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddresses,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddresses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddressclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives one IPAddress's hold, teardown, and conflict recovery.
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

	// The hold invariant applies only to addresses of classes this driver
	// serves, and only when the address parses.
	ours, err := r.servedByUs(ctx, addr)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ours || driver.FamilyOf(addr.Spec.Address) == "" {
		return ctrl.Result{}, nil
	}

	if holder := addr.Status.AssociatedTo; holder != nil {
		live, err := r.holderLive(ctx, holder)
		if err != nil {
			return ctrl.Result{}, err
		}
		if live {
			// Associated: the real Service holds the address; a lingering
			// placeholder would fight it for the pin.
			return ctrl.Result{}, r.deletePlaceholders(ctx, addr.Name)
		}
		// The holder is gone (its deletion event may have been missed):
		// the association is stale. Clearing it re-arms the hold below.
		addr.Status.AssociatedTo = nil
		if err := r.Status().Update(ctx, addr); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(addr, "Normal", "AssociationCleared",
			"associated Service %s/%s no longer exists", holder.Namespace, holder.Name)
	}

	// Unassociated: reserved but inert — ensure a placeholder holds the
	// address in MetalLB's books so nothing else can be assigned it.
	return ctrl.Result{}, r.ensureHold(ctx, addr)
}

// servedByUs reports whether the address's class names this driver.
func (r *IPAddressReconciler) servedByUs(ctx context.Context, addr *localv1alpha1.IPAddress) (bool, error) {
	class := &localv1alpha1.IPAddressClass{}
	err := r.Get(ctx, types.NamespacedName{Name: addr.Spec.ClassName}, class)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return class.Spec.Provisioner == driver.ProvisionerName, nil
}

// holderLive reports whether the associated workload still exists. Holder
// kinds other than Service are treated as live, conservatively.
func (r *IPAddressReconciler) holderLive(ctx context.Context, holder *localv1alpha1.AssociationReference) (bool, error) {
	if holder.Kind != "Service" {
		return true, nil
	}
	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Namespace: holder.Namespace, Name: holder.Name}, svc)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return svc.DeletionTimestamp.IsZero(), nil
}

// ensureHold makes sure exactly one placeholder is pinned to the address.
// The allocation placeholder, once linked, satisfies this; after a
// disassociation a fresh hold placeholder is created.
func (r *IPAddressReconciler) ensureHold(ctx context.Context, addr *localv1alpha1.IPAddress) error {
	existing, err := r.placeholdersFor(ctx, addr.Name)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		return nil
	}
	placeholder := driver.NewPlaceholder(
		driver.HoldPlaceholderName(addr.Name), r.PlaceholderNamespace,
		addr.Spec.ClassName, driver.FamilyOf(addr.Spec.Address), addr.Spec.Address)
	if err := controllerutil.SetOwnerReference(addr, placeholder, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, placeholder); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	r.Recorder.Eventf(addr, "Normal", "Held",
		"placeholder %s re-holds %s in MetalLB", placeholder.Name, addr.Spec.Address)
	return nil
}

// finalize is teardown: delete the reservation's placeholder (releasing
// the MetalLB allocation), withdraw any pin this driver wrote, and let the
// object go.
func (r *IPAddressReconciler) finalize(ctx context.Context, addr *localv1alpha1.IPAddress) error {
	if !controllerutil.ContainsFinalizer(addr, driver.TeardownFinalizer) {
		return nil
	}
	if err := r.deletePlaceholders(ctx, addr.Name); err != nil {
		return err
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

// deletePlaceholders removes every placeholder linked to the address.
func (r *IPAddressReconciler) deletePlaceholders(ctx context.Context, addressName string) error {
	placeholders, err := r.placeholdersFor(ctx, addressName)
	if err != nil {
		return err
	}
	for i := range placeholders {
		if err := r.Delete(ctx, &placeholders[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *IPAddressReconciler) placeholdersFor(ctx context.Context, addressName string) ([]corev1.Service, error) {
	services := &corev1.ServiceList{}
	if err := r.List(ctx, services,
		client.InNamespace(r.PlaceholderNamespace),
		client.MatchingLabels{driver.PlaceholderAddressLabel: addressName}); err != nil {
		return nil, err
	}
	return services.Items, nil
}

// maybeClearConflict returns a conflicted address to core-owned
// bookkeeping once no Service wrongfully holds it. This driver's own
// placeholders are authorized holders, not offenders.
func (r *IPAddressReconciler) maybeClearConflict(ctx context.Context, addr *localv1alpha1.IPAddress) error {
	services := &corev1.ServiceList{}
	if err := r.List(ctx, services); err != nil {
		return err
	}
	for i := range services.Items {
		svc := &services.Items[i]
		if svc.Namespace == r.PlaceholderNamespace && driver.IsPlaceholder(svc) {
			continue
		}
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

// addressesForService requeues the IPAddress objects a Service event may
// affect: the ones matching its live assignments, the one it is associated
// with, and — cheaply — every conflicted address, so withdrawn wrongful
// assignments and deleted holders propagate.
func (r *IPAddressReconciler) addressesForService(ctx context.Context, o client.Object) []reconcile.Request {
	svc := o.(*corev1.Service)
	seen := map[string]bool{}
	var reqs []reconcile.Request
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
		}
	}
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if ingress.IP == "" {
			continue
		}
		addresses := &localv1alpha1.IPAddressList{}
		if err := r.List(ctx, addresses, client.MatchingFields{IPAddressByAddressIndex: ingress.IP}); err != nil {
			continue
		}
		for _, addr := range addresses.Items {
			add(addr.Name)
		}
	}
	all := &localv1alpha1.IPAddressList{}
	if err := r.List(ctx, all); err == nil {
		for _, addr := range all.Items {
			if addr.Status.Phase == localv1alpha1.IPAddressConflict {
				add(addr.Name)
				continue
			}
			if holder := addr.Status.AssociatedTo; holder != nil &&
				holder.Kind == "Service" && holder.Namespace == svc.Namespace && holder.Name == svc.Name {
				add(addr.Name)
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
