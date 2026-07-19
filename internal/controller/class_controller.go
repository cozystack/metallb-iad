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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	localv1alpha1 "github.com/lllamnyp/address-controller/api/v1alpha1"
	"github.com/lllamnyp/metallb-iad/internal/driver"
)

var (
	ipAddressPoolGVK = schema.GroupVersionKind{
		Group: "metallb.io", Version: "v1beta1", Kind: "IPAddressPool",
	}
	l2AdvertisementGVK = schema.GroupVersionKind{
		Group: "metallb.io", Version: "v1beta1", Kind: "L2Advertisement",
	}
)

// ClassReconciler renders the MetalLB side of an IPAddressClass served by
// this driver: one IPAddressPool with autoAssign: false covering the
// class's ranges, and one L2Advertisement selecting it. Per the design,
// that is the entire per-cluster MetalLB configuration — no pool per
// tenant, no /32s. The objects are owned by the class, so deleting the
// class garbage-collects them.
type ClassReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// MetalLBNamespace is where MetalLB expects its configuration
	// resources, normally "metallb-system".
	MetalLBNamespace string
}

// +kubebuilder:rbac:groups=local.sdn.cozystack.io,resources=ipaddressclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=metallb.io,resources=ipaddresspools;l2advertisements,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile renders pool and advertisement for one class.
func (r *ClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	class := &localv1alpha1.IPAddressClass{}
	if err := r.Get(ctx, req.NamespacedName, class); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Deletion is handled by garbage collection through owner references.
	if !class.DeletionTimestamp.IsZero() || class.Spec.Provisioner != driver.ProvisionerName {
		return ctrl.Result{}, nil
	}

	params, err := driver.ParseClassParameters(class.Spec.Parameters)
	if err != nil {
		r.Recorder.Eventf(class, "Warning", "InvalidClassParameters", "%v", err)
		return ctrl.Result{}, nil
	}

	poolName := "iad-" + class.Name

	pool := &unstructured.Unstructured{}
	pool.SetGroupVersionKind(ipAddressPoolGVK)
	pool.SetNamespace(r.MetalLBNamespace)
	pool.SetName(poolName)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, pool, func() error {
		addresses := make([]any, 0, len(params.Addresses))
		for _, cidr := range params.Addresses {
			addresses = append(addresses, cidr)
		}
		if err := unstructured.SetNestedSlice(pool.Object, addresses, "spec", "addresses"); err != nil {
			return err
		}
		// autoAssign: false keeps MetalLB from handing reserved addresses
		// to plain Services automatically; only explicit pins draw from it.
		if err := unstructured.SetNestedField(pool.Object, false, "spec", "autoAssign"); err != nil {
			return err
		}
		return controllerutil.SetOwnerReference(class, pool, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, err
	}

	adv := &unstructured.Unstructured{}
	adv.SetGroupVersionKind(l2AdvertisementGVK)
	adv.SetNamespace(r.MetalLBNamespace)
	adv.SetName(poolName)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, adv, func() error {
		if err := unstructured.SetNestedSlice(adv.Object, []any{poolName}, "spec", "ipAddressPools"); err != nil {
			return err
		}
		return controllerutil.SetOwnerReference(class, adv, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, err
	}

	logger.V(1).Info("rendered MetalLB config", "pool", poolName)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&localv1alpha1.IPAddressClass{}).
		Named("iad-class").
		Complete(r)
}
