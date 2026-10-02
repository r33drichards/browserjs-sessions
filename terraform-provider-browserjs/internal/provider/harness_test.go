package provider

import (
	"context"
	"fmt"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/r33drichards/computer-use/terraform-provider-browserjs/internal/fakeapi"
)

// The harness drives the provider the way Terraform does, over protocol 6
// in process, against the fake API. It needs no terraform or tofu binary.

const testToken = "bjs_test_0123456789abcdef"

// unknown stands for a value not known until apply, in configs and in what
// plain() returns.
const unknown = "<unknown>"

type cfg = map[string]any

type harness struct {
	t      *testing.T
	fake   *fakeapi.Server
	url    string
	server tfprotov6.ProviderServer
	schema *tfprotov6.GetProviderSchemaResponse
}

func newServer(t *testing.T) (tfprotov6.ProviderServer, *tfprotov6.GetProviderSchemaResponse) {
	t.Helper()
	p := &browserjsProvider{version: "test", pollEvery: func(int) time.Duration { return time.Millisecond }}
	server, err := providerserver.NewProtocol6WithError(p)()
	if err != nil {
		t.Fatal(err)
	}
	schema, err := server.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if errs := errorsOf(schema.Diagnostics); len(errs) > 0 {
		t.Fatalf("provider schema: %v", errs)
	}
	return server, schema
}

// newHarness starts a fake API and a provider configured for it.
func newHarness(t *testing.T) *harness {
	t.Helper()
	t.Setenv(envEndpoint, "")
	t.Setenv(envToken, "")
	fake := fakeapi.New(testToken)
	ts := httptest.NewServer(fake.Handler())
	t.Cleanup(ts.Close)
	h := &harness{t: t, fake: fake, url: ts.URL}
	h.server, h.schema = newServer(t)
	if diags := h.configure(cfg{"endpoint": ts.URL, "token": testToken}); len(errorsOf(diags)) > 0 {
		t.Fatalf("configure: %v", errorsOf(diags))
	}
	return h
}

func (h *harness) configure(c cfg) []*tfprotov6.Diagnostic {
	h.t.Helper()
	resp, err := h.server.ConfigureProvider(context.Background(), &tfprotov6.ConfigureProviderRequest{
		Config: h.dynamic(h.schema.Provider.ValueType(), toValue(h.t, h.schema.Provider.ValueType(), c)),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return resp.Diagnostics
}

func (h *harness) dynamic(typ tftypes.Type, v tftypes.Value) *tfprotov6.DynamicValue {
	h.t.Helper()
	dv, err := tfprotov6.NewDynamicValue(typ, v)
	if err != nil {
		h.t.Fatal(err)
	}
	return &dv
}

func (h *harness) value(typ tftypes.Type, dv *tfprotov6.DynamicValue) tftypes.Value {
	h.t.Helper()
	if dv == nil {
		return tftypes.NewValue(typ, nil)
	}
	v, err := dv.Unmarshal(typ)
	if err != nil {
		h.t.Fatal(err)
	}
	return v
}

func (h *harness) resourceSchema(name string) *tfprotov6.Schema {
	h.t.Helper()
	s, ok := h.schema.ResourceSchemas[name]
	if !ok {
		h.t.Fatalf("no resource %s", name)
	}
	return s
}

// planned is the answer to a plan.
type planned struct {
	state    tftypes.Value
	replace  []string // attributes that force replacement
	diags    []*tfprotov6.Diagnostic
	prior    tftypes.Value
	config   tftypes.Value
	resource string
}

// changes reports whether the plan differs from the prior state.
func (p planned) changes() bool { return !p.state.Equal(p.prior) }

func (h *harness) null(resource string) tftypes.Value {
	return tftypes.NewValue(h.resourceSchema(resource).ValueType(), nil)
}

// validate checks a resource's configuration, as `terraform validate` does.
func (h *harness) validate(resource string, c cfg) []*tfprotov6.Diagnostic {
	h.t.Helper()
	typ := h.resourceSchema(resource).ValueType()
	resp, err := h.server.ValidateResourceConfig(context.Background(), &tfprotov6.ValidateResourceConfigRequest{
		TypeName: resource, Config: h.dynamic(typ, toValue(h.t, typ, c)),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return resp.Diagnostics
}

// plan plans a configuration against a prior state. A nil cfg plans a
// destroy.
func (h *harness) plan(resource string, prior tftypes.Value, c cfg) planned {
	h.t.Helper()
	s := h.resourceSchema(resource)
	typ := s.ValueType()
	config := tftypes.NewValue(typ, nil)
	proposed := config
	if c != nil {
		config = toValue(h.t, typ, c)
		proposed = proposedNew(h.t, s, prior, config)
	}
	resp, err := h.server.PlanResourceChange(context.Background(), &tfprotov6.PlanResourceChangeRequest{
		TypeName:         resource,
		PriorState:       h.dynamic(typ, prior),
		ProposedNewState: h.dynamic(typ, proposed),
		Config:           h.dynamic(typ, config),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	out := planned{state: h.value(typ, resp.PlannedState), diags: resp.Diagnostics, prior: prior, config: config, resource: resource}
	for _, p := range resp.RequiresReplace {
		out.replace = append(out.replace, p.String())
	}
	return out
}

// proposedNew is what Terraform proposes: the configuration, with the prior
// state's value wherever a computed attribute is not configured.
func proposedNew(t *testing.T, s *tfprotov6.Schema, prior, config tftypes.Value) tftypes.Value {
	t.Helper()
	var cv, pv map[string]tftypes.Value
	if err := config.As(&cv); err != nil {
		t.Fatal(err)
	}
	if !prior.IsNull() {
		if err := prior.As(&pv); err != nil {
			t.Fatal(err)
		}
	}
	// A copy: As hands out the value's own map.
	out := map[string]tftypes.Value{}
	for name, v := range cv {
		out[name] = v
	}
	for _, a := range s.Block.Attributes {
		if a.Computed && cv[a.Name].IsNull() && pv != nil {
			out[a.Name] = pv[a.Name]
		}
	}
	return tftypes.NewValue(config.Type(), out)
}

// apply carries out a plan and returns the new state.
func (h *harness) apply(p planned) (tftypes.Value, []*tfprotov6.Diagnostic) {
	h.t.Helper()
	typ := h.resourceSchema(p.resource).ValueType()
	resp, err := h.server.ApplyResourceChange(context.Background(), &tfprotov6.ApplyResourceChangeRequest{
		TypeName:     p.resource,
		PriorState:   h.dynamic(typ, p.prior),
		PlannedState: h.dynamic(typ, p.state),
		Config:       h.dynamic(typ, p.config),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return h.value(typ, resp.NewState), resp.Diagnostics
}

// mustApply validates, plans and applies, and fails the test on any error.
func (h *harness) mustApply(resource string, prior tftypes.Value, c cfg) tftypes.Value {
	h.t.Helper()
	if c != nil {
		noErrors(h.t, "validate", h.validate(resource, c))
	}
	p := h.plan(resource, prior, c)
	noErrors(h.t, "plan", p.diags)
	state, diags := h.apply(p)
	noErrors(h.t, "apply", diags)
	if c != nil {
		consistent(h.t, p.state, state)
	}
	return state
}

// consistent is Terraform's check after apply: whatever the plan knew, the
// new state must have.
func consistent(t *testing.T, plan, state tftypes.Value) {
	t.Helper()
	var pv, sv map[string]tftypes.Value
	if err := plan.As(&pv); err != nil {
		t.Fatal(err)
	}
	if err := state.As(&sv); err != nil {
		t.Fatal(err)
	}
	for name, want := range pv {
		if want.IsFullyKnown() && !want.Equal(sv[name]) {
			t.Errorf("provider produced an inconsistent result after apply: %s was planned as %v and is %v", name, plain(want), plain(sv[name]))
		}
	}
}

// read refreshes a state.
func (h *harness) read(resource string, state tftypes.Value) (tftypes.Value, []*tfprotov6.Diagnostic) {
	h.t.Helper()
	typ := h.resourceSchema(resource).ValueType()
	resp, err := h.server.ReadResource(context.Background(), &tfprotov6.ReadResourceRequest{
		TypeName: resource, CurrentState: h.dynamic(typ, state),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return h.value(typ, resp.NewState), resp.Diagnostics
}

func (h *harness) mustRead(resource string, state tftypes.Value) tftypes.Value {
	h.t.Helper()
	out, diags := h.read(resource, state)
	noErrors(h.t, "read", diags)
	return out
}

// importState imports by ID and refreshes, as `terraform import` does.
func (h *harness) importState(resource, id string) (tftypes.Value, []*tfprotov6.Diagnostic) {
	h.t.Helper()
	typ := h.resourceSchema(resource).ValueType()
	resp, err := h.server.ImportResourceState(context.Background(), &tfprotov6.ImportResourceStateRequest{TypeName: resource, ID: id})
	if err != nil {
		h.t.Fatal(err)
	}
	if len(errorsOf(resp.Diagnostics)) > 0 {
		return tftypes.NewValue(typ, nil), resp.Diagnostics
	}
	if len(resp.ImportedResources) != 1 {
		h.t.Fatalf("imported %d resources", len(resp.ImportedResources))
	}
	return h.read(resource, h.value(typ, resp.ImportedResources[0].State))
}

// data reads a data source.
func (h *harness) data(name string, c cfg) (map[string]any, []*tfprotov6.Diagnostic) {
	h.t.Helper()
	s, ok := h.schema.DataSourceSchemas[name]
	if !ok {
		h.t.Fatalf("no data source %s", name)
	}
	typ := s.ValueType()
	config := h.dynamic(typ, toValue(h.t, typ, c))
	v, err := h.server.ValidateDataResourceConfig(context.Background(), &tfprotov6.ValidateDataResourceConfigRequest{TypeName: name, Config: config})
	if err != nil {
		h.t.Fatal(err)
	}
	if len(errorsOf(v.Diagnostics)) > 0 {
		return nil, v.Diagnostics
	}
	resp, err := h.server.ReadDataSource(context.Background(), &tfprotov6.ReadDataSourceRequest{TypeName: name, Config: config})
	if err != nil {
		h.t.Fatal(err)
	}
	if len(errorsOf(resp.Diagnostics)) > 0 {
		return nil, resp.Diagnostics
	}
	return attrs(h.value(typ, resp.State)), resp.Diagnostics
}

// blockNames are the nested blocks of the provider's schemas.
var blockNames = map[string]bool{"rule": true, "constraint": true}

// toValue builds a value of type typ from plain Go: maps for objects, slices
// for lists and sets, nil for null, and the unknown constant.
func toValue(t *testing.T, typ tftypes.Type, v any) tftypes.Value {
	t.Helper()
	if v == nil {
		return tftypes.NewValue(typ, nil)
	}
	if v == unknown {
		return tftypes.NewValue(typ, tftypes.UnknownValue)
	}
	switch tt := typ.(type) {
	case tftypes.Object:
		m, ok := v.(cfg)
		if !ok {
			t.Fatalf("want a cfg for %s, got %T", typ, v)
		}
		for k := range m {
			if _, ok := tt.AttributeTypes[k]; !ok {
				t.Fatalf("no attribute %q in %s", k, typ)
			}
		}
		out := map[string]tftypes.Value{}
		for k, at := range tt.AttributeTypes {
			out[k] = toValue(t, at, m[k])
			// A list of blocks that is left out is empty, not null.
			if _, isObj := elementType(at).(tftypes.Object); isObj && m[k] == nil && blockNames[k] {
				out[k] = tftypes.NewValue(at, []tftypes.Value{})
			}
		}
		return tftypes.NewValue(typ, out)
	case tftypes.List, tftypes.Set:
		items, ok := v.([]any)
		if !ok {
			t.Fatalf("want a []any for %s, got %T", typ, v)
		}
		out := make([]tftypes.Value, len(items))
		for i, item := range items {
			out[i] = toValue(t, elementType(typ), item)
		}
		return tftypes.NewValue(typ, out)
	}
	switch x := v.(type) {
	case int:
		return tftypes.NewValue(typ, big.NewFloat(float64(x)))
	case float64:
		return tftypes.NewValue(typ, big.NewFloat(x))
	}
	return tftypes.NewValue(typ, v)
}

func elementType(typ tftypes.Type) tftypes.Type {
	switch tt := typ.(type) {
	case tftypes.List:
		return tt.ElementType
	case tftypes.Set:
		return tt.ElementType
	}
	return nil
}

// plain turns a value into plain Go for assertions.
func plain(v tftypes.Value) any {
	if !v.IsKnown() {
		return unknown
	}
	if v.IsNull() {
		return nil
	}
	switch {
	case v.Type().Is(tftypes.String):
		var s string
		_ = v.As(&s)
		return s
	case v.Type().Is(tftypes.Bool):
		var b bool
		_ = v.As(&b)
		return b
	case v.Type().Is(tftypes.Number):
		var f big.Float
		_ = v.As(&f)
		out, _ := f.Float64()
		return out
	}
	var m map[string]tftypes.Value
	if err := v.As(&m); err == nil {
		out := map[string]any{}
		for k, e := range m {
			out[k] = plain(e)
		}
		return out
	}
	var l []tftypes.Value
	_ = v.As(&l)
	out := make([]any, len(l))
	for i, e := range l {
		out[i] = plain(e)
	}
	return out
}

// attrs is a state's attributes as plain Go.
func attrs(state tftypes.Value) map[string]any {
	m, _ := plain(state).(map[string]any)
	return m
}

func str(state tftypes.Value, name string) string {
	s, _ := attrs(state)[name].(string)
	return s
}

func errorsOf(diags []*tfprotov6.Diagnostic) []string {
	return texts(diags, tfprotov6.DiagnosticSeverityError)
}

func warningsOf(diags []*tfprotov6.Diagnostic) []string {
	return texts(diags, tfprotov6.DiagnosticSeverityWarning)
}

func texts(diags []*tfprotov6.Diagnostic, severity tfprotov6.DiagnosticSeverity) []string {
	var out []string
	for _, d := range diags {
		if d.Severity == severity {
			at := ""
			if d.Attribute != nil {
				at = d.Attribute.String() + ": "
			}
			out = append(out, fmt.Sprintf("%s%s: %s", at, d.Summary, d.Detail))
		}
	}
	return out
}

func noErrors(t *testing.T, doing string, diags []*tfprotov6.Diagnostic) {
	t.Helper()
	if errs := errorsOf(diags); len(errs) > 0 {
		t.Fatalf("%s: %s", doing, strings.Join(errs, "\n"))
	}
}

// wantError fails unless one error contains every part.
func wantError(t *testing.T, diags []*tfprotov6.Diagnostic, parts ...string) {
	t.Helper()
	wantText(t, "error", errorsOf(diags), parts)
}

func wantWarning(t *testing.T, diags []*tfprotov6.Diagnostic, parts ...string) {
	t.Helper()
	wantText(t, "warning", warningsOf(diags), parts)
}

func wantText(t *testing.T, kind string, have []string, parts []string) {
	t.Helper()
next:
	for _, text := range have {
		for _, p := range parts {
			if !strings.Contains(text, p) {
				continue next
			}
		}
		return
	}
	t.Fatalf("no %s containing %q; got %q", kind, parts, have)
}
