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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
)

func TestClassRendersPoolAndAdvertisement(t *testing.T) {
	c := testClient(t, iadClass())
	r := &ClassReconciler{
		Client: c, Scheme: c.Scheme(),
		Recorder:         record.NewFakeRecorder(100),
		MetalLBNamespace: "metallb-system",
	}
	reconcileOnce(t, r, "", "public")

	pool := &unstructured.Unstructured{}
	pool.SetGroupVersionKind(ipAddressPoolGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "metallb-system", Name: "iad-public"}, pool); err != nil {
		t.Fatalf("IPAddressPool not rendered: %v", err)
	}
	autoAssign, found, err := unstructured.NestedBool(pool.Object, "spec", "autoAssign")
	if err != nil || !found || autoAssign {
		t.Errorf("autoAssign = %v/%v/%v, want explicit false", autoAssign, found, err)
	}
	addresses, _, _ := unstructured.NestedStringSlice(pool.Object, "spec", "addresses")
	if len(addresses) != 2 || addresses[0] != "203.0.113.0/29" {
		t.Errorf("pool addresses = %v", addresses)
	}
	if owners := pool.GetOwnerReferences(); len(owners) != 1 || owners[0].Name != "public" {
		t.Errorf("ownerReferences = %+v, want the class", owners)
	}

	adv := &unstructured.Unstructured{}
	adv.SetGroupVersionKind(l2AdvertisementGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "metallb-system", Name: "iad-public"}, adv); err != nil {
		t.Fatalf("L2Advertisement not rendered: %v", err)
	}
	pools, _, _ := unstructured.NestedStringSlice(adv.Object, "spec", "ipAddressPools")
	if len(pools) != 1 || pools[0] != "iad-public" {
		t.Errorf("advertisement pools = %v, want [iad-public]", pools)
	}
}

func TestClassOfAnotherProvisionerIsIgnored(t *testing.T) {
	class := iadClass()
	class.Spec.Provisioner = "someone.else.example.com"
	c := testClient(t, class)
	r := &ClassReconciler{
		Client: c, Scheme: c.Scheme(),
		Recorder:         record.NewFakeRecorder(100),
		MetalLBNamespace: "metallb-system",
	}
	reconcileOnce(t, r, "", "public")

	pool := &unstructured.Unstructured{}
	pool.SetGroupVersionKind(ipAddressPoolGVK)
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "metallb-system", Name: "iad-public"}, pool)
	if err == nil {
		t.Error("pool rendered for another driver's class")
	}
}
