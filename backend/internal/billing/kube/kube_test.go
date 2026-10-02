package kube

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

var accountsResource = schema.GroupResource{Group: group, Resource: "accounts"}

// server is the API server as Accounts uses it: objects in a map, each
// write checked against the resourceVersion it was read at, and every call
// counted. It is not client-go's fake: nothing here needs client-go.
type server struct {
	objects  map[string]*unstructured.Unstructured
	version  int
	gets     int
	lists    int
	updates  int
	conflict int // this many Updates lose a race first
}

func newServer() *server { return &server{objects: map[string]*unstructured.Unstructured{}} }

func (s *server) Get(_ context.Context, name string, _ metav1.GetOptions, _ ...string) (*unstructured.Unstructured, error) {
	s.gets++
	obj, ok := s.objects[name]
	if !ok {
		return nil, apierrors.NewNotFound(accountsResource, name)
	}
	return obj.DeepCopy(), nil
}

func (s *server) List(_ context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	s.lists++
	selector, err := labels.Parse(opts.LabelSelector)
	if err != nil {
		return nil, err
	}
	list := &unstructured.UnstructuredList{}
	for _, obj := range s.objects {
		if selector.Matches(labels.Set(obj.GetLabels())) {
			list.Items = append(list.Items, *obj.DeepCopy())
		}
	}
	return list, nil
}

func (s *server) store(obj *unstructured.Unstructured) *unstructured.Unstructured {
	s.version++
	obj = obj.DeepCopy()
	obj.SetResourceVersion(strconv.Itoa(s.version))
	s.objects[obj.GetName()] = obj
	return obj.DeepCopy()
}

func (s *server) Create(_ context.Context, obj *unstructured.Unstructured, _ metav1.CreateOptions, _ ...string) (*unstructured.Unstructured, error) {
	if _, ok := s.objects[obj.GetName()]; ok {
		return nil, apierrors.NewAlreadyExists(accountsResource, obj.GetName())
	}
	obj = obj.DeepCopy()
	obj.SetCreationTimestamp(metav1.NewTime(time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)))
	return s.store(obj), nil
}

func (s *server) Update(_ context.Context, obj *unstructured.Unstructured, _ metav1.UpdateOptions, _ ...string) (*unstructured.Unstructured, error) {
	s.updates++
	have, ok := s.objects[obj.GetName()]
	if !ok {
		return nil, apierrors.NewNotFound(accountsResource, obj.GetName())
	}
	if s.conflict > 0 {
		// Someone else wrote first.
		s.conflict--
		other := have.DeepCopy()
		_ = unstructured.SetNestedField(other.Object, true, "spec", "exempt")
		s.store(other)
		return nil, apierrors.NewConflict(accountsResource, obj.GetName(), errors.New("the object has been modified"))
	}
	if obj.GetResourceVersion() != have.GetResourceVersion() {
		return nil, apierrors.NewConflict(accountsResource, obj.GetName(), errors.New("the object has been modified"))
	}
	return s.store(obj), nil
}

func TestEnsureMakesTheAccountOnce(t *testing.T) {
	api := newServer()
	accounts := Over(api)
	ctx := t.Context()

	acc, err := accounts.Ensure(ctx, " U@Example.com ")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Name != billing.AccountName("u@example.com") || acc.Spec.Owner != "u@example.com" || acc.Spec.OwnerHash != sessions.OwnerLabel("u@example.com") || acc.Created.IsZero() {
		t.Fatalf("made %+v", acc)
	}
	obj := api.objects[acc.Name]
	if obj.GetKind() != "Account" || obj.GetLabels()[sessions.LabelOwner] != acc.Spec.OwnerHash {
		t.Fatalf("object %v", obj.Object)
	}
	if again, err := accounts.Ensure(ctx, "u@example.com"); err != nil || again.Name != acc.Name || len(api.objects) != 1 {
		t.Fatalf("a second Ensure: %+v, %v, %d objects", again, err, len(api.objects))
	}
	if _, err := accounts.Ensure(ctx, ""); err == nil {
		t.Error("an account for nobody")
	}
	if _, err := accounts.Get(ctx, "acct-none"); !errors.Is(err, billing.ErrNotFound) {
		t.Errorf("Get of none: %v", err)
	}
}

// There is no informer and no cache: every read is a read of the API
// server, and a change made there is seen by the next call.
func TestEveryReadIsARead(t *testing.T) {
	api := newServer()
	accounts := Over(api)
	ctx := t.Context()
	acc, _ := accounts.Ensure(ctx, "u@example.com")

	before := api.gets
	for range 2 {
		if _, err := accounts.Get(ctx, acc.Name); err != nil {
			t.Fatal(err)
		}
	}
	if api.gets-before != 2 {
		t.Fatalf("two Gets made %d reads", api.gets-before)
	}
	before = api.gets
	for range 2 {
		if _, err := accounts.Ensure(ctx, "u@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	if api.gets-before != 2 {
		t.Fatalf("two Ensures made %d reads", api.gets-before)
	}
	// Written behind its back (kubectl, another process): seen at once.
	_ = unstructured.SetNestedField(api.objects[acc.Name].Object, map[string]any{"reason": "abuse"}, "spec", "blocked")
	if got, _ := accounts.Get(ctx, acc.Name); got.Spec.Blocked == nil || got.Spec.Blocked.Reason != "abuse" {
		t.Fatalf("a change in the cluster was not seen: %+v", got.Spec)
	}
	if got, _ := accounts.Ensure(ctx, "u@example.com"); got.Spec.Blocked == nil {
		t.Fatal("Ensure answered from somewhere else than the cluster")
	}
}

func TestUpdateRetriesOnConflictAndFindsByLabel(t *testing.T) {
	api := newServer()
	accounts := Over(api)
	ctx := t.Context()
	acc, _ := accounts.Ensure(ctx, "u@example.com")
	other, _ := accounts.Ensure(ctx, "v@example.com")
	now := time.Date(2026, 10, 2, 11, 0, 0, 0, time.UTC)

	api.conflict = 2
	applied := 0
	got, err := accounts.Update(ctx, acc.Name, func(spec *billing.AccountSpec) error {
		applied++
		spec.StripeCustomerID, spec.MetronomeCustomerID = "cus_T1", "m-123"
		spec.PaymentMethod = &billing.PaymentMethods{Present: true, IDs: []string{"pm_T1"}, ReadAt: now}
		spec.Credit = &billing.AccountCredit{Exhausted: true, ExhaustedAt: &now, CheckedAt: &now}
		return nil
	})
	if err != nil || applied != 3 {
		t.Fatalf("Update: %v after %d tries", err, applied)
	}
	// The change was made to the spec it replaced: the other writer's
	// change is kept.
	if !got.Spec.Exempt || got.Spec.StripeCustomerID != "cus_T1" || !got.Spec.Credit.Exhausted || !got.Spec.Credit.ExhaustedAt.Equal(now) {
		t.Fatalf("after the conflicts: %+v", got.Spec)
	}
	obj := api.objects[acc.Name]
	if obj.GetLabels()[LabelMetronomeCustomer] != "m-123" || obj.GetLabels()[sessions.LabelOwner] == "" {
		t.Fatalf("labels %v", obj.GetLabels())
	}
	// exhausted false is written, not left out: the CRD requires it.
	if _, err := accounts.Update(ctx, acc.Name, func(spec *billing.AccountSpec) error {
		spec.Credit = &billing.AccountCredit{BalanceMicros: 5000000, CheckedAt: &now}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	credit := api.objects[acc.Name].Object["spec"].(map[string]any)["credit"].(map[string]any)
	if exhausted, ok := credit["exhausted"].(bool); !ok || exhausted {
		t.Fatalf("credit %v", credit)
	}
	if n, ok := credit["balanceMicros"].(int64); !ok || n != 5000000 {
		t.Fatalf("balanceMicros = %#v, want a whole number", credit["balanceMicros"])
	}
	if _, ok := credit["exhaustedAt"]; ok {
		t.Fatalf("exhaustedAt kept: %v", credit)
	}

	for name, find := range map[string]func() (billing.Account, error){
		"ByCustomer":          func() (billing.Account, error) { return accounts.ByCustomer(ctx, "cus_T1") },
		"ByMetronomeCustomer": func() (billing.Account, error) { return accounts.ByMetronomeCustomer(ctx, "m-123") },
		"ByPaymentMethod":     func() (billing.Account, error) { return accounts.ByPaymentMethod(ctx, "pm_T1") },
	} {
		if found, err := find(); err != nil || found.Name != acc.Name {
			t.Errorf("%s: %+v, %v", name, found.Name, err)
		}
	}
	for name, find := range map[string]func() (billing.Account, error){
		"ByCustomer":          func() (billing.Account, error) { return accounts.ByCustomer(ctx, "") },
		"ByMetronomeCustomer": func() (billing.Account, error) { return accounts.ByMetronomeCustomer(ctx, "m-999") },
		"ByPaymentMethod":     func() (billing.Account, error) { return accounts.ByPaymentMethod(ctx, "pm_T9") },
	} {
		if _, err := find(); !errors.Is(err, billing.ErrNotFound) {
			t.Errorf("%s of none: %v", name, err)
		}
	}
	if with, err := accounts.WithCustomer(ctx); err != nil || len(with) != 1 || with[0].Name != acc.Name {
		t.Errorf("WithCustomer: %+v, %v (the other is %s)", with, err, other.Name)
	}
	if _, err := accounts.Update(ctx, "acct-none", func(*billing.AccountSpec) error { return nil }); !errors.Is(err, billing.ErrNotFound) {
		t.Errorf("Update of none: %v", err)
	}
	refused := errors.New("no")
	if _, err := accounts.Update(ctx, acc.Name, func(*billing.AccountSpec) error { return refused }); !errors.Is(err, refused) {
		t.Errorf("a change that refuses: %v", err)
	}
	if err := accounts.Check(ctx); err != nil {
		t.Errorf("Check: %v", err)
	}
}
