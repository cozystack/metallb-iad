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
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/cozystack/metallb-iad/internal/driver"
	localv1alpha1 "github.com/lllamnyp/address-controller/api/v1alpha1"
)

// ServiceReconciler is the association half of the driver — the separate,
// reversible act of attaching a reserved address to a workload. A tenant
// names an IPAddressClaim in the Service's own namespace via the
// local.sdn.cozystack.io/ip-address-claim annotation; this reconciler
// translates it into MetalLB's raw pin annotation, maintains
// IPAddress.status.associatedTo, and enforces that a claim serves one
// Service at a time. It also reconciles live Service assignments against
// the IPAddress ledger and surfaces collisions as phase Conflict — the
// detection layer for what admission cannot prevent.
type ServiceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// PlaceholderNamespace is where reservation placeholders live.
	PlaceholderNamespace string
}

// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddressclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddresses,verbs=get;list;watch
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddresses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives one Service's association state.
func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	svc := &corev1.Service{}
	if err := r.Get(ctx, req.NamespacedName, svc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// The driver's own reservation placeholders are not workloads; the
	// claim and address controllers manage them.
	if svc.Namespace == r.PlaceholderNamespace && driver.IsPlaceholder(svc) {
		return ctrl.Result{}, nil
	}

	if !svc.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.withdrawAssociations(ctx, svc)
	}

	claimName := svc.Annotations[localv1alpha1.ServiceClaimAnnotation]
	if claimName == "" {
		// No claim named (anymore): withdraw any pin this driver wrote.
		// The address stays Bound to its claim — reserved, attached to
		// nothing.
		if err := r.unpin(ctx, svc); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.detectConflicts(ctx, svc)
	}

	if err := r.associate(ctx, svc, claimName); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.detectConflicts(ctx, svc)
}

// associate resolves claim → addresses and writes the MetalLB pin.
func (r *ServiceReconciler) associate(ctx context.Context, svc *corev1.Service, claimName string) error {
	logger := log.FromContext(ctx)

	// The annotation names a claim in the Service's own namespace, by
	// construction: cross-namespace address sharing is not a thing.
	claim := &localv1alpha1.IPAddressClaim{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: claimName}, claim); err != nil {
		if client.IgnoreNotFound(err) == nil {
			r.Recorder.Eventf(svc, "Warning", "ClaimNotFound",
				"IPAddressClaim %q not found in namespace %s", claimName, svc.Namespace)
			return nil
		}
		return err
	}
	if claim.Annotations[localv1alpha1.ProvisionerAnnotation] != driver.ProvisionerName {
		// Another driver's claim; not ours to act on.
		return nil
	}
	if claim.Status.Phase != localv1alpha1.ClaimBound {
		r.Recorder.Eventf(svc, "Normal", "ClaimNotBound",
			"IPAddressClaim %q is %s; waiting for it to bind", claimName, claim.Status.Phase)
		return nil
	}

	// One claim serves one Service: a second Service referencing the claim
	// is rejected, loudly, rather than silently stealing the address.
	var ips, addrNames []string
	for _, bound := range claim.Status.Addresses {
		addr := &localv1alpha1.IPAddress{}
		if err := r.Get(ctx, types.NamespacedName{Name: bound.Name}, addr); err != nil {
			return client.IgnoreNotFound(err)
		}
		if holder := addr.Status.AssociatedTo; holder != nil &&
			(holder.Namespace != svc.Namespace || holder.Name != svc.Name) {
			// A live holder makes this a rejected second association. A
			// holder that no longer exists is a stale record (its deletion
			// event may have been missed) and must not wedge the cutover
			// flow — fall through and take the association over.
			live, err := r.holderExists(ctx, holder)
			if err != nil {
				return err
			}
			if live {
				r.Recorder.Eventf(svc, "Warning", "AssociationRejected",
					"IPAddressClaim %q is already associated to Service %s/%s",
					claimName, holder.Namespace, holder.Name)
				return nil
			}
		}
		ips = append(ips, bound.Address)
		addrNames = append(addrNames, bound.Name)
	}
	if len(ips) == 0 {
		return nil
	}
	sortByFamily(ips)

	// Gap handoff, in an order every step of which re-converges after a
	// crash: (1) record the association in the ledger — this is what stops
	// the address controller from re-arming the hold; (2) release the hold
	// placeholders, freeing the address in MetalLB's books; (3) pin the
	// real Service so MetalLB assigns the address to it. Between (2) and
	// (3) the address is briefly unheld — the gap — guarded only by
	// autoAssign:false, admission, and conflict detection.
	for _, name := range addrNames {
		addr := &localv1alpha1.IPAddress{}
		if err := r.Get(ctx, types.NamespacedName{Name: name}, addr); err != nil {
			return client.IgnoreNotFound(err)
		}
		desired := &localv1alpha1.AssociationReference{
			Kind: "Service", Namespace: svc.Namespace, Name: svc.Name,
		}
		if addr.Status.AssociatedTo == nil || *addr.Status.AssociatedTo != *desired {
			addr.Status.AssociatedTo = desired
			if err := r.Status().Update(ctx, addr); err != nil {
				return err
			}
		}
		if err := r.deletePlaceholders(ctx, name); err != nil {
			return err
		}
	}

	pin := strings.Join(ips, ",")
	if svc.Annotations[driver.MetalLBPinAnnotation] != pin ||
		svc.Annotations[driver.PinnedAnnotation] != "true" {
		if svc.Annotations == nil {
			svc.Annotations = map[string]string{}
		}
		svc.Annotations[driver.MetalLBPinAnnotation] = pin
		svc.Annotations[driver.PinnedAnnotation] = "true"
		if err := r.Update(ctx, svc); err != nil {
			return err
		}
		r.Recorder.Eventf(svc, "Normal", "Associated",
			"pinned %s from IPAddressClaim %q", pin, claimName)
	}
	logger.V(1).Info("associated", "service", client.ObjectKeyFromObject(svc), "ips", pin)
	return nil
}

// deletePlaceholders releases the hold placeholders linked to an address.
func (r *ServiceReconciler) deletePlaceholders(ctx context.Context, addressName string) error {
	services := &corev1.ServiceList{}
	if err := r.List(ctx, services,
		client.InNamespace(r.PlaceholderNamespace),
		client.MatchingLabels{driver.PlaceholderAddressLabel: addressName}); err != nil {
		return err
	}
	for i := range services.Items {
		if err := r.Delete(ctx, &services.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// holderExists reports whether the Service an association points at is
// still alive.
func (r *ServiceReconciler) holderExists(ctx context.Context, holder *localv1alpha1.AssociationReference) (bool, error) {
	if holder.Kind != "Service" {
		return true, nil // unknown holder kinds are treated as live, conservatively
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

// unpin removes a pin this driver wrote (never a hand-written one) and
// clears associatedTo on the addresses it pointed at.
func (r *ServiceReconciler) unpin(ctx context.Context, svc *corev1.Service) error {
	if svc.Annotations[driver.PinnedAnnotation] != "true" {
		return nil
	}
	delete(svc.Annotations, driver.MetalLBPinAnnotation)
	delete(svc.Annotations, driver.PinnedAnnotation)
	if err := r.Update(ctx, svc); err != nil {
		return err
	}
	r.Recorder.Event(svc, "Normal", "Disassociated",
		"withdrew MetalLB pin; the claim keeps its address")
	return r.withdrawAssociations(ctx, svc)
}

// withdrawAssociations clears associatedTo on every address that names this
// Service.
func (r *ServiceReconciler) withdrawAssociations(ctx context.Context, svc *corev1.Service) error {
	addresses := &localv1alpha1.IPAddressList{}
	if err := r.List(ctx, addresses); err != nil {
		return err
	}
	for i := range addresses.Items {
		addr := &addresses.Items[i]
		if holder := addr.Status.AssociatedTo; holder != nil &&
			holder.Kind == "Service" && holder.Namespace == svc.Namespace && holder.Name == svc.Name {
			addr.Status.AssociatedTo = nil
			if err := r.Status().Update(ctx, addr); err != nil {
				return err
			}
		}
	}
	return nil
}

// detectConflicts compares the Service's live assignments against the
// ledger: an assigned address that is bound to a claim which does not
// authorize this Service — by explicit pin, a race, or an impersonated
// write — drives the IPAddress to Conflict. An assignment no IPAddress
// tracks is the normal eager-allocator case and is left alone.
func (r *ServiceReconciler) detectConflicts(ctx context.Context, svc *corev1.Service) error {
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if ingress.IP == "" {
			continue
		}
		addresses := &localv1alpha1.IPAddressList{}
		if err := r.List(ctx, addresses, client.MatchingFields{IPAddressByAddressIndex: ingress.IP}); err != nil {
			return err
		}
		for i := range addresses.Items {
			addr := &addresses.Items[i]
			if addr.Spec.ClaimRef == nil || authorizes(addr, svc) {
				continue
			}
			if addr.Status.Phase != localv1alpha1.IPAddressConflict {
				addr.Status.Phase = localv1alpha1.IPAddressConflict
				if err := r.Status().Update(ctx, addr); err != nil {
					return err
				}
				r.Recorder.Eventf(svc, "Warning", "AddressConflict",
					"Service holds %s, which is reserved by IPAddressClaim %s/%s",
					ingress.IP, addr.Spec.ClaimRef.Namespace, addr.Spec.ClaimRef.Name)
				r.Recorder.Eventf(addr, "Warning", "Conflict",
					"held by Service %s/%s, which the binding does not authorize",
					svc.Namespace, svc.Name)
			}
		}
	}
	return nil
}

// authorizes reports whether the address's binding entitles this Service to
// hold it.
func authorizes(addr *localv1alpha1.IPAddress, svc *corev1.Service) bool {
	holder := addr.Status.AssociatedTo
	return holder != nil && holder.Kind == "Service" &&
		holder.Namespace == svc.Namespace && holder.Name == svc.Name
}

// sortByFamily orders IPs v4 before v6, each group lexically — the order
// MetalLB expects in a dual-stack pin.
func sortByFamily(ips []string) {
	sort.Slice(ips, func(i, j int) bool {
		fi, fj := driver.FamilyOf(ips[i]), driver.FamilyOf(ips[j])
		if fi != fj {
			return fi == localv1alpha1.FamilyIPv4
		}
		return ips[i] < ips[j]
	})
}

// servicesForAddress requeues the Services an address change may affect:
// the associated Service (re-drives a pin left unwritten by a partial
// handoff) and the Services referencing the bound claim.
func (r *ServiceReconciler) servicesForAddress(ctx context.Context, o client.Object) []reconcile.Request {
	addr := o.(*localv1alpha1.IPAddress)
	var reqs []reconcile.Request
	if holder := addr.Status.AssociatedTo; holder != nil && holder.Kind == "Service" {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: holder.Namespace, Name: holder.Name},
		})
	}
	if ref := addr.Spec.ClaimRef; ref != nil {
		services := &corev1.ServiceList{}
		if err := r.List(ctx, services, client.InNamespace(ref.Namespace)); err == nil {
			for _, svc := range services.Items {
				if svc.Annotations[localv1alpha1.ServiceClaimAnnotation] == ref.Name {
					reqs = append(reqs, reconcile.Request{
						NamespacedName: types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name},
					})
				}
			}
		}
	}
	return reqs
}

// servicesForClaim requeues the Services in the claim's namespace that
// reference it, so claim binding progress propagates to the pin.
func (r *ServiceReconciler) servicesForClaim(ctx context.Context, o client.Object) []reconcile.Request {
	services := &corev1.ServiceList{}
	if err := r.List(ctx, services, client.InNamespace(o.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, svc := range services.Items {
		if svc.Annotations[localv1alpha1.ServiceClaimAnnotation] == o.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name},
			})
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Watches(&localv1alpha1.IPAddressClaim{}, handler.EnqueueRequestsFromMapFunc(r.servicesForClaim)).
		Watches(&localv1alpha1.IPAddress{}, handler.EnqueueRequestsFromMapFunc(r.servicesForAddress)).
		Named("iad-service").
		Complete(r)
}
