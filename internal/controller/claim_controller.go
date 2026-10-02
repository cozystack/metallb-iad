/*
Copyright 2026 Timofei Larkin

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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	localv1alpha1 "github.com/lllamnyp/address-controller/api/v1alpha1"
	"github.com/lllamnyp/metallb-iad/internal/driver"
)

// ClaimReconciler is the provisioning half of the driver: for each family a
// stamped claim misses, it reserves an address by creating a placeholder
// Service (allocation delegated to MetalLB — the allocator of record),
// observes the assigned IP, and only then records it as a pre-bound
// IPAddress. Binding completion stays with the core controller; this
// reconciler never writes claim status.
type ClaimReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// PlaceholderNamespace is where reservation placeholders live. Tenants
	// must have no write access to it.
	PlaceholderNamespace string
}

// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddressclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddresses,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddressclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile provisions reservations for a pending claim of this driver.
func (r *ClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	claim := &localv1alpha1.IPAddressClaim{}
	if err := r.Get(ctx, req.NamespacedName, claim); err != nil {
		if apierrors.IsNotFound(err) {
			// The claim is gone; allocation placeholders that never
			// materialized an IPAddress would otherwise hold addresses
			// forever.
			return ctrl.Result{}, r.cleanupOrphanPlaceholders(ctx, req.NamespacedName)
		}
		return ctrl.Result{}, err
	}
	if !claim.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.cleanupOrphanPlaceholders(ctx, req.NamespacedName)
	}
	if claim.Annotations[localv1alpha1.ProvisionerAnnotation] != driver.ProvisionerName {
		return ctrl.Result{}, nil
	}
	// A claim pinned to a specific pre-provisioned address is the core
	// controller's to match, not ours to provision for.
	if claim.Spec.AddressName != "" {
		return ctrl.Result{}, nil
	}

	className := claim.Status.ClassName
	if className == "" {
		className = claim.Spec.ClassName
	}
	if className == "" {
		return ctrl.Result{}, nil
	}
	class := &localv1alpha1.IPAddressClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: className}, class); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if class.Spec.Provisioner != driver.ProvisionerName {
		return ctrl.Result{}, nil
	}

	missing, err := r.missingFamilies(ctx, claim)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(missing) == 0 {
		// Fully provisioned (or satisfied by core matching); allocation
		// placeholders that never got as far as an IPAddress are litter.
		return ctrl.Result{}, r.cleanupOrphanPlaceholders(ctx, req.NamespacedName)
	}

	waiting := false
	for _, family := range missing {
		done, err := r.provisionFamily(ctx, claim, class, family)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !done {
			waiting = true
		}
	}
	if waiting {
		// MetalLB allocates asynchronously; the Service watch retriggers on
		// assignment and this timer is only a safety net.
		return ctrl.Result{RequeueAfter: 2 * time.Minute}, nil
	}
	logger.V(1).Info("provisioned all families", "claim", req.NamespacedName)
	return ctrl.Result{}, nil
}

// missingFamilies reports the requested families no live bound address
// covers.
func (r *ClaimReconciler) missingFamilies(ctx context.Context, claim *localv1alpha1.IPAddressClaim) ([]localv1alpha1.AddressFamily, error) {
	addresses := &localv1alpha1.IPAddressList{}
	if err := r.List(ctx, addresses); err != nil {
		return nil, err
	}
	bound := map[localv1alpha1.AddressFamily]bool{}
	for _, addr := range addresses.Items {
		if ref := addr.Spec.ClaimRef; ref != nil &&
			ref.Namespace == claim.Namespace && ref.Name == claim.Name &&
			(ref.UID == "" || ref.UID == claim.UID) &&
			addr.DeletionTimestamp.IsZero() {
			bound[driver.FamilyOf(addr.Spec.Address)] = true
		}
	}
	var missing []localv1alpha1.AddressFamily
	for _, family := range driver.RequestedFamilies(claim) {
		if !bound[family] {
			missing = append(missing, family)
		}
	}
	return missing, nil
}

// provisionFamily drives one family through reserve → observe → record.
// It returns done=false while the reservation waits on MetalLB.
func (r *ClaimReconciler) provisionFamily(ctx context.Context, claim *localv1alpha1.IPAddressClaim, class *localv1alpha1.IPAddressClass, family localv1alpha1.AddressFamily) (bool, error) {
	name := driver.AllocationPlaceholderName(claim.UID, family)
	placeholder := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Namespace: r.PlaceholderNamespace, Name: name}, placeholder)
	if apierrors.IsNotFound(err) {
		placeholder = driver.NewPlaceholder(name, r.PlaceholderNamespace, class.Name, family, "")
		placeholder.Annotations[driver.PlaceholderClaimNamespaceAnnotation] = claim.Namespace
		placeholder.Annotations[driver.PlaceholderClaimNameAnnotation] = claim.Name
		if err := r.Create(ctx, placeholder); err != nil && !apierrors.IsAlreadyExists(err) {
			return false, err
		}
		r.Recorder.Eventf(claim, "Normal", "Reserving",
			"created placeholder %s; waiting for MetalLB to allocate a %s address", name, family)
		return false, nil
	}
	if err != nil {
		return false, err
	}

	ip := observedIP(placeholder)
	if ip == "" {
		return false, nil // MetalLB has not assigned yet; the watch retriggers.
	}

	// Record in three idempotent steps: link + pin the placeholder, create
	// the ledger entry, then hand the placeholder's lifecycle to it. A
	// crash between any two steps re-converges on the next pass.
	addrName := driver.AddressObjectName(ip)
	if placeholder.Labels[driver.PlaceholderAddressLabel] != addrName ||
		placeholder.Annotations[driver.MetalLBPinAnnotation] != ip {
		placeholder.Labels[driver.PlaceholderAddressLabel] = addrName
		// Pin the placeholder to its observed address so a MetalLB restart
		// can never reshuffle the reservation.
		placeholder.Annotations[driver.MetalLBPinAnnotation] = ip
		if err := r.Update(ctx, placeholder); err != nil {
			return false, err
		}
	}

	addr := &localv1alpha1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{
			Name:       addrName,
			Finalizers: []string{driver.TeardownFinalizer},
		},
		Spec: localv1alpha1.IPAddressSpec{
			ClassName:     class.Name,
			Address:       ip,
			ReclaimPolicy: class.Spec.ReclaimPolicy,
			ClaimRef: &localv1alpha1.ClaimReference{
				Namespace: claim.Namespace,
				Name:      claim.Name,
				UID:       claim.UID,
			},
			Source: localv1alpha1.IPAddressSource{FromClass: &localv1alpha1.FromClassSource{}},
		},
	}
	if err := r.Create(ctx, addr); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return false, err
		}
		if err := r.Get(ctx, types.NamespacedName{Name: addrName}, addr); err != nil {
			return false, err
		}
		if ref := addr.Spec.ClaimRef; ref == nil || ref.Namespace != claim.Namespace || ref.Name != claim.Name {
			// The ledger already tracks this IP for someone else, yet
			// MetalLB handed it to our placeholder — the ledger and MetalLB
			// disagree. Surface it; do not fight over it.
			r.Recorder.Eventf(claim, "Warning", "LedgerMismatch",
				"MetalLB allocated %s but IPAddress %s is not bound to this claim", ip, addrName)
			return true, nil
		}
	} else {
		r.Recorder.Eventf(claim, "Normal", "Provisioned", "created IPAddress %s (%s)", addrName, ip)
	}

	if err := controllerutil.SetOwnerReference(addr, placeholder, r.Scheme); err != nil {
		return false, err
	}
	return true, r.Update(ctx, placeholder)
}

// observedIP extracts the assigned load-balancer IP of a placeholder.
func observedIP(svc *corev1.Service) string {
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if ingress.IP != "" {
			return ingress.IP
		}
	}
	return ""
}

// cleanupOrphanPlaceholders deletes this claim's allocation placeholders
// that never materialized an IPAddress. Placeholders backing an existing
// IPAddress are left alone — their lifecycle belongs to the address, and
// under Retain they keep holding the reservation after the claim is gone.
func (r *ClaimReconciler) cleanupOrphanPlaceholders(ctx context.Context, claimKey types.NamespacedName) error {
	placeholders, err := r.claimPlaceholders(ctx, claimKey)
	if err != nil {
		return err
	}
	for i := range placeholders {
		placeholder := &placeholders[i]
		if name := placeholder.Labels[driver.PlaceholderAddressLabel]; name != "" {
			err := r.Get(ctx, types.NamespacedName{Name: name}, &localv1alpha1.IPAddress{})
			if err == nil {
				continue // backed by a ledger entry; not an orphan
			}
			if !apierrors.IsNotFound(err) {
				return err
			}
		}
		if err := r.Delete(ctx, placeholder); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *ClaimReconciler) claimPlaceholders(ctx context.Context, claimKey types.NamespacedName) ([]corev1.Service, error) {
	services := &corev1.ServiceList{}
	if err := r.List(ctx, services,
		client.InNamespace(r.PlaceholderNamespace),
		client.MatchingLabels{driver.PlaceholderLabel: "true"}); err != nil {
		return nil, err
	}
	var matched []corev1.Service
	for _, svc := range services.Items {
		if svc.Annotations[driver.PlaceholderClaimNamespaceAnnotation] == claimKey.Namespace &&
			svc.Annotations[driver.PlaceholderClaimNameAnnotation] == claimKey.Name {
			matched = append(matched, svc)
		}
	}
	return matched, nil
}

// claimForPlaceholder maps a placeholder event to the claim it allocates
// for.
func (r *ClaimReconciler) claimForPlaceholder(_ context.Context, o client.Object) []reconcile.Request {
	svc := o.(*corev1.Service)
	if svc.Namespace != r.PlaceholderNamespace || !driver.IsPlaceholder(svc) {
		return nil
	}
	namespace := svc.Annotations[driver.PlaceholderClaimNamespaceAnnotation]
	name := svc.Annotations[driver.PlaceholderClaimNameAnnotation]
	if namespace == "" || name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}}
}

// pendingClaimsOfOurs requeues every not-yet-bound claim stamped with this
// driver's name — used when an IPAddress deletion may have freed capacity.
func (r *ClaimReconciler) pendingClaimsOfOurs(ctx context.Context, _ client.Object) []reconcile.Request {
	claims := &localv1alpha1.IPAddressClaimList{}
	if err := r.List(ctx, claims); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, c := range claims.Items {
		if c.Annotations[localv1alpha1.ProvisionerAnnotation] != driver.ProvisionerName {
			continue
		}
		if c.Status.Phase != localv1alpha1.ClaimBound {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: c.Namespace, Name: c.Name},
			})
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&localv1alpha1.IPAddressClaim{}).
		Watches(&localv1alpha1.IPAddress{}, handler.EnqueueRequestsFromMapFunc(r.pendingClaimsOfOurs)).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(r.claimForPlaceholder)).
		Named("iad-claim").
		Complete(r)
}
