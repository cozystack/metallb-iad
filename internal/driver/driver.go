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
// address allocator. The controllers in internal/controller wire it to the
// cluster.
package driver

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"

	localv1alpha1 "github.com/lllamnyp/address-controller/api/v1alpha1"
)

const (
	// ProvisionerName is what an IPAddressClass's spec.provisioner must be
	// for this driver to serve it.
	ProvisionerName = "metallb.drivers.local.sdn.cozystack.io"

	// TeardownFinalizer is placed on every IPAddress this driver creates.
	// MetalLB has no per-address backend object to deallocate, so teardown
	// only withdraws a live pin, but the finalizer keeps the contract shape
	// every driver must have.
	TeardownFinalizer = "metallb.drivers.local.sdn.cozystack.io/teardown"

	// PinnedAnnotation marks a Service whose MetalLB pin annotation was
	// written by this driver, so the driver only ever removes pins it owns.
	PinnedAnnotation = "metallb.drivers.local.sdn.cozystack.io/pinned"

	// MetalLBPinAnnotation is MetalLB's "use exactly these addresses" hook.
	MetalLBPinAnnotation = "metallb.io/loadBalancerIPs"
)

// ClassParameters is this driver's interpretation of the opaque
// IPAddressClass.spec.parameters blob.
type ClassParameters struct {
	// Addresses are the CIDRs the class carves addresses from.
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

// Allocate carves the first free address of the wanted family from the
// class's ranges. IPv4 network and broadcast addresses are never handed
// out. inUse holds every address already backed by an IPAddress object,
// regardless of class — one IP must never be represented twice.
func Allocate(params ClassParameters, family localv1alpha1.AddressFamily, inUse map[netip.Addr]bool) (netip.Addr, error) {
	for _, cidr := range params.Addresses {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("class parameter address %q: %w", cidr, err)
		}
		prefix = prefix.Masked()
		if prefixFamily(prefix) != family {
			continue
		}
		for addr := prefix.Addr(); prefix.Contains(addr); addr = addr.Next() {
			if isReservedV4(prefix, addr) || inUse[addr] {
				continue
			}
			return addr, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("no free %s address in class ranges %v", family, params.Addresses)
}

func prefixFamily(prefix netip.Prefix) localv1alpha1.AddressFamily {
	if prefix.Addr().Is4() || prefix.Addr().Is4In6() {
		return localv1alpha1.FamilyIPv4
	}
	return localv1alpha1.FamilyIPv6
}

// isReservedV4 reports whether addr is the network or broadcast address of
// an IPv4 prefix wider than /31.
func isReservedV4(prefix netip.Prefix, addr netip.Addr) bool {
	if !addr.Is4() || prefix.Bits() >= 31 {
		return false
	}
	if addr == prefix.Addr() {
		return true
	}
	return !prefix.Contains(addr.Next())
}

// AddressObjectName derives the conventional IPAddress object name for an
// IP, e.g. "ip-203-0-113-7" or "ip-2001-db8--7".
func AddressObjectName(address string) string {
	name := strings.NewReplacer(".", "-", ":", "-").Replace(address)
	return "ip-" + name
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
