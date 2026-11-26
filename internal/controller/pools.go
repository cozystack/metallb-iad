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
	"slices"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	localv1alpha1 "github.com/cozystack/address-controller/api/v1alpha1"
	"github.com/cozystack/metallb-iad/internal/driver"
)

var ipAddressPoolGVK = schema.GroupVersionKind{
	Group: "metallb.io", Version: "v1beta1", Kind: "IPAddressPool",
}

// classPool resolves the MetalLB pool a class this instance serves names
// (servesClass with lbClass). A non-empty problem says why the class cannot
// back new reservations: its parameters are invalid, the pool is missing or
// auto-assigns (driver.PoolProblem), or another class of this instance
// names the same pool. The driver never edits the pool.
func classPool(ctx context.Context, c client.Reader, metallbNS, lbClass string, class *localv1alpha1.IPAddressClass) (pool, problem string, err error) {
	params, err := driver.ParseClassParameters(class.Spec.Parameters)
	if err != nil {
		return "", err.Error(), nil
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(ipAddressPoolGVK)
	if err := c.Get(ctx, types.NamespacedName{Namespace: metallbNS, Name: params.Pool}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return params.Pool, fmt.Sprintf("IPAddressPool %s/%s not found", metallbNS, params.Pool), nil
		}
		return "", "", err
	}
	if problem := driver.PoolProblem(obj); problem != "" {
		return params.Pool, problem, nil
	}
	others, err := classesNamingPool(ctx, c, lbClass, params.Pool)
	if err != nil {
		return "", "", err
	}
	others = slices.DeleteFunc(others, func(name string) bool { return name == class.Name })
	if len(others) > 0 {
		return params.Pool, fmt.Sprintf("IPAddressPool %s/%s is also named by IPAddressClass %v", metallbNS, params.Pool, others), nil
	}
	return params.Pool, "", nil
}

// classesNamingPool lists, sorted, the classes this instance serves whose
// parameters name pool.
func classesNamingPool(ctx context.Context, c client.Reader, lbClass, pool string) ([]string, error) {
	classes := &localv1alpha1.IPAddressClassList{}
	if err := c.List(ctx, classes); err != nil {
		return nil, err
	}
	var names []string
	for _, class := range classes.Items {
		if !servesClass(&class, lbClass) || !class.DeletionTimestamp.IsZero() {
			continue
		}
		if params, err := driver.ParseClassParameters(class.Spec.Parameters); err == nil && params.Pool == pool {
			names = append(names, class.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// servesClass reports whether this driver instance serves class: the class
// names this driver as its provisioner, and its loadBalancerClass is lbClass,
// the --lb-class of the MetalLB instance the driver fronts. Unset on the
// class and empty for lbClass both mean the cluster's default
// implementation. Every controller decides ownership by this rule alone, so
// instances fronting different MetalLBs split the classes between them.
func servesClass(class *localv1alpha1.IPAddressClass, lbClass string) bool {
	return class.Spec.Provisioner == driver.ProvisionerName &&
		deref(class.Spec.LoadBalancerClass) == lbClass
}

// deref reads an optional loadBalancerClass, unset as empty.
func deref(lbClass *string) string {
	if lbClass == nil {
		return ""
	}
	return *lbClass
}
