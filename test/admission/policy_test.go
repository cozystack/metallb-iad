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

// Package admission runs the chart's optional ValidatingAdmissionPolicy
// against a real kube-apiserver (envtest): every write it must refuse, and
// the driver and admin writes it must admit.
package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	releaseNS = "cozy-metallb-iad"
	tenantNS  = "tenant-a"
	otherNS   = "system-x"
	tenant    = "alice"
	driverSA  = "system:serviceaccount:" + releaseNS + ":metallb-iad"
)

func TestAdmissionPolicy(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run through `make test`")
	}
	policy := renderPolicy(t)

	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	ctx := context.Background()
	admin := newClient(t, cfg, "", nil)
	for _, obj := range fixtures() {
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatalf("creating fixture %s: %v", obj.GetName(), err)
		}
	}
	for _, obj := range policy {
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatalf("creating %s %s: %v", obj.GetKind(), obj.GetName(), err)
		}
	}

	alice := newClient(t, cfg, tenant, []string{"system:authenticated"})
	driver := newClient(t, cfg, driverSA, []string{"system:serviceaccounts", "system:authenticated"})
	waitForEnforcement(t, alice)

	// pinned is a tenant Service the driver has pinned, created by the
	// exempt admin so the update cases start from a pinned state.
	pinned := lbService("pinned", tenantNS, map[string]string{
		"metallb.io/loadBalancerIPs":                    "203.0.113.7",
		"metallb.drivers.local.sdn.cozystack.io/pinned": "true",
	})
	if err := admin.Create(ctx, pinned); err != nil {
		t.Fatal(err)
	}

	const (
		pin       = "metallb.io/loadBalancerIPs"
		pool      = "metallb.io/address-pool"
		share     = "metallb.io/allow-shared-ip"
		oldPin    = "metallb.universe.tf/loadBalancerIPs"
		oldPool   = "metallb.universe.tf/address-pool"
		oldShare  = "metallb.universe.tf/allow-shared-ip"
		marker    = "metallb.drivers.local.sdn.cozystack.io/pinned"
		claimRef  = "local.sdn.cozystack.io/ip-address-claim"
		addr      = "203.0.113.8"
		otherAddr = "203.0.113.9"
	)
	relabel := map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": "web"}}}
	cases := []struct {
		name  string
		as    client.Client
		write func(client.Client) error
		admit bool
	}{
		{"tenant creates a plain Service", alice, createSvc(tenantNS, nil, ""), true},
		{"tenant names a claim", alice, createSvc(tenantNS, ann(claimRef, "web"), ""), true},
		{"tenant pins an address", alice, createSvc(tenantNS, ann(pin, addr), ""), false},
		{"tenant selects a pool", alice, createSvc(tenantNS, ann(pool, "reserved"), ""), false},
		{"tenant shares an address", alice, createSvc(tenantNS, ann(share, "k"), ""), false},
		{"tenant pins through the legacy prefix", alice, createSvc(tenantNS, ann(oldPin, addr), ""), false},
		{"tenant selects a pool through the legacy prefix", alice, createSvc(tenantNS, ann(oldPool, "reserved"), ""), false},
		{"tenant shares through the legacy prefix", alice, createSvc(tenantNS, ann(oldShare, "k"), ""), false},
		{"tenant sets spec.loadBalancerIP", alice, createSvc(tenantNS, nil, addr), false},
		{"tenant forges the driver's pinned marker", alice, createSvc(tenantNS, ann(marker, "true"), ""), false},
		{"tenant pins outside the bound namespaces", alice, createSvc(otherNS, ann(pin, addr), ""), true},
		{"tenant edits a pinned Service, pin untouched", alice, patchPinned(relabel), true},
		{"tenant changes the pin", alice, patchPinned(annotation(pin, otherAddr)), false},
		{"tenant removes the pin", alice, patchPinned(annotation(pin, nil)), false},
		{"tenant removes the pinned marker", alice, patchPinned(annotation(marker, nil)), false},
		{"driver pins a tenant Service", driver, createSvc(tenantNS, map[string]string{pin: addr, marker: "true"}, ""), true},
		{"driver withdraws its pin", driver, patchPinned(annotation(pin, nil)), true},
		{"admin selects a pool", admin, createSvc(tenantNS, ann(pool, "reserved"), ""), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.write(tc.as)
			switch {
			case tc.admit && err != nil:
				t.Errorf("denied, want admitted: %v", err)
			case !tc.admit && err == nil:
				t.Error("admitted, want denied")
			case !tc.admit && !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err):
				t.Errorf("failed for another reason than admission: %v", err)
			}
		})
	}
}

// renderPolicy renders the chart's admission policy with the binding scoped
// to namespaces labelled tenant=yes.
func renderPolicy(t *testing.T) []*unstructured.Unstructured {
	t.Helper()
	out, err := exec.Command("helm", "template", "metallb-iad", "../../chart/metallb-iad",
		"--namespace", releaseNS,
		"--set", "admissionPolicy.enabled=true",
		"--set-json", `admissionPolicy.namespaceSelector={"matchLabels":{"tenant":"yes"}}`,
		"--show-only", "templates/admission-policy.yaml").Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	var objs []*unstructured.Unstructured
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(&obj.Object); err != nil {
			if errors.Is(err, io.EOF) {
				return objs
			}
			t.Fatalf("decoding rendered policy: %v", err)
		}
		if len(obj.Object) > 0 {
			objs = append(objs, obj)
		}
	}
}

// fixtures are the namespaces and the RBAC that lets the tenant and the
// driver write Services, so only admission can refuse them.
func fixtures() []client.Object {
	return []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: releaseNS}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantNS, Labels: map[string]string{"tenant": "yes"}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: otherNS}},
		&rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "writers"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
			Subjects: []rbacv1.Subject{
				{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: tenant},
				{Kind: rbacv1.ServiceAccountKind, Name: "metallb-iad", Namespace: releaseNS},
			},
		},
	}
}

// waitForEnforcement waits until the API server has loaded the policy and
// its binding, which it does asynchronously.
func waitForEnforcement(t *testing.T, tenantClient client.Client) {
	t.Helper()
	probe := createSvc(tenantNS, map[string]string{"metallb.io/loadBalancerIPs": "203.0.113.1"}, "")
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := probe(tenantClient)
		if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("policy not enforced after 30s; last result: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func newClient(t *testing.T, cfg *rest.Config, user string, groups []string) client.Client {
	t.Helper()
	cfg = rest.CopyConfig(cfg)
	if user != "" {
		cfg.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: groups}
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func lbService(name, namespace string, annotations map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Annotations: annotations},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{{Port: 80}},
		},
	}
}

// createSvc dry-runs the creation of a LoadBalancer Service; admission runs
// in full on a dry run.
func createSvc(namespace string, annotations map[string]string, loadBalancerIP string) func(client.Client) error {
	return func(c client.Client) error {
		svc := lbService("", namespace, annotations)
		svc.GenerateName = "probe-"
		svc.Spec.LoadBalancerIP = loadBalancerIP
		return c.Create(context.Background(), svc, client.DryRunAll)
	}
}

// patchPinned dry-runs a merge patch on the pinned tenant Service.
func patchPinned(patch map[string]any) func(client.Client) error {
	return func(c client.Client) error {
		svc := &corev1.Service{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: tenantNS, Name: "pinned"}, svc); err != nil {
			return err
		}
		return c.Patch(context.Background(), svc, client.RawPatch(types.MergePatchType, mustJSON(patch)), client.DryRunAll)
	}
}

func ann(key, value string) map[string]string { return map[string]string{key: value} }

// annotation builds a merge patch that sets one annotation, or removes it
// when value is nil.
func annotation(key string, value any) map[string]any {
	return map[string]any{"metadata": map[string]any{"annotations": map[string]any{key: value}}}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
