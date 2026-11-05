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

// Package driver holds the MetalLB-specific half of the IP Allocation
// Driver: the provisioner identity, class-parameter parsing, and the
// placeholder-Service reservation mechanics. The controllers in
// internal/controller wire it to the cluster.
package driver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	localv1alpha1 "github.com/cozystack/address-controller/api/v1alpha1"
)

const (
	// ProvisionerName is what an IPAddressClass's spec.provisioner must be
	// for this driver to serve it.
	ProvisionerName = "metallb.drivers.local.sdn.cozystack.io"

	// TeardownFinalizer is placed on every IPAddress this driver creates.
	// Teardown deletes the placeholder Service holding the reservation and
	// withdraws any live pin.
	TeardownFinalizer = "metallb.drivers.local.sdn.cozystack.io/teardown"

	// PinnedAnnotation marks a Service whose MetalLB pin annotation was
	// written by this driver, so the driver only ever removes pins it owns.
	PinnedAnnotation = "metallb.drivers.local.sdn.cozystack.io/pinned"

	// MetalLBPinAnnotation is MetalLB's "use exactly these addresses" hook.
	MetalLBPinAnnotation = "metallb.io/loadBalancerIPs"

	// MetalLBPoolAnnotation names the pool a Service draws from; it is how
	// placeholders reach the reserved autoAssign:false pool.
	MetalLBPoolAnnotation = "metallb.io/address-pool"

	// PlaceholderLabel marks a Service as one of this driver's reservation
	// placeholders. Only honoured on Services in the driver's placeholder
	// namespace, which tenants cannot write to.
	PlaceholderLabel = ProvisionerName + "/placeholder"

	// PlaceholderAddressLabel links a placeholder to the IPAddress object
	// whose reservation it holds. Set once the allocation is observed.
	PlaceholderAddressLabel = ProvisionerName + "/ip-address"

	// PlaceholderClaimNamespaceAnnotation / PlaceholderClaimNameAnnotation
	// record, on an allocation placeholder, which claim it is allocating
	// for — annotations, not labels, because object names may exceed label
	// value limits.
	PlaceholderClaimNamespaceAnnotation = ProvisionerName + "/claim-namespace"
	PlaceholderClaimNameAnnotation      = ProvisionerName + "/claim-name"

	// MarkerValue is the value of PlaceholderLabel and PinnedAnnotation.
	MarkerValue = "true"

	// ServiceKind is the AssociatedTo kind of an address held by a Service.
	ServiceKind = "Service"
)

// ClassParameters is this driver's interpretation of the opaque
// IPAddressClass.spec.parameters blob. The class names MetalLB
// configuration by reference and does not restate it.
type ClassParameters struct {
	// Pool names an existing MetalLB IPAddressPool, in the MetalLB
	// namespace, that the admin owns together with its advertisements.
	Pool string `json:"pool"`
}

// ParseClassParameters decodes and validates class parameters.
func ParseClassParameters(raw *runtime.RawExtension) (ClassParameters, error) {
	var params ClassParameters
	if raw == nil || len(raw.Raw) == 0 {
		return params, fmt.Errorf("class has no parameters; this driver requires parameters.pool")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw.Raw, &fields); err != nil {
		return params, fmt.Errorf("parsing class parameters: %w", err)
	}
	if _, ok := fields["addresses"]; ok {
		return params, fmt.Errorf("parameters.addresses is no longer supported: name an existing IPAddressPool in parameters.pool")
	}
	dec := json.NewDecoder(bytes.NewReader(raw.Raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&params); err != nil {
		return params, fmt.Errorf("parsing class parameters: %w", err)
	}
	if params.Pool == "" {
		return params, fmt.Errorf("class parameters name no pool")
	}
	return params, nil
}

// PoolProblem reports why a MetalLB IPAddressPool cannot back a class, or ""
// if it can. The pool must not auto-assign, so MetalLB hands its addresses
// only to placeholders and pinned Services, and its serviceAllocation must
// admit placeholders, which live in placeholderNS. MetalLB treats a missing
// autoAssign as true.
func PoolProblem(pool *unstructured.Unstructured, placeholderNS *corev1.Namespace) string {
	autoAssign, found, err := unstructured.NestedBool(pool.Object, "spec", "autoAssign")
	if err != nil || !found || autoAssign {
		return fmt.Sprintf("IPAddressPool %s/%s auto-assigns; it must set autoAssign: false", pool.GetNamespace(), pool.GetName())
	}
	alloc, found, err := unstructured.NestedMap(pool.Object, "spec", "serviceAllocation")
	if err != nil || !found {
		return ""
	}
	if !admitsNamespace(alloc, placeholderNS) {
		return fmt.Sprintf("IPAddressPool %s/%s serviceAllocation does not admit the placeholder namespace %s",
			pool.GetNamespace(), pool.GetName(), placeholderNS.Name)
	}
	if !matchesAnySelector(alloc["serviceSelectors"], labels.Set{PlaceholderLabel: MarkerValue}) {
		return fmt.Sprintf("IPAddressPool %s/%s serviceAllocation.serviceSelectors do not match placeholder Services",
			pool.GetNamespace(), pool.GetName())
	}
	return ""
}

// admitsNamespace applies serviceAllocation's namespace rule as MetalLB
// does: with neither namespaces nor namespaceSelectors every namespace is
// admitted, otherwise the namespace must be listed or selected.
func admitsNamespace(alloc map[string]any, ns *corev1.Namespace) bool {
	names, _, _ := unstructured.NestedStringSlice(alloc, "namespaces")
	selectors, _, _ := unstructured.NestedSlice(alloc, "namespaceSelectors")
	if len(names) == 0 && len(selectors) == 0 {
		return true
	}
	return slices.Contains(names, ns.Name) ||
		(len(selectors) > 0 && matchesAnySelector(selectors, labels.Set(ns.Labels)))
}

// matchesAnySelector reports whether set matches one of the label
// selectors; an empty or absent list matches everything.
func matchesAnySelector(raw any, set labels.Set) bool {
	list, _ := raw.([]any)
	if len(list) == 0 {
		return true
	}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		var ls metav1.LabelSelector
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, &ls); err != nil {
			continue
		}
		if sel, err := metav1.LabelSelectorAsSelector(&ls); err == nil && sel.Matches(set) {
			return true
		}
	}
	return false
}

// FamilyOf reports the concrete address family of an IP, or "" if it does
// not parse.
func FamilyOf(address string) localv1alpha1.AddressFamily {
	ip, err := netip.ParseAddr(address)
	if err != nil {
		return ""
	}
	if ip.Is4() || ip.Is4In6() {
		return localv1alpha1.FamilyIPv4
	}
	return localv1alpha1.FamilyIPv6
}

// AddressObjectName derives the conventional IPAddress object name for an
// IP, e.g. "ip-203-0-113-7" or "ip-2001-db8--7".
func AddressObjectName(address string) string {
	name := strings.NewReplacer(".", "-", ":", "-").Replace(address)
	return "ip-" + name
}

// AllocationPlaceholderName is the deterministic name of the placeholder
// that allocates one family for one claim — deterministic so racing
// reconciles collide on create instead of double-allocating.
func AllocationPlaceholderName(claimUID types.UID, family localv1alpha1.AddressFamily) string {
	suffix := "v4"
	if family == localv1alpha1.FamilyIPv6 {
		suffix = "v6"
	}
	return "iad-" + string(claimUID) + "-" + suffix
}

// HoldPlaceholderName is the deterministic name of a placeholder recreated
// to re-hold an existing address after disassociation.
func HoldPlaceholderName(addressName string) string {
	return "iad-" + addressName
}

// NewPlaceholder builds a reservation placeholder: a selectorless
// LoadBalancer Service that MetalLB assigns an address to but — having no
// endpoints — never announces. It draws from pool, the class's MetalLB
// pool, and carries lbClass, the loadBalancerClass of the MetalLB instance
// this driver fronts (empty for the cluster default). pinIP empty means
// "allocate from the pool"; non-empty pins the placeholder to re-hold that
// exact address. A pinned placeholder may leave pool empty: MetalLB assigns
// a pinned address from whichever pool holds it.
func NewPlaceholder(name, namespace, pool, lbClass string, family localv1alpha1.AddressFamily, pinIP string) *corev1.Service {
	ipFamily := corev1.IPv4Protocol
	if family == localv1alpha1.FamilyIPv6 {
		ipFamily = corev1.IPv6Protocol
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{PlaceholderLabel: MarkerValue},
			Annotations: map[string]string{
				// Placeholders hold reservations; the core must not
				// auto-claim an address for them.
				localv1alpha1.ServiceAutoClaimAnnotation: "false",
			},
		},
		Spec: corev1.ServiceSpec{
			Type:                          corev1.ServiceTypeLoadBalancer,
			Ports:                         []corev1.ServicePort{{Name: "placeholder", Port: 65535, Protocol: corev1.ProtocolTCP}},
			AllocateLoadBalancerNodePorts: new(false),
			IPFamilyPolicy:                new(corev1.IPFamilyPolicySingleStack),
			IPFamilies:                    []corev1.IPFamily{ipFamily},
		},
	}
	if pool != "" {
		svc.Annotations[MetalLBPoolAnnotation] = pool
	}
	if lbClass != "" {
		svc.Spec.LoadBalancerClass = new(lbClass)
	}
	if pinIP != "" {
		svc.Annotations[MetalLBPinAnnotation] = pinIP
		svc.Labels[PlaceholderAddressLabel] = AddressObjectName(pinIP)
	}
	return svc
}

// IsPlaceholder reports whether a Service is one of this driver's
// reservation placeholders. Callers must additionally check the namespace
// against the configured placeholder namespace — the label alone is
// forgeable by anyone who can create Services.
func IsPlaceholder(svc *corev1.Service) bool {
	return svc.Labels[PlaceholderLabel] == MarkerValue
}

// RequestedFamilies expands a claim's family into the concrete families it
// needs bound.
func RequestedFamilies(claim *localv1alpha1.IPAddressClaim) []localv1alpha1.AddressFamily {
	switch claim.Spec.Family {
	case localv1alpha1.FamilyIPv6:
		return []localv1alpha1.AddressFamily{localv1alpha1.FamilyIPv6}
	case localv1alpha1.FamilyDual:
		return []localv1alpha1.AddressFamily{localv1alpha1.FamilyIPv4, localv1alpha1.FamilyIPv6}
	default:
		return []localv1alpha1.AddressFamily{localv1alpha1.FamilyIPv4}
	}
}
