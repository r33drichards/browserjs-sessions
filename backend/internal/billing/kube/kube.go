package kube

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

// Resource is what Accounts needs of the API server: the four calls of a
// dynamic.ResourceInterface it makes.
type Resource interface {
	Get(ctx context.Context, name string, options metav1.GetOptions, subresources ...string) (*unstructured.Unstructured, error)
	List(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error)
	Create(ctx context.Context, obj *unstructured.Unstructured, options metav1.CreateOptions, subresources ...string) (*unstructured.Unstructured, error)
	Update(ctx context.Context, obj *unstructured.Unstructured, options metav1.UpdateOptions, subresources ...string) (*unstructured.Unstructured, error)
}

// Accounts is billing.Accounts over the cluster. It is made of its client
// and nothing else: each call reads the API server.
type Accounts struct {
	client Resource
}

// NewAccounts is the Accounts of a namespace.
func NewAccounts(client dynamic.Interface, namespace string) *Accounts {
	return &Accounts{client: client.Resource(AccountGVR).Namespace(namespace)}
}

// Over is Accounts over any Resource.
func Over(client Resource) *Accounts { return &Accounts{client: client} }

// Check reports whether the Account custom resource is served.
func (a *Accounts) Check(ctx context.Context) error {
	if _, err := a.client.List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("billing: the custom resource %s.%s is not served; apply its CRD or leave BILLING off", AccountGVR.Resource, AccountGVR.Group)
		}
		return fmt.Errorf("billing: %s: %w", AccountGVR.Resource, err)
	}
	return nil
}

func (a *Accounts) Ensure(ctx context.Context, owner string) (billing.Account, error) {
	owner = strings.ToLower(strings.TrimSpace(owner))
	if owner == "" {
		return billing.Account{}, sessions.ErrOwnerRequired
	}
	name := billing.AccountName(owner)
	obj, err := a.client.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		obj, err = a.client.Create(ctx, newAccount(owner), metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			obj, err = a.client.Get(ctx, name, metav1.GetOptions{})
		} else if err == nil {
			slog.Info("billing: account made", "account", name)
		}
	}
	if err != nil {
		return billing.Account{}, err
	}
	return accountFrom(obj)
}

func (a *Accounts) Get(ctx context.Context, name string) (billing.Account, error) {
	obj, err := a.client.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return billing.Account{}, billing.ErrNotFound
	}
	if err != nil {
		return billing.Account{}, err
	}
	return accountFrom(obj)
}

// list reads the Accounts a label selector finds ("" for all).
func (a *Accounts) list(ctx context.Context, selector string) ([]billing.Account, error) {
	list, err := a.client.List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	out := make([]billing.Account, 0, len(list.Items))
	for i := range list.Items {
		acc, err := accountFrom(&list.Items[i])
		if err != nil {
			slog.Error("billing: account not read", "err", err)
			continue
		}
		out = append(out, acc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (a *Accounts) first(ctx context.Context, selector string, match func(billing.AccountSpec) bool) (billing.Account, error) {
	all, err := a.list(ctx, selector)
	if err != nil {
		return billing.Account{}, err
	}
	for _, acc := range all {
		if match(acc.Spec) {
			return acc, nil
		}
	}
	return billing.Account{}, billing.ErrNotFound
}

func (a *Accounts) ByCustomer(ctx context.Context, customer string) (billing.Account, error) {
	return a.first(ctx, "", func(s billing.AccountSpec) bool { return customer != "" && s.StripeCustomerID == customer })
}

func (a *Accounts) ByMetronomeCustomer(ctx context.Context, id string) (billing.Account, error) {
	if id == "" {
		return billing.Account{}, billing.ErrNotFound
	}
	selector := labels.SelectorFromSet(labels.Set{LabelMetronomeCustomer: id}).String()
	return a.first(ctx, selector, func(s billing.AccountSpec) bool { return s.MetronomeCustomerID == id })
}

func (a *Accounts) ByPaymentMethod(ctx context.Context, id string) (billing.Account, error) {
	return a.first(ctx, "", func(s billing.AccountSpec) bool {
		if s.PaymentMethod == nil {
			return false
		}
		for _, have := range s.PaymentMethod.IDs {
			if have == id {
				return true
			}
		}
		return false
	})
}

func (a *Accounts) WithCustomer(ctx context.Context) ([]billing.Account, error) {
	all, err := a.list(ctx, "")
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, acc := range all {
		if acc.Spec.StripeCustomerID != "" {
			out = append(out, acc)
		}
	}
	return out, nil
}

// How often Update reads again after losing a race before giving up.
const updateAttempts = 5

func (a *Accounts) Update(ctx context.Context, name string, change func(*billing.AccountSpec) error) (billing.Account, error) {
	var err error
	for range updateAttempts {
		var obj *unstructured.Unstructured
		if obj, err = a.client.Get(ctx, name, metav1.GetOptions{}); err != nil {
			break
		}
		var acc billing.Account
		if acc, err = accountFrom(obj); err != nil {
			return billing.Account{}, err
		}
		if err = change(&acc.Spec); err != nil {
			return billing.Account{}, err
		}
		if err = setSpec(obj, acc.Spec); err != nil {
			return billing.Account{}, err
		}
		var written *unstructured.Unstructured
		// The write carries the read's resourceVersion: on a conflict the
		// change is made again, to the spec it is about to replace.
		if written, err = a.client.Update(ctx, obj, metav1.UpdateOptions{}); err == nil {
			return accountFrom(written)
		}
		if !apierrors.IsConflict(err) {
			break
		}
	}
	if apierrors.IsNotFound(err) {
		return billing.Account{}, billing.ErrNotFound
	}
	return billing.Account{}, err
}

var leaseGVR = schema.GroupVersionResource{Group: "coordination.k8s.io", Version: "v1", Resource: "leases"}

// ObserverLease is the Lease the observer renews after each window of
// usage it delivered to Metronome.
const ObserverLease = "billing-observer"

// Observer is billing.Observer: when the observer's Lease was renewed.
type Observer struct {
	leases dynamic.ResourceInterface
}

func NewObserver(client dynamic.Interface, namespace string) *Observer {
	return &Observer{leases: client.Resource(leaseGVR).Namespace(namespace)}
}

func (o *Observer) Renewed(ctx context.Context) (time.Time, error) {
	obj, err := o.leases.Get(ctx, ObserverLease, metav1.GetOptions{})
	if err != nil {
		return time.Time{}, err
	}
	raw, _, _ := unstructured.NestedString(obj.Object, "spec", "renewTime")
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000000Z07:00"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("billing: the Lease %s has no renewTime", ObserverLease)
}
