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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	localv1alpha1 "github.com/lllamnyp/address-controller/api/v1alpha1"
	"github.com/lllamnyp/metallb-iad/internal/driver"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := localv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	// The MetalLB kinds are only ever handled as unstructured objects.
	scheme.AddKnownTypeWithName(ipAddressPoolGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(ipAddressPoolGVK.GroupVersion().WithKind("IPAddressPoolList"), &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(l2AdvertisementGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(l2AdvertisementGVK.GroupVersion().WithKind("L2AdvertisementList"), &unstructured.UnstructuredList{})
	return scheme
}

func testClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&localv1alpha1.IPAddress{}, &localv1alpha1.IPAddressClaim{}).
		WithIndex(&localv1alpha1.IPAddress{}, IPAddressByAddressIndex, func(o client.Object) []string {
			addr := o.(*localv1alpha1.IPAddress)
			if addr.Spec.Address == "" {
				return nil
			}
			return []string{addr.Spec.Address}
		}).
		WithObjects(objs...).
		Build()
}

func iadClass() *localv1alpha1.IPAddressClass {
	return &localv1alpha1.IPAddressClass{
		ObjectMeta: metav1.ObjectMeta{Name: "public"},
		Spec: localv1alpha1.IPAddressClassSpec{
			Provisioner:   driver.ProvisionerName,
			ReclaimPolicy: localv1alpha1.ReclaimRetain,
			Parameters:    &runtime.RawExtension{Raw: []byte(`{"addresses":["203.0.113.0/29","2001:db8::/126"]}`)},
		},
	}
}

func stampedClaim(family localv1alpha1.AddressFamily) *localv1alpha1.IPAddressClaim {
	return &localv1alpha1.IPAddressClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "tenant-a",
			UID:       "claim-uid-1",
			Annotations: map[string]string{
				localv1alpha1.ProvisionerAnnotation: driver.ProvisionerName,
			},
		},
		Spec:   localv1alpha1.IPAddressClaimSpec{ClassName: "public", Family: family},
		Status: localv1alpha1.IPAddressClaimStatus{Phase: localv1alpha1.ClaimPending, ClassName: "public"},
	}
}

func reconcileOnce(t *testing.T, r interface {
	Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
}, namespace, name string) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: namespace, Name: name},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func listAddresses(t *testing.T, c client.Client) []localv1alpha1.IPAddress {
	t.Helper()
	list := &localv1alpha1.IPAddressList{}
	if err := c.List(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func TestProvisionsAddressForPendingClaim(t *testing.T) {
	c := testClient(t, iadClass(), stampedClaim(localv1alpha1.FamilyIPv4))
	r := &ClaimReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(100)}
	reconcileOnce(t, r, "tenant-a", "web")

	addrs := listAddresses(t, c)
	if len(addrs) != 1 {
		t.Fatalf("got %d addresses, want 1", len(addrs))
	}
	addr := addrs[0]
	if addr.Spec.Address != "203.0.113.1" {
		t.Errorf("address = %q, want 203.0.113.1", addr.Spec.Address)
	}
	if addr.Spec.ClaimRef == nil || addr.Spec.ClaimRef.UID != "claim-uid-1" {
		t.Errorf("claimRef = %+v, want pre-bound with the claim UID", addr.Spec.ClaimRef)
	}
	if addr.Spec.ReclaimPolicy != localv1alpha1.ReclaimRetain {
		t.Errorf("reclaimPolicy = %q, want copied Retain", addr.Spec.ReclaimPolicy)
	}
	if addr.Spec.Source.FromClass == nil {
		t.Error("source.fromClass unset; this driver allocates, not adopts")
	}
	if len(addr.Finalizers) != 1 || addr.Finalizers[0] != driver.TeardownFinalizer {
		t.Errorf("finalizers = %v, want the teardown finalizer", addr.Finalizers)
	}
}

func TestProvisionsBothFamiliesForDualClaim(t *testing.T) {
	c := testClient(t, iadClass(), stampedClaim(localv1alpha1.FamilyDual))
	r := &ClaimReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(100)}
	reconcileOnce(t, r, "tenant-a", "web")

	addrs := listAddresses(t, c)
	if len(addrs) != 2 {
		t.Fatalf("got %d addresses, want one per family", len(addrs))
	}
	families := map[localv1alpha1.AddressFamily]bool{}
	for _, addr := range addrs {
		families[driver.FamilyOf(addr.Spec.Address)] = true
	}
	if !families[localv1alpha1.FamilyIPv4] || !families[localv1alpha1.FamilyIPv6] {
		t.Errorf("families = %v, want IPv4 and IPv6", families)
	}
}

func TestDoesNotDoubleProvision(t *testing.T) {
	c := testClient(t, iadClass(), stampedClaim(localv1alpha1.FamilyIPv4))
	r := &ClaimReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(100)}
	reconcileOnce(t, r, "tenant-a", "web")
	reconcileOnce(t, r, "tenant-a", "web")

	if addrs := listAddresses(t, c); len(addrs) != 1 {
		t.Errorf("got %d addresses after two reconciles, want 1", len(addrs))
	}
}

func TestIgnoresForeignClaims(t *testing.T) {
	claim := stampedClaim(localv1alpha1.FamilyIPv4)
	claim.Annotations[localv1alpha1.ProvisionerAnnotation] = "someone.else.example.com"
	c := testClient(t, iadClass(), claim)
	r := &ClaimReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(100)}
	reconcileOnce(t, r, "tenant-a", "web")

	if addrs := listAddresses(t, c); len(addrs) != 0 {
		t.Errorf("provisioned %d addresses for another driver's claim", len(addrs))
	}
}

func TestSkipsInUseAddresses(t *testing.T) {
	taken := &localv1alpha1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Name: "ip-203-0-113-1"},
		Spec: localv1alpha1.IPAddressSpec{
			ClassName: "public",
			Address:   "203.0.113.1",
			ClaimRef:  &localv1alpha1.ClaimReference{Namespace: "tenant-b", Name: "other", UID: "other-uid"},
			Source:    localv1alpha1.IPAddressSource{FromClass: &localv1alpha1.FromClassSource{}},
		},
	}
	c := testClient(t, iadClass(), stampedClaim(localv1alpha1.FamilyIPv4), taken)
	r := &ClaimReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(100)}
	reconcileOnce(t, r, "tenant-a", "web")

	for _, addr := range listAddresses(t, c) {
		if addr.Name == "ip-203-0-113-1" {
			continue
		}
		if addr.Spec.Address != "203.0.113.2" {
			t.Errorf("allocated %q, want 203.0.113.2 (next free)", addr.Spec.Address)
		}
	}
}
