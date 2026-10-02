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
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/r33drichards/browserjs-sessions/backend/internal/billing"
	"github.com/r33drichards/browserjs-sessions/backend/internal/sessions"
)

const group, version = "browserjs.dev", "v1alpha1"

var AccountGVR = schema.GroupVersionResource{Group: group, Version: version, Resource: "accounts"}

// Mode is which of Stripe's modes, and with it which of Metronome's
// environments, the backend is in (STRIPE_MODE). An Account keeps what it
// has from each provider apart for each mode (crd-account.yaml):
// spec.stripe.<test|live> and spec.metronome.<sandbox|production>. Here
// one pair is read and written and the other is left as it is, so
// switching mode clears nothing. billing.AccountSpec is the Account as
// seen in that mode: its Stripe and Metronome fields are the mode's.
type Mode string

const (
	Test Mode = "test" // with Metronome's sandbox; also while STRIPE_MODE is unset
	Live Mode = "live" // with Metronome's production environment
)

// ModeOf is the Mode of a STRIPE_MODE.
func ModeOf(stripeMode string) Mode {
	if stripeMode == string(Live) {
		return Live
	}
	return Test
}

// environment is the Metronome environment that goes with the mode.
func (m Mode) environment() string {
	if m == Live {
		return "production"
	}
	return "sandbox"
}

// label is on an Account: its Metronome customer in this mode's
// environment, which is all Metronome's alert names.
func (m Mode) label() string { return "browserjs.dev/metronome-customer-" + m.environment() }

// stripeState is spec.stripe.<mode>, and metronomeState
// spec.metronome.<environment>.
type stripeState struct {
	CustomerID    string                     `json:"customerId,omitempty"`
	PaymentMethod *billing.PaymentMethods    `json:"paymentMethod,omitempty"`
	SignupCredit  *billing.SignupCredit      `json:"signupCredit,omitempty"`
	Subscription  *billing.SubscriptionState `json:"subscription,omitempty"`
	AutoRecharge  *billing.AutoRecharge      `json:"autoRecharge,omitempty"`
}

type metronomeState struct {
	CustomerID string                 `json:"customerId,omitempty"`
	Credit     *billing.AccountCredit `json:"credit,omitempty"`
}

// common is the part of spec that is not per mode.
type common struct {
	Owner           string           `json:"owner"`
	OwnerHash       string           `json:"ownerHash"`
	Exempt          bool             `json:"exempt,omitempty"`
	Blocked         *billing.Blocked `json:"blocked,omitempty"`
	TermsAcceptedAt *time.Time       `json:"termsAcceptedAt,omitempty"`
	TermsVersion    string           `json:"termsVersion,omitempty"`
	DeletedAt       *time.Time       `json:"deletedAt,omitempty"`
}

// into reads a part of an object (its spec, or something inside it) as v.
func into(obj *unstructured.Unstructured, v any, fields ...string) error {
	part, found, err := unstructured.NestedMap(obj.Object, fields...)
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

// accountFrom reads an Account as it is in mode.
func accountFrom(obj *unstructured.Unstructured, mode Mode) (billing.Account, error) {
	acc := billing.Account{Name: obj.GetName(), Created: obj.GetCreationTimestamp().Time}
	var c common
	var st stripeState
	var me metronomeState
	for _, part := range []struct {
		into   any
		fields []string
	}{{&c, []string{"spec"}}, {&st, []string{"spec", "stripe", string(mode)}}, {&me, []string{"spec", "metronome", mode.environment()}}} {
		if err := into(obj, part.into, part.fields...); err != nil {
			return billing.Account{}, fmt.Errorf("account %s: %w", obj.GetName(), err)
		}
	}
	acc.Spec = billing.AccountSpec{
		Owner: c.Owner, OwnerHash: c.OwnerHash, Exempt: c.Exempt, Blocked: c.Blocked,
		TermsAcceptedAt: c.TermsAcceptedAt, TermsVersion: c.TermsVersion, DeletedAt: c.DeletedAt,
		StripeCustomerID: st.CustomerID, PaymentMethod: st.PaymentMethod, SignupCredit: st.SignupCredit,
		Subscription: st.Subscription, AutoRecharge: st.AutoRecharge,
		MetronomeCustomerID: me.CustomerID, Credit: me.Credit,
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

// setSpec writes spec into an Account as mode's: the part that is not per
// mode, the mode's Stripe state and its environment's Metronome state,
// and the label its Metronome customer is found by. The other mode's
// state, and the rest of the object, are left as they are.
func setSpec(obj *unstructured.Unstructured, spec billing.AccountSpec, mode Mode) error {
	old, _, _ := unstructured.NestedMap(obj.Object, "spec")
	next, err := object(common{Owner: spec.Owner, OwnerHash: spec.OwnerHash, Exempt: spec.Exempt, Blocked: spec.Blocked,
		TermsAcceptedAt: spec.TermsAcceptedAt, TermsVersion: spec.TermsVersion, DeletedAt: spec.DeletedAt})
	if err != nil {
		return err
	}
	for _, part := range []struct {
		provider, mode string
		state          any
	}{
		{"stripe", string(mode), stripeState{spec.StripeCustomerID, spec.PaymentMethod, spec.SignupCredit, spec.Subscription, spec.AutoRecharge}},
		{"metronome", mode.environment(), metronomeState{spec.MetronomeCustomerID, spec.Credit}},
	} {
		state, err := object(part.state)
		if err != nil {
			return err
		}
		modes, _ := old[part.provider].(map[string]any)
		_, had := modes[part.mode]
		if len(state) == 0 && !had {
			if modes != nil {
				next[part.provider] = modes
			}
			continue // nothing known in this mode yet
		}
		if modes == nil {
			modes = map[string]any{}
		}
		modes[part.mode] = state
		next[part.provider] = modes
	}
	obj.Object["spec"] = next
	if spec.MetronomeCustomerID != "" {
		labels := obj.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[mode.label()] = spec.MetronomeCustomerID
		obj.SetLabels(labels)
	}
	return nil
}
