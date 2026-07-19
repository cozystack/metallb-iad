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

package driver

import (
	"net/netip"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	localv1alpha1 "github.com/lllamnyp/address-controller/api/v1alpha1"
)

func params(t *testing.T, cidrs ...string) ClassParameters {
	t.Helper()
	return ClassParameters{Addresses: cidrs}
}

func TestAllocateSkipsNetworkAndBroadcast(t *testing.T) {
	got, err := Allocate(params(t, "203.0.113.0/30"), localv1alpha1.FamilyIPv4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "203.0.113.1" {
		t.Errorf("first allocation = %s, want 203.0.113.1 (network address skipped)", got)
	}
}

func TestAllocateSkipsInUse(t *testing.T) {
	inUse := map[netip.Addr]bool{netip.MustParseAddr("203.0.113.1"): true}
	got, err := Allocate(params(t, "203.0.113.0/30"), localv1alpha1.FamilyIPv4, inUse)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "203.0.113.2" {
		t.Errorf("allocation = %s, want 203.0.113.2", got)
	}
}

func TestAllocateExhaustion(t *testing.T) {
	inUse := map[netip.Addr]bool{
		netip.MustParseAddr("203.0.113.1"): true,
		netip.MustParseAddr("203.0.113.2"): true,
	}
	// /30 has exactly two usable hosts; both are taken.
	if _, err := Allocate(params(t, "203.0.113.0/30"), localv1alpha1.FamilyIPv4, inUse); err == nil {
		t.Error("expected exhaustion error, got success")
	}
}

func TestAllocatePicksMatchingFamily(t *testing.T) {
	p := params(t, "203.0.113.0/30", "2001:db8::/126")
	got, err := Allocate(p, localv1alpha1.FamilyIPv6, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Is6() || got.Is4In6() {
		t.Errorf("allocation = %s, want an IPv6 address", got)
	}
}

func TestAllocateSlash32(t *testing.T) {
	got, err := Allocate(params(t, "203.0.113.9/32"), localv1alpha1.FamilyIPv4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "203.0.113.9" {
		t.Errorf("allocation = %s, want the /32 itself", got)
	}
}

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
