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

	localv1alpha1 "github.com/lllamnyp/address-controller/api/v1alpha1"
	"github.com/lllamnyp/metallb-iad/internal/driver"
)

func TestTeardownWithdrawsPinAndReleasesFinalizer(t *testing.T) {
	claim, addr := boundClaimWithAddress()
	addr.Finalizers = []string{driver.TeardownFinalizer}
	addr.Status.AssociatedTo = &localv1alpha1.AssociationReference{
		Kind: "Service", Namespace: "tenant-a", Name: "web-lb",
	}
	svc := lbService("web-lb", map[string]string{
		driver.MetalLBPinAnnotation: "203.0.113.1",
		driver.PinnedAnnotation:     "true",
	})
	c := testClient(t, claim, addr, svc)
	if err := c.Delete(context.Background(), addr); err != nil {
		t.Fatal(err)
	}
	r := &IPAddressReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(100)}
	reconcileOnce(t, r, "", "ip-203-0-113-1")

	err := c.Get(context.Background(), types.NamespacedName{Name: "ip-203-0-113-1"}, &localv1alpha1.IPAddress{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("address still present after teardown: %v", err)
	}
	got := getService(t, c, "web-lb")
	if _, still := got.Annotations[driver.MetalLBPinAnnotation]; still {
		t.Error("pin annotation not withdrawn on address deletion")
	}
}

func TestConflictClearsWhenOffenderGone(t *testing.T) {
	claim, addr := boundClaimWithAddress()
	addr.Status.Phase = localv1alpha1.IPAddressConflict
	// No Service holds the address anymore.
	c := testClient(t, claim, addr)
	r := &IPAddressReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(100)}
	reconcileOnce(t, r, "", "ip-203-0-113-1")

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
	r := &IPAddressReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(100)}
	reconcileOnce(t, r, "", "ip-203-0-113-1")

	if got := getAddr(t, c, "ip-203-0-113-1"); got.Status.Phase != localv1alpha1.IPAddressConflict {
		t.Errorf("phase = %q, want Conflict to persist", got.Status.Phase)
	}
}
