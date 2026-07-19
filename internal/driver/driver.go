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

// Package driver holds the MetalLB-specific half of the IP Allocation
// Driver: the provisioner identity, class-parameter parsing, and the
// placeholder-Service reservation mechanics. The controllers in
// internal/controller wire it to the cluster.
package driver

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	localv1alpha1 "github.com/lllamnyp/address-controller/api/v1alpha1"
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
)

// ClassParameters is this driver's interpretation of the opaque
// IPAddressClass.spec.parameters blob.
type ClassParameters struct {
	// Addresses are the CIDRs the class's MetalLB pool covers.
	Addresses []string `json:"addresses"`
}

// ParseClassParameters decodes and validates class parameters.
func ParseClassParameters(raw *runtime.RawExtension) (ClassParameters, error) {
	var params ClassParameters
	if raw == nil || len(raw.Raw) == 0 {
		return params, fmt.Errorf("class has no parameters; this driver requires an addresses list")
	}
	if err := json.Unmarshal(raw.Raw, &params); err != nil {
		return params, fmt.Errorf("parsing class parameters: %w", err)
	}
	if len(params.Addresses) == 0 {
		return params, fmt.Errorf("class parameters name no addresses")
	}
	for _, cidr := range params.Addresses {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return params, fmt.Errorf("class parameter address %q: %w", cidr, err)
		}
	}
	return params, nil
}

// PoolName is the deterministic name of the MetalLB objects rendered for a
// class.
func PoolName(className string) string {
	return "iad-" + className
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
// endpoints — never announces. pinIP empty means "allocate from the
// class's pool"; non-empty pins the placeholder to re-hold that exact
// address.
func NewPlaceholder(name, namespace, className string, family localv1alpha1.AddressFamily, pinIP string) *corev1.Service {
	ipFamily := corev1.IPv4Protocol
	if family == localv1alpha1.FamilyIPv6 {
		ipFamily = corev1.IPv6Protocol
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{PlaceholderLabel: "true"},
			Annotations: map[string]string{
				MetalLBPoolAnnotation: PoolName(className),
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
	return svc.Labels[PlaceholderLabel] == "true"
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
