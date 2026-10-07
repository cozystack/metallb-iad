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

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/cozystack/metallb-iad/internal/driver"
	localv1alpha1 "github.com/lllamnyp/address-controller/api/v1alpha1"
)

const testPlaceholderNS = "metallb-iad-system"

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
		WithStatusSubresource(&localv1alpha1.IPAddress{}, &localv1alpha1.IPAddressClaim{}, &corev1.Service{}).
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

// allocationPlaceholder builds the placeholder the claim controller would
// have created, optionally with MetalLB's assignment already observed.
func allocationPlaceholder(claim *localv1alpha1.IPAddressClaim, family localv1alpha1.AddressFamily, assignedIP string) *corev1.Service {
	svc := driver.NewPlaceholder(
		driver.AllocationPlaceholderName(claim.UID, family), testPlaceholderNS, "public", family, "")
	svc.Annotations[driver.PlaceholderClaimNamespaceAnnotation] = claim.Namespace
	svc.Annotations[driver.PlaceholderClaimNameAnnotation] = claim.Name
	if assignedIP != "" {
		svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: assignedIP}}
	}
	return svc
}

func claimRec(c client.Client) *ClaimReconciler {
	return &ClaimReconciler{
		Client:               c,
		Scheme:               c.Scheme(),
		Recorder:             record.NewFakeRecorder(100),
		PlaceholderNamespace: testPlaceholderNS,
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

func listPlaceholders(t *testing.T, c client.Client) []corev1.Service {
	t.Helper()
	list := &corev1.ServiceList{}
	if err := c.List(context.Background(), list,
		client.InNamespace(testPlaceholderNS),
		client.MatchingLabels{driver.PlaceholderLabel: "true"}); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func TestReservesViaPlaceholder(t *testing.T) {
	c := testClient(t, iadClass(), stampedClaim(localv1alpha1.FamilyIPv4))
	reconcileOnce(t, claimRec(c), "tenant-a", "web")

	placeholders := listPlaceholders(t, c)
	if len(placeholders) != 1 {
		t.Fatalf("got %d placeholders, want 1", len(placeholders))
	}
	svc := placeholders[0]
	if svc.Annotations[driver.MetalLBPoolAnnotation] != "iad-public" {
		t.Errorf("pool annotation = %q, want iad-public", svc.Annotations[driver.MetalLBPoolAnnotation])
	}
	if _, pinned := svc.Annotations[driver.MetalLBPinAnnotation]; pinned {
		t.Error("allocation placeholder pinned before MetalLB assigned anything")
	}
	if addrs := listAddresses(t, c); len(addrs) != 0 {
		t.Errorf("IPAddress created before any allocation was observed: %v", addrs)
	}
}

func TestRecordsObservedAllocation(t *testing.T) {
	claim := stampedClaim(localv1alpha1.FamilyIPv4)
	c := testClient(t, iadClass(), claim, allocationPlaceholder(claim, localv1alpha1.FamilyIPv4, "203.0.113.7"))
	reconcileOnce(t, claimRec(c), "tenant-a", "web")

	addrs := listAddresses(t, c)
	if len(addrs) != 1 {
		t.Fatalf("got %d addresses, want 1", len(addrs))
	}
	addr := addrs[0]
	if addr.Name != "ip-203-0-113-7" || addr.Spec.Address != "203.0.113.7" {
		t.Errorf("recorded %s (%s), want the observed allocation", addr.Name, addr.Spec.Address)
	}
	if addr.Spec.ClaimRef == nil || addr.Spec.ClaimRef.UID != "claim-uid-1" {
		t.Errorf("claimRef = %+v, want pre-bound with the claim UID", addr.Spec.ClaimRef)
	}
	if addr.Spec.ReclaimPolicy != localv1alpha1.ReclaimRetain {
		t.Errorf("reclaimPolicy = %q, want copied Retain", addr.Spec.ReclaimPolicy)
	}
	if addr.Spec.Source.FromClass == nil {
		t.Error("source.fromClass unset")
	}
	if len(addr.Finalizers) != 1 || addr.Finalizers[0] != driver.TeardownFinalizer {
		t.Errorf("finalizers = %v, want the teardown finalizer", addr.Finalizers)
	}

	placeholders := listPlaceholders(t, c)
	if len(placeholders) != 1 {
		t.Fatalf("placeholder count = %d", len(placeholders))
	}
	svc := placeholders[0]
	if svc.Annotations[driver.MetalLBPinAnnotation] != "203.0.113.7" {
		t.Error("placeholder not pinned to its observed address")
	}
	if svc.Labels[driver.PlaceholderAddressLabel] != "ip-203-0-113-7" {
		t.Error("placeholder not linked to the IPAddress")
	}
	if owners := svc.GetOwnerReferences(); len(owners) != 1 || owners[0].Name != "ip-203-0-113-7" {
		t.Errorf("ownerReferences = %+v, want the IPAddress", owners)
	}
}

func TestProvisionsBothFamiliesForDualClaim(t *testing.T) {
	claim := stampedClaim(localv1alpha1.FamilyDual)
	c := testClient(t, iadClass(), claim)
	r := claimRec(c)
	reconcileOnce(t, r, "tenant-a", "web")

	if placeholders := listPlaceholders(t, c); len(placeholders) != 2 {
		t.Fatalf("got %d placeholders, want one per family", len(placeholders))
	}

	// Simulate MetalLB assigning both.
	for family, ip := range map[localv1alpha1.AddressFamily]string{
		localv1alpha1.FamilyIPv4: "203.0.113.1",
		localv1alpha1.FamilyIPv6: "2001:db8::1",
	} {
		svc := &corev1.Service{}
		name := driver.AllocationPlaceholderName(claim.UID, family)
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: testPlaceholderNS, Name: name}, svc); err != nil {
			t.Fatal(err)
		}
		svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: ip}}
		if err := c.Status().Update(context.Background(), svc); err != nil {
			t.Fatal(err)
		}
	}
	reconcileOnce(t, r, "tenant-a", "web")

	addrs := listAddresses(t, c)
	if len(addrs) != 2 {
		t.Fatalf("got %d addresses, want both families recorded", len(addrs))
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
	claim := stampedClaim(localv1alpha1.FamilyIPv4)
	c := testClient(t, iadClass(), claim, allocationPlaceholder(claim, localv1alpha1.FamilyIPv4, "203.0.113.7"))
	r := claimRec(c)
	reconcileOnce(t, r, "tenant-a", "web")
	reconcileOnce(t, r, "tenant-a", "web")

	if addrs := listAddresses(t, c); len(addrs) != 1 {
		t.Errorf("got %d addresses after two reconciles, want 1", len(addrs))
	}
	// The linked placeholder keeps holding the reservation.
	if placeholders := listPlaceholders(t, c); len(placeholders) != 1 {
		t.Errorf("got %d placeholders, want the hold to remain", len(placeholders))
	}
}

func TestIgnoresForeignClaims(t *testing.T) {
	claim := stampedClaim(localv1alpha1.FamilyIPv4)
	claim.Annotations[localv1alpha1.ProvisionerAnnotation] = "someone.else.example.com"
	c := testClient(t, iadClass(), claim)
	reconcileOnce(t, claimRec(c), "tenant-a", "web")

	if placeholders := listPlaceholders(t, c); len(placeholders) != 0 {
		t.Errorf("created %d placeholders for another driver's claim", len(placeholders))
	}
}

func TestOrphanPlaceholderCleanup(t *testing.T) {
	claim := stampedClaim(localv1alpha1.FamilyIPv4)
	// An allocation placeholder that never materialized an IPAddress...
	orphan := allocationPlaceholder(claim, localv1alpha1.FamilyIPv4, "")
	// ...and one linked to a live IPAddress (Retain semantics: it must stay).
	held := driver.NewPlaceholder("iad-ip-203-0-113-9", testPlaceholderNS, "public", localv1alpha1.FamilyIPv4, "203.0.113.9")
	held.Annotations[driver.PlaceholderClaimNamespaceAnnotation] = claim.Namespace
	held.Annotations[driver.PlaceholderClaimNameAnnotation] = claim.Name
	backing := &localv1alpha1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Name: "ip-203-0-113-9"},
		Spec: localv1alpha1.IPAddressSpec{
			ClassName: "public",
			Address:   "203.0.113.9",
			ClaimRef:  &localv1alpha1.ClaimReference{Namespace: "tenant-a", Name: "web", UID: "claim-uid-1"},
			Source:    localv1alpha1.IPAddressSource{FromClass: &localv1alpha1.FromClassSource{}},
		},
	}
	// The claim itself is gone.
	c := testClient(t, iadClass(), orphan, held, backing)
	reconcileOnce(t, claimRec(c), "tenant-a", "web")

	err := c.Get(context.Background(), types.NamespacedName{Namespace: testPlaceholderNS, Name: orphan.Name}, &corev1.Service{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("orphan placeholder still present: %v", err)
	}
	err = c.Get(context.Background(), types.NamespacedName{Namespace: testPlaceholderNS, Name: held.Name}, &corev1.Service{})
	if err != nil {
		t.Errorf("held placeholder was deleted; Retain reservations must survive the claim: %v", err)
	}
}
