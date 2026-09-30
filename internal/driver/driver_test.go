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

package driver

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	localv1alpha1 "github.com/lllamnyp/address-controller/api/v1alpha1"
)

func TestParseClassParameters(t *testing.T) {
	good := &runtime.RawExtension{Raw: []byte(`{"addresses":["203.0.113.0/24"]}`)}
	if _, err := ParseClassParameters(good); err != nil {
		t.Errorf("valid parameters rejected: %v", err)
	}
	for name, raw := range map[string]*runtime.RawExtension{
		"nil":       nil,
		"empty":     {Raw: []byte(`{}`)},
		"not-cidr":  {Raw: []byte(`{"addresses":["not-a-cidr"]}`)},
		"bare-ip":   {Raw: []byte(`{"addresses":["203.0.113.7"]}`)},
		"non-json":  {Raw: []byte(`addresses: [x]`)},
		"wrongtype": {Raw: []byte(`{"addresses":"203.0.113.0/24"}`)},
	} {
		if _, err := ParseClassParameters(raw); err == nil {
			t.Errorf("%s parameters accepted, want error", name)
		}
	}
}

func TestAddressObjectName(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.7": "ip-203-0-113-7",
		"2001:db8::7": "ip-2001-db8--7",
	} {
		if got := AddressObjectName(in); got != want {
			t.Errorf("AddressObjectName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewPlaceholderForAllocation(t *testing.T) {
	svc := NewPlaceholder("iad-uid-v4", "iad-ns", "public", localv1alpha1.FamilyIPv4, "")
	if svc.Annotations[MetalLBPoolAnnotation] != "iad-public" {
		t.Errorf("pool annotation = %q", svc.Annotations[MetalLBPoolAnnotation])
	}
	if _, pinned := svc.Annotations[MetalLBPinAnnotation]; pinned {
		t.Error("allocation placeholder must not be pinned; MetalLB chooses the address")
	}
	if !IsPlaceholder(svc) {
		t.Error("placeholder label missing")
	}
	if svc.Spec.Selector != nil {
		t.Error("placeholder must be selectorless so it has no endpoints and is never announced")
	}
	if svc.Spec.AllocateLoadBalancerNodePorts == nil || *svc.Spec.AllocateLoadBalancerNodePorts {
		t.Error("placeholder must not consume node ports")
	}
	if len(svc.Spec.IPFamilies) != 1 || svc.Spec.IPFamilies[0] != corev1.IPv4Protocol {
		t.Errorf("ipFamilies = %v, want [IPv4]", svc.Spec.IPFamilies)
	}
}

func TestNewPlaceholderForHold(t *testing.T) {
	svc := NewPlaceholder("iad-ip-2001-db8--7", "iad-ns", "public", localv1alpha1.FamilyIPv6, "2001:db8::7")
	if svc.Annotations[MetalLBPinAnnotation] != "2001:db8::7" {
		t.Errorf("pin annotation = %q, want the held address", svc.Annotations[MetalLBPinAnnotation])
	}
	if svc.Labels[PlaceholderAddressLabel] != "ip-2001-db8--7" {
		t.Errorf("address label = %q", svc.Labels[PlaceholderAddressLabel])
	}
	if len(svc.Spec.IPFamilies) != 1 || svc.Spec.IPFamilies[0] != corev1.IPv6Protocol {
		t.Errorf("ipFamilies = %v, want [IPv6]", svc.Spec.IPFamilies)
	}
}

func TestPlaceholderNames(t *testing.T) {
	v4 := AllocationPlaceholderName("abc-123", localv1alpha1.FamilyIPv4)
	v6 := AllocationPlaceholderName("abc-123", localv1alpha1.FamilyIPv6)
	if v4 == v6 {
		t.Error("per-family allocation placeholders must not collide")
	}
	if got := HoldPlaceholderName("ip-203-0-113-7"); got != "iad-ip-203-0-113-7" {
		t.Errorf("HoldPlaceholderName = %q", got)
	}
}
