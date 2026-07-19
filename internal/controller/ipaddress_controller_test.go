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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	localv1alpha1 "github.com/lllamnyp/address-controller/api/v1alpha1"
	"github.com/lllamnyp/metallb-iad/internal/driver"
)

func addressRec(c client.Client) *IPAddressReconciler {
	return &IPAddressReconciler{
		Client:               c,
		Scheme:               c.Scheme(),
		Recorder:             record.NewFakeRecorder(100),
		PlaceholderNamespace: testPlaceholderNS,
	}
}

func TestUnassociatedAddressGetsHoldPlaceholder(t *testing.T) {
	claim, addr := boundClaimWithAddress()
	c := testClient(t, iadClass(), claim, addr)
	reconcileOnce(t, addressRec(c), "", "ip-203-0-113-1")

	hold := &corev1.Service{}
	err := c.Get(context.Background(), types.NamespacedName{
		Namespace: testPlaceholderNS, Name: driver.HoldPlaceholderName("ip-203-0-113-1"),
	}, hold)
	if err != nil {
		t.Fatalf("hold placeholder not created: %v", err)
	}
	if hold.Annotations[driver.MetalLBPinAnnotation] != "203.0.113.1" {
		t.Errorf("hold pin = %q, want the held address", hold.Annotations[driver.MetalLBPinAnnotation])
	}
	if hold.Labels[driver.PlaceholderAddressLabel] != "ip-203-0-113-1" {
		t.Error("hold placeholder not linked to the address")
	}
}

func TestAssociatedAddressDropsHold(t *testing.T) {
	claim, addr := boundClaimWithAddress()
	addr.Status.AssociatedTo = &localv1alpha1.AssociationReference{
		Kind: "Service", Namespace: "tenant-a", Name: "web-lb",
	}
	liveHolder := lbService("web-lb", nil)
	hold := holdPlaceholderFor(addr)
	c := testClient(t, iadClass(), claim, addr, liveHolder, hold)
	reconcileOnce(t, addressRec(c), "", "ip-203-0-113-1")

	err := c.Get(context.Background(), types.NamespacedName{Namespace: testPlaceholderNS, Name: hold.Name}, &corev1.Service{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("placeholder still present while the address is associated: %v", err)
	}
}

func TestStaleAssociationClearedAndReHeld(t *testing.T) {
	// The associated Service is gone and its deletion event was missed:
	// the association must clear and the hold must re-arm.
	claim, addr := boundClaimWithAddress()
	addr.Status.AssociatedTo = &localv1alpha1.AssociationReference{
		Kind: "Service", Namespace: "tenant-a", Name: "deleted-vm",
	}
	c := testClient(t, iadClass(), claim, addr)
	reconcileOnce(t, addressRec(c), "", "ip-203-0-113-1")

	got := getAddr(t, c, "ip-203-0-113-1")
	if got.Status.AssociatedTo != nil {
		t.Errorf("associatedTo = %+v, want cleared", got.Status.AssociatedTo)
	}
	err := c.Get(context.Background(), types.NamespacedName{
		Namespace: testPlaceholderNS, Name: driver.HoldPlaceholderName("ip-203-0-113-1"),
	}, &corev1.Service{})
	if err != nil {
		t.Errorf("hold placeholder not re-armed: %v", err)
	}
}

func TestForeignClassAddressIsIgnored(t *testing.T) {
	class := iadClass()
	class.Spec.Provisioner = "someone.else.example.com"
	_, addr := boundClaimWithAddress()
	c := testClient(t, class, addr)
	reconcileOnce(t, addressRec(c), "", "ip-203-0-113-1")

	if placeholders := listPlaceholders(t, c); len(placeholders) != 0 {
		t.Errorf("created placeholders for another driver's address: %d", len(placeholders))
	}
}

func TestTeardownDeletesPlaceholderAndWithdrawsPin(t *testing.T) {
	claim, addr := boundClaimWithAddress()
	addr.Finalizers = []string{driver.TeardownFinalizer}
	addr.Status.AssociatedTo = &localv1alpha1.AssociationReference{
		Kind: "Service", Namespace: "tenant-a", Name: "web-lb",
	}
	svc := lbService("web-lb", map[string]string{
		driver.MetalLBPinAnnotation: "203.0.113.1",
		driver.PinnedAnnotation:     "true",
	})
	hold := holdPlaceholderFor(addr)
	c := testClient(t, iadClass(), claim, addr, svc, hold)
	if err := c.Delete(context.Background(), addr); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, addressRec(c), "", "ip-203-0-113-1")

	err := c.Get(context.Background(), types.NamespacedName{Name: "ip-203-0-113-1"}, &localv1alpha1.IPAddress{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("address still present after teardown: %v", err)
	}
	err = c.Get(context.Background(), types.NamespacedName{Namespace: testPlaceholderNS, Name: hold.Name}, &corev1.Service{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("placeholder outlived its address: %v", err)
	}
	got := getService(t, c, "web-lb")
	if _, still := got.Annotations[driver.MetalLBPinAnnotation]; still {
		t.Error("pin annotation not withdrawn on address deletion")
	}
}

func TestConflictClearsWhenOffenderGone(t *testing.T) {
	claim, addr := boundClaimWithAddress()
	addr.Status.Phase = localv1alpha1.IPAddressConflict
	// The driver's own placeholder still holds the address — that is an
	// authorized holder, not an offender, and must not block clearing.
	hold := holdPlaceholderFor(addr)
	hold.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "203.0.113.1"}}
	c := testClient(t, claim, addr, hold)
	reconcileOnce(t, addressRec(c), "", "ip-203-0-113-1")

	if got := getAddr(t, c, "ip-203-0-113-1"); got.Status.Phase != localv1alpha1.IPAddressBound {
		t.Errorf("phase = %q, want Bound after the conflict cleared", got.Status.Phase)
	}
}

func TestConflictPersistsWhileOffenderHoldsAddress(t *testing.T) {
	claim, addr := boundClaimWithAddress()
	addr.Status.Phase = localv1alpha1.IPAddressConflict
	thief := lbService("thief", nil)
	thief.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "203.0.113.1"}}
	c := testClient(t, claim, addr, thief)
	reconcileOnce(t, addressRec(c), "", "ip-203-0-113-1")

	if got := getAddr(t, c, "ip-203-0-113-1"); got.Status.Phase != localv1alpha1.IPAddressConflict {
		t.Errorf("phase = %q, want Conflict to persist", got.Status.Phase)
	}
}
