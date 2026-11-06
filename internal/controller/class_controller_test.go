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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
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

func TestClassWithUsablePoolIsQuiet(t *testing.T) {
	c := testClient(t, iadClass())
	r, recorder := classRec(c, "")
	reconcileOnce(t, r, "", "public")

	if events := drainEvents(recorder); len(events) != 0 {
		t.Errorf("events = %v, want none for a usable pool", events)
	}
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
			if len(events) != 1 || !strings.Contains(events[0], "PoolRefused") {
				t.Errorf("events = %v, want one PoolRefused", events)
			}
		})
	}
}

func TestAutoClaimClassOfANonDefaultInstanceWarns(t *testing.T) {
	class := iadClass()
	class.Annotations = map[string]string{localv1alpha1.IsAutoClaimClassAnnotation: "true"}
	r, recorder := classRec(testClient(t, class), "metallb.io/reserved")
	reconcileOnce(t, r, "", "public")

	events := drainEvents(recorder)
	if len(events) != 1 || !strings.Contains(events[0], "AutoClaimUnservable") {
		t.Errorf("events = %v, want one AutoClaimUnservable", events)
	}
}

func TestClassOfAnotherProvisionerIsIgnored(t *testing.T) {
	class := iadClass()
	class.Spec.Provisioner = "someone.else.example.com"
	r, recorder := classRec(bareTestClient(t, class), "")
	reconcileOnce(t, r, "", "public")

	if events := drainEvents(recorder); len(events) != 0 {
		t.Errorf("events = %v, want none for another driver's class", events)
	}
}
