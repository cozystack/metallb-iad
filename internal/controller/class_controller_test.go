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
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	localv1alpha1 "github.com/cozystack/address-controller/api/v1alpha1"
)

func classRec(c client.Client, lbClass string) (*ClassReconciler, *record.FakeRecorder) {
	recorder := record.NewFakeRecorder(100)
	return &ClassReconciler{
		Client: c, Scheme: c.Scheme(),
		Recorder:          recorder,
		MetalLBNamespace:  testMetalLBNS,
		LoadBalancerClass: lbClass,
	}, recorder
}

// drainEvents returns the events recorded so far.
func drainEvents(recorder *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case e := <-recorder.Events:
			events = append(events, e)
		default:
			return events
		}
	}
}

// getClass reads the test class back.
func getClass(t *testing.T, c client.Client) *localv1alpha1.IPAddressClass {
	t.Helper()
	class := &localv1alpha1.IPAddressClass{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "public"}, class); err != nil {
		t.Fatal(err)
	}
	return class
}

func TestClassWithUsablePoolIsAccepted(t *testing.T) {
	for _, lbClass := range []string{"", "metallb.io/reserved"} {
		t.Run(fmt.Sprintf("--lb-class %q", lbClass), func(t *testing.T) {
			c := testClient(t, iadClass())
			r, recorder := classRec(c, lbClass)
			reconcileOnce(t, r, "", "public")

			if events := drainEvents(recorder); len(events) != 0 {
				t.Errorf("events = %v, want none for a usable pool", events)
			}
			got, ok := getClass(t, c).AcceptedLoadBalancerClass()
			if !ok || got != lbClass {
				t.Errorf("AcceptedLoadBalancerClass() = %q, %t; want %q, true", got, ok, lbClass)
			}
		})
	}
}

func TestClassPoolIsNotRendered(t *testing.T) {
	c := testClient(t, iadClass())
	r, _ := classRec(c, "")
	reconcileOnce(t, r, "", "public")

	pools := &unstructured.UnstructuredList{}
	pools.SetGroupVersionKind(ipAddressPoolGVK.GroupVersion().WithKind("IPAddressPoolList"))
	if err := c.List(context.Background(), pools); err != nil {
		t.Fatal(err)
	}
	if len(pools.Items) != 1 || pools.Items[0].GetName() != testPool {
		t.Errorf("pools = %d, want only the admin's %q: the driver must not render one", len(pools.Items), testPool)
	}
}

func TestClassPoolProblemsAreReported(t *testing.T) {
	oldParams := iadClass()
	oldParams.Spec.Parameters = &runtime.RawExtension{Raw: []byte(`{"addresses":["203.0.113.0/29"]}`)}
	twin := iadClass()
	twin.Name = "public-twin"
	cases := map[string]client.Client{
		"pool missing":      bareTestClient(t, iadClass()),
		"pool auto-assigns": bareTestClient(t, metallbPool(true), iadClass()),
		"old parameters":    testClient(t, oldParams),
		"pool named twice":  testClient(t, iadClass(), twin),
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r, recorder := classRec(c, "")
			reconcileOnce(t, r, "", "public")

			events := drainEvents(recorder)
			if len(events) != 1 || !strings.Contains(events[0], ReasonPoolRefused) {
				t.Errorf("events = %v, want one PoolRefused", events)
			}
			cond := meta.FindStatusCondition(getClass(t, c).Status.Conditions, localv1alpha1.ConditionAccepted)
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonPoolRefused {
				t.Errorf("Accepted condition = %+v, want False with reason %s", cond, ReasonPoolRefused)
			}
		})
	}
}

func TestClassOfAnotherProvisionerIsIgnored(t *testing.T) {
	class := iadClass()
	class.Spec.Provisioner = "someone.else.example.com"
	c := bareTestClient(t, class)
	r, recorder := classRec(c, "")
	reconcileOnce(t, r, "", "public")

	if events := drainEvents(recorder); len(events) != 0 {
		t.Errorf("events = %v, want none for another driver's class", events)
	}
	if status := getClass(t, c).Status; len(status.Conditions) != 0 || status.LoadBalancerClass != "" {
		t.Errorf("status = %+v, want another driver's class left unreported", status)
	}
}
