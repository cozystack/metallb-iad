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

func TestClassOfAnotherProvisionerIsIgnored(t *testing.T) {
	class := iadClass()
	class.Spec.Provisioner = "someone.else.example.com"
	r, recorder := classRec(bareTestClient(t, class), "")
	reconcileOnce(t, r, "", "public")

	if events := drainEvents(recorder); len(events) != 0 {
		t.Errorf("events = %v, want none for another driver's class", events)
	}
}

func TestClassIsCheckedOnlyByTheInstanceServingIt(t *testing.T) {
	cases := map[string]struct {
		classLB  *string
		driverLB string
		served   bool
	}{
		"unset, default instance":     {classLB: nil, driverLB: "", served: true},
		"unset, non-default instance": {classLB: nil, driverLB: "metallb.io/reserved"},
		"matching":                    {classLB: new("metallb.io/reserved"), driverLB: "metallb.io/reserved", served: true},
		"another instance's":          {classLB: new("metallb.io/other"), driverLB: "metallb.io/reserved"},
		"set, default instance":       {classLB: new("metallb.io/other"), driverLB: ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			class := iadClass()
			class.Spec.LoadBalancerClass = tc.classLB
			// No pool: the instance serving the class reports it missing.
			r, recorder := classRec(bareTestClient(t, class), tc.driverLB)
			reconcileOnce(t, r, "", "public")

			events := drainEvents(recorder)
			if tc.served && (len(events) != 1 || !strings.Contains(events[0], "PoolRefused")) {
				t.Errorf("events = %v, want one PoolRefused from the serving instance", events)
			}
			if !tc.served && len(events) != 0 {
				t.Errorf("events = %v, want none from an instance not serving the class", events)
			}
		})
	}
}

func TestPoolNamedByAnotherInstancesClassIsNotShared(t *testing.T) {
	// Another instance's class naming a pool of the same name refers to
	// that instance's MetalLB, not to ours.
	other := iadClass()
	other.Name = "public-other"
	other.Spec.LoadBalancerClass = new("metallb.io/other")
	r, recorder := classRec(testClient(t, iadClass(), other), "")
	reconcileOnce(t, r, "", "public")

	if events := drainEvents(recorder); len(events) != 0 {
		t.Errorf("events = %v, want none: the pool is named once among this instance's classes", events)
	}
}
