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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	localv1alpha1 "github.com/lllamnyp/address-controller/api/v1alpha1"
	"github.com/lllamnyp/metallb-iad/internal/driver"
)

func boundClaimWithAddress() (*localv1alpha1.IPAddressClaim, *localv1alpha1.IPAddress) {
	claim := stampedClaim(localv1alpha1.FamilyIPv4)
	claim.Status.Phase = localv1alpha1.ClaimBound
	claim.Status.Addresses = []localv1alpha1.BoundAddress{{Name: "ip-203-0-113-1", Address: "203.0.113.1"}}
	addr := &localv1alpha1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Name: "ip-203-0-113-1"},
		Spec: localv1alpha1.IPAddressSpec{
			ClassName: "public",
			Address:   "203.0.113.1",
			ClaimRef:  &localv1alpha1.ClaimReference{Namespace: "tenant-a", Name: "web", UID: "claim-uid-1"},
			Source:    localv1alpha1.IPAddressSource{FromClass: &localv1alpha1.FromClassSource{}},
		},
		Status: localv1alpha1.IPAddressStatus{Phase: localv1alpha1.IPAddressBound},
	}
	return claim, addr
}

func lbService(name string, annotations map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-a", Annotations: annotations},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
	}
}

func getService(t *testing.T, c client.Client, name string) *corev1.Service {
	t.Helper()
	svc := &corev1.Service{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant-a", Name: name}, svc); err != nil {
		t.Fatal(err)
	}
	return svc
}

func getAddr(t *testing.T, c client.Client, name string) *localv1alpha1.IPAddress {
	t.Helper()
	addr := &localv1alpha1.IPAddress{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, addr); err != nil {
		t.Fatal(err)
	}
	return addr
}

func TestAssociatePinsServiceAndRecordsAssociation(t *testing.T) {
	claim, addr := boundClaimWithAddress()
	svc := lbService("web-lb", map[string]string{localv1alpha1.ServiceClaimAnnotation: "web"})
	c := testClient(t, claim, addr, svc)
	r := &ServiceReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(100)}
	reconcileOnce(t, r, "tenant-a", "web-lb")

	got := getService(t, c, "web-lb")
	if pin := got.Annotations[driver.MetalLBPinAnnotation]; pin != "203.0.113.1" {
		t.Errorf("pin annotation = %q, want 203.0.113.1", pin)
	}
	if got.Annotations[driver.PinnedAnnotation] != "true" {
		t.Error("pinned marker missing")
	}
	holder := getAddr(t, c, "ip-203-0-113-1").Status.AssociatedTo
	if holder == nil || holder.Kind != "Service" || holder.Name != "web-lb" || holder.Namespace != "tenant-a" {
		t.Errorf("associatedTo = %+v", holder)
	}
}

func TestSecondServiceIsRejected(t *testing.T) {
	claim, addr := boundClaimWithAddress()
	addr.Status.AssociatedTo = &localv1alpha1.AssociationReference{
		Kind: "Service", Namespace: "tenant-a", Name: "web-lb",
	}
	holderSvc := lbService("web-lb", map[string]string{localv1alpha1.ServiceClaimAnnotation: "web"})
	thief := lbService("thief", map[string]string{localv1alpha1.ServiceClaimAnnotation: "web"})
	c := testClient(t, claim, addr, holderSvc, thief)
	r := &ServiceReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(100)}
	reconcileOnce(t, r, "tenant-a", "thief")

	got := getService(t, c, "thief")
	if _, pinned := got.Annotations[driver.MetalLBPinAnnotation]; pinned {
		t.Error("second Service was pinned; a 1:1 binding is 1:1")
	}
	holder := getAddr(t, c, "ip-203-0-113-1").Status.AssociatedTo
	if holder == nil || holder.Name != "web-lb" {
		t.Errorf("associatedTo = %+v, want unchanged web-lb", holder)
	}
}

func TestReassociationAfterHolderDeleted(t *testing.T) {
	// The cutover flow: the old workload's Service is gone (its deletion
	// event may have been missed), and the claim is attached to a new one.
	claim, addr := boundClaimWithAddress()
	addr.Status.AssociatedTo = &localv1alpha1.AssociationReference{
		Kind: "Service", Namespace: "tenant-a", Name: "old-vm",
	}
	newSvc := lbService("new-vm", map[string]string{localv1alpha1.ServiceClaimAnnotation: "web"})
	c := testClient(t, claim, addr, newSvc)
	r := &ServiceReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(100)}
	reconcileOnce(t, r, "tenant-a", "new-vm")

	got := getService(t, c, "new-vm")
	if pin := got.Annotations[driver.MetalLBPinAnnotation]; pin != "203.0.113.1" {
		t.Errorf("pin = %q, want the same address back on the new workload", pin)
	}
	holder := getAddr(t, c, "ip-203-0-113-1").Status.AssociatedTo
	if holder == nil || holder.Name != "new-vm" {
		t.Errorf("associatedTo = %+v, want taken over by new-vm", holder)
	}
}

func TestRemovingClaimAnnotationWithdrawsPin(t *testing.T) {
	claim, addr := boundClaimWithAddress()
	addr.Status.AssociatedTo = &localv1alpha1.AssociationReference{
		Kind: "Service", Namespace: "tenant-a", Name: "web-lb",
	}
	svc := lbService("web-lb", map[string]string{
		driver.MetalLBPinAnnotation: "203.0.113.1",
		driver.PinnedAnnotation:     "true",
	})
	c := testClient(t, claim, addr, svc)
	r := &ServiceReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(100)}
	reconcileOnce(t, r, "tenant-a", "web-lb")

	got := getService(t, c, "web-lb")
	if _, still := got.Annotations[driver.MetalLBPinAnnotation]; still {
		t.Error("pin annotation not withdrawn")
	}
	gotAddr := getAddr(t, c, "ip-203-0-113-1")
	if gotAddr.Status.AssociatedTo != nil {
		t.Errorf("associatedTo = %+v, want nil (reserved but inert)", gotAddr.Status.AssociatedTo)
	}
	if gotAddr.Spec.ClaimRef == nil {
		t.Error("claimRef gone; disassociation must not release the address")
	}
}

func TestHandWrittenPinIsNeverRemoved(t *testing.T) {
	// No PinnedAnnotation marker: the pin was written by someone else
	// (e.g. an allowlisted GitOps principal) and is not ours to touch.
	svc := lbService("hand-pinned", map[string]string{
		driver.MetalLBPinAnnotation: "198.51.100.9",
	})
	c := testClient(t, svc)
	r := &ServiceReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(100)}
	reconcileOnce(t, r, "tenant-a", "hand-pinned")

	got := getService(t, c, "hand-pinned")
	if got.Annotations[driver.MetalLBPinAnnotation] != "198.51.100.9" {
		t.Error("hand-written pin was modified")
	}
}

func TestWrongfulAssignmentDrivesConflict(t *testing.T) {
	claim, addr := boundClaimWithAddress()
	// A Service ends up holding the reserved address without any claim
	// reference — explicit pin, race, or impersonated write.
	thief := lbService("thief", nil)
	thief.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "203.0.113.1"}}
	c := testClient(t, claim, addr, thief)
	r := &ServiceReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(100)}
	reconcileOnce(t, r, "tenant-a", "thief")

	if got := getAddr(t, c, "ip-203-0-113-1"); got.Status.Phase != localv1alpha1.IPAddressConflict {
		t.Errorf("phase = %q, want Conflict", got.Status.Phase)
	}
}

func TestUntrackedAssignmentIsLeftAlone(t *testing.T) {
	// An eager allocator handing a plain Service an address no IPAddress
	// tracks is the normal case, not a conflict.
	svc := lbService("plain", nil)
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "198.51.100.7"}}
	c := testClient(t, svc)
	r := &ServiceReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(100)}
	reconcileOnce(t, r, "tenant-a", "plain")
	// Nothing to assert beyond "no error and no objects mutated": the
	// ledger is empty.
	if addrs := listAddresses(t, c); len(addrs) != 0 {
		t.Errorf("unexpected IPAddress objects: %v", addrs)
	}
}
