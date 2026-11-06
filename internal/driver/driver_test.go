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

package driver

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	localv1alpha1 "github.com/cozystack/address-controller/api/v1alpha1"
)

func TestParseClassParameters(t *testing.T) {
	good := &runtime.RawExtension{Raw: []byte(`{"pool":"reserved"}`)}
	if params, err := ParseClassParameters(good); err != nil || params.Pool != "reserved" {
		t.Errorf("valid parameters: pool %q, err %v", params.Pool, err)
	}
	for name, raw := range map[string]*runtime.RawExtension{
		"nil":           nil,
		"empty":         {Raw: []byte(`{}`)},
		"empty pool":    {Raw: []byte(`{"pool":""}`)},
		"old addresses": {Raw: []byte(`{"addresses":["203.0.113.0/24"]}`)},
		"unknown field": {Raw: []byte(`{"pool":"reserved","range":"x"}`)},
		"non-json":      {Raw: []byte(`pool: reserved`)},
		"wrong type":    {Raw: []byte(`{"pool":7}`)},
	} {
		if _, err := ParseClassParameters(raw); err == nil {
			t.Errorf("%s parameters accepted, want error", name)
		}
	}
}

func TestPoolProblem(t *testing.T) {
	for name, tc := range map[string]struct {
		spec map[string]any
		ok   bool
	}{
		"autoAssign false":  {map[string]any{"autoAssign": false}, true},
		"autoAssign absent": {map[string]any{}, false},
		"autoAssign true":   {map[string]any{"autoAssign": true}, false},
	} {
		t.Run(name, func(t *testing.T) {
			pool := &unstructured.Unstructured{Object: map[string]any{"spec": tc.spec}}
			if problem := PoolProblem(pool); (problem == "") != tc.ok {
				t.Errorf("PoolProblem = %q, want usable=%v", problem, tc.ok)
			}
		})
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
	svc := NewPlaceholder("iad-uid-v4", "iad-ns", "reserved", "", localv1alpha1.FamilyIPv4, "")
	if svc.Annotations[MetalLBPoolAnnotation] != "reserved" {
		t.Errorf("pool annotation = %q", svc.Annotations[MetalLBPoolAnnotation])
	}
	if _, pinned := svc.Annotations[MetalLBPinAnnotation]; pinned {
		t.Error("allocation placeholder must not be pinned; MetalLB chooses the address")
	}
	if !IsPlaceholder(svc) {
		t.Error("placeholder label missing")
	}
	if svc.Annotations[localv1alpha1.ServiceAutoClaimAnnotation] != "false" {
		t.Error("placeholder not opted out of the core's auto-claim")
	}
	if svc.Spec.Selector != nil {
		t.Error("placeholder must be selectorless so it has no endpoints and is never announced")
	}
	if svc.Spec.LoadBalancerClass != nil {
		t.Errorf("loadBalancerClass = %q, want none for the default implementation", *svc.Spec.LoadBalancerClass)
	}
	if svc.Spec.AllocateLoadBalancerNodePorts == nil || *svc.Spec.AllocateLoadBalancerNodePorts {
		t.Error("placeholder must not consume node ports")
	}
	if len(svc.Spec.IPFamilies) != 1 || svc.Spec.IPFamilies[0] != corev1.IPv4Protocol {
		t.Errorf("ipFamilies = %v, want [IPv4]", svc.Spec.IPFamilies)
	}
}

func TestNewPlaceholderForHold(t *testing.T) {
	svc := NewPlaceholder("iad-ip-2001-db8--7", "iad-ns", "", "metallb.io/reserved", localv1alpha1.FamilyIPv6, "2001:db8::7")
	if svc.Annotations[MetalLBPinAnnotation] != "2001:db8::7" {
		t.Errorf("pin annotation = %q, want the held address", svc.Annotations[MetalLBPinAnnotation])
	}
	if svc.Labels[PlaceholderAddressLabel] != "ip-2001-db8--7" {
		t.Errorf("address label = %q", svc.Labels[PlaceholderAddressLabel])
	}
	if len(svc.Spec.IPFamilies) != 1 || svc.Spec.IPFamilies[0] != corev1.IPv6Protocol {
		t.Errorf("ipFamilies = %v, want [IPv6]", svc.Spec.IPFamilies)
	}
	if _, named := svc.Annotations[MetalLBPoolAnnotation]; named {
		t.Error("pool annotation set although no pool was given")
	}
	if svc.Spec.LoadBalancerClass == nil || *svc.Spec.LoadBalancerClass != "metallb.io/reserved" {
		t.Errorf("loadBalancerClass = %v, want the driver's", svc.Spec.LoadBalancerClass)
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
