// Package kube is billing.Accounts over the cluster: an Account is a custom
// resource (docs/contracts/billing/crd-account.yaml) and the backend is
// its only writer. Every read is a read of the API server at that moment:
// there is no informer and no cache, so nothing here is a copy of anyone's
// credit.
package kube

import (
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

const group, version = "browserjs.dev", "v1alpha1"

var AccountGVR = schema.GroupVersionResource{Group: group, Version: version, Resource: "accounts"}

// LabelMetronomeCustomer is on an Account: its Metronome customer, which
// is all Metronome's alert names.
const LabelMetronomeCustomer = "browserjs.dev/metronome-customer"

// into reads a part of an object (its spec, its status) as v.
func into(obj *unstructured.Unstructured, field string, v any) error {
	part, found, err := unstructured.NestedMap(obj.Object, field)
	if err != nil || !found {
		return err
	}
	raw, err := json.Marshal(part)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// object is v as the JSON object a custom resource holds. Whole numbers are
// int64, as the API machinery has them.
func object(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return whole(out).(map[string]any), nil
}

func whole(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, item := range v {
			v[k] = whole(item)
		}
	case []any:
		for i, item := range v {
			v[i] = whole(item)
		}
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n
		}
		f, _ := v.Float64()
		return f
	}
	return v
}

// accountFrom reads an Account.
func accountFrom(obj *unstructured.Unstructured) (billing.Account, error) {
	acc := billing.Account{Name: obj.GetName(), Created: obj.GetCreationTimestamp().Time}
	if err := into(obj, "spec", &acc.Spec); err != nil {
		return billing.Account{}, fmt.Errorf("account %s: %w", obj.GetName(), err)
	}
	return acc, nil
}

// newAccount is the Account made for an owner seen for the first time.
func newAccount(owner string) *unstructured.Unstructured {
	hash := sessions.OwnerLabel(owner)
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": AccountGVR.GroupVersion().String(),
		"kind":       "Account",
		"metadata": map[string]any{
			"name":   billing.AccountName(owner),
			"labels": map[string]any{sessions.LabelOwner: hash},
		},
		"spec": map[string]any{"owner": owner, "ownerHash": hash},
	}}
}

// setSpec writes spec into an Account, and the label its Metronome
// customer is found by, leaving the rest of it alone.
func setSpec(obj *unstructured.Unstructured, spec billing.AccountSpec) error {
	m, err := object(spec)
	if err != nil {
		return err
	}
	obj.Object["spec"] = m
	if spec.MetronomeCustomerID != "" {
		labels := obj.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[LabelMetronomeCustomer] = spec.MetronomeCustomerID
		obj.SetLabels(labels)
	}
	return nil
}
