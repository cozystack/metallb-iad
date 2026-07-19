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
	"net/netip"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	localv1alpha1 "github.com/lllamnyp/address-controller/api/v1alpha1"
	"github.com/lllamnyp/metallb-iad/internal/driver"
)

// ClaimReconciler is the provisioning half of the driver: it watches
// IPAddressClaims stamped with this driver's provisioner name and creates
// IPAddress objects for the families the core controller reports missing.
// The core controller completes the binding; this reconciler never touches
// claim status.
type ClaimReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddressclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddresses,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddressclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile provisions addresses for a pending claim of this driver.
func (r *ClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	claim := &localv1alpha1.IPAddressClaim{}
	if err := r.Get(ctx, req.NamespacedName, claim); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !claim.DeletionTimestamp.IsZero() ||
		claim.Annotations[localv1alpha1.ProvisionerAnnotation] != driver.ProvisionerName {
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

	addresses := &localv1alpha1.IPAddressList{}
	if err := r.List(ctx, addresses); err != nil {
		return ctrl.Result{}, err
	}
	inUse := map[netip.Addr]bool{}
	var boundFamilies []localv1alpha1.AddressFamily
	for _, addr := range addresses.Items {
		if ip, err := netip.ParseAddr(addr.Spec.Address); err == nil {
			inUse[ip] = true
		}
		if ref := addr.Spec.ClaimRef; ref != nil &&
			ref.Namespace == claim.Namespace && ref.Name == claim.Name &&
			(ref.UID == "" || ref.UID == claim.UID) &&
			addr.DeletionTimestamp.IsZero() {
			boundFamilies = append(boundFamilies, driver.FamilyOf(addr.Spec.Address))
		}
	}

	params, err := driver.ParseClassParameters(class.Spec.Parameters)
	if err != nil {
		r.Recorder.Eventf(claim, "Warning", "InvalidClassParameters",
			"IPAddressClass %s: %v", class.Name, err)
		return ctrl.Result{}, nil
	}

	for _, family := range missingFamilies(claim, boundFamilies) {
		ip, err := driver.Allocate(params, family, inUse)
		if err != nil {
			r.Recorder.Eventf(claim, "Warning", "AllocationFailed", "%v", err)
			// Exhaustion clears when an address is deleted; the IPAddress
			// watch retriggers then, and the timer is a safety net.
			return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
		}
		if err := r.createAddress(ctx, claim, class, ip); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Lost a race for this IP; retry against fresh state.
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, err
		}
		inUse[ip] = true
		logger.Info("provisioned address", "claim", req.NamespacedName, "address", ip.String())
	}
	return ctrl.Result{}, nil
}

func (r *ClaimReconciler) createAddress(ctx context.Context, claim *localv1alpha1.IPAddressClaim, class *localv1alpha1.IPAddressClass, ip netip.Addr) error {
	addr := &localv1alpha1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{
			Name:       driver.AddressObjectName(ip.String()),
			Finalizers: []string{driver.TeardownFinalizer},
		},
		Spec: localv1alpha1.IPAddressSpec{
			ClassName:     class.Name,
			Address:       ip.String(),
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
		return err
	}
	r.Recorder.Eventf(claim, "Normal", "Provisioned", "created IPAddress %s (%s)", addr.Name, ip)
	return nil
}

func missingFamilies(claim *localv1alpha1.IPAddressClaim, bound []localv1alpha1.AddressFamily) []localv1alpha1.AddressFamily {
	var missing []localv1alpha1.AddressFamily
	for _, family := range driver.RequestedFamilies(claim) {
		satisfied := false
		for _, b := range bound {
			if b == family {
				satisfied = true
				break
			}
		}
		if !satisfied {
			missing = append(missing, family)
		}
	}
	return missing
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
		Named("iad-claim").
		Complete(r)
}
