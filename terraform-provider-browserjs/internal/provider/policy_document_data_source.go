package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// policyOperations are the operations of version 1 of the JSON format
// (docs/contracts/policy/json-policy.schema.json).
var policyOperations = []string{"click", "evaluate", "navigate", "press", "screenshot", "select", "setContent", "setViewport", "type", "url", "wait"}

var _ datasource.DataSource = (*policyDocumentDataSource)(nil)

func newPolicyDocumentDataSource() datasource.DataSource { return &policyDocumentDataSource{} }

type policyDocumentDataSource struct{}

type policyDocumentModel struct {
	Description     types.String      `tfsdk:"description"`
	AllowOperations types.Set         `tfsdk:"allow_operations"`
	DenyOperations  types.Set         `tfsdk:"deny_operations"`
	Rules           []policyRuleModel `tfsdk:"rule"`
	JSON            types.String      `tfsdk:"json"`
}

type policyRuleModel struct {
	Operation   types.String            `tfsdk:"operation"`
	Constraints []policyConstraintModel `tfsdk:"constraint"`
}

type policyConstraintModel struct {
	Parameter       types.String `tfsdk:"parameter"`
	Min             types.Number `tfsdk:"min"`
	Max             types.Number `tfsdk:"max"`
	MaxLength       types.Int64  `tfsdk:"max_length"`
	Pattern         types.String `tfsdk:"pattern"`
	Allowed         types.List   `tfsdk:"allowed"`
	AllowedNumbers  types.List   `tfsdk:"allowed_numbers"`
	AllowedBooleans types.List   `tfsdk:"allowed_booleans"`
	Hosts           types.Set    `tfsdk:"hosts"`
	Schemes         types.Set    `tfsdk:"schemes"`
}

func (d *policyDocumentDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_policy_document"
}

func (d *policyDocumentDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Builds a policy in the JSON format from HCL, for the `json` argument of `browserjs_session_policy`. It calls nothing; it only renders JSON.\n\n" +
			"A policy denies by default. An operation is allowed if it is not in `deny_operations` and is either in `allow_operations` or satisfies one of the `rule` blocks. " +
			"A `browser_execute` call is allowed only if every operation in it is.\n\n" +
			"The operations are `click`, `evaluate`, `navigate`, `press`, `screenshot`, `select`, `setContent`, `setViewport`, `type`, `url` and `wait`.",
		Attributes: map[string]schema.Attribute{
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "What the policy is for; shown in the UI.",
				Validators:          []validator.String{stringvalidator.LengthAtMost(1024)},
			},
			"allow_operations": schema.SetAttribute{
				ElementType:         types.StringType,
				Optional:            true,
				MarkdownDescription: "Operations allowed with any parameters. `\"*\"` means every operation.",
				Validators: []validator.Set{
					setvalidator.ValueStringsAre(stringvalidator.OneOf(append([]string{"*"}, policyOperations...)...)),
				},
			},
			"deny_operations": schema.SetAttribute{
				ElementType:         types.StringType,
				Optional:            true,
				MarkdownDescription: "Operations refused whatever is allowed.",
				Validators: []validator.Set{
					setvalidator.ValueStringsAre(stringvalidator.OneOf(policyOperations...)),
				},
			},
			"json": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The rendered policy, keys in a stable order.",
			},
		},
		Blocks: map[string]schema.Block{
			"rule": schema.ListNestedBlock{
				MarkdownDescription: "An operation allowed when every one of its constraints passes. A rule with no constraint allows the operation as `allow_operations` does. At most 64.",
				Validators:          []validator.List{listvalidator.SizeAtMost(64)},
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"operation": schema.StringAttribute{
							Required:            true,
							MarkdownDescription: "The operation the rule allows.",
							Validators:          []validator.String{stringvalidator.OneOf(policyOperations...)},
						},
					},
					Blocks: map[string]schema.Block{
						"constraint": schema.ListNestedBlock{
							MarkdownDescription: "A condition on one parameter of the operation. A constrained parameter that is absent from the call fails. At least one condition is required. At most 16 per rule.",
							Validators:          []validator.List{listvalidator.SizeAtMost(16)},
							NestedObject: schema.NestedBlockObject{
								Attributes: map[string]schema.Attribute{
									"parameter": schema.StringAttribute{
										Required:            true,
										MarkdownDescription: "The name of the parameter, such as `url` for `navigate` or `text` for `type`.",
										Validators: []validator.String{
											stringvalidator.RegexMatches(parameterName, "must be a parameter name: a letter or underscore, then letters, digits and underscores, at most 64 characters"),
										},
									},
									"min": schema.NumberAttribute{Optional: true, MarkdownDescription: "The parameter is a number, at least this."},
									"max": schema.NumberAttribute{Optional: true, MarkdownDescription: "The parameter is a number, at most this."},
									"max_length": schema.Int64Attribute{
										Optional:            true,
										MarkdownDescription: "The parameter is a string of at most this many characters.",
										Validators:          []validator.Int64{int64validator.AtLeast(0)},
									},
									"pattern": schema.StringAttribute{
										Optional:            true,
										MarkdownDescription: "The parameter is a string matching this regular expression (RE2 syntax, unanchored unless it anchors itself).",
										Validators:          []validator.String{stringvalidator.LengthAtMost(1024)},
									},
									"allowed": schema.ListAttribute{
										ElementType:         types.StringType,
										Optional:            true,
										MarkdownDescription: "The parameter is one of these strings.",
										Validators:          []validator.List{listvalidator.SizeAtLeast(1), listvalidator.UniqueValues()},
									},
									"allowed_numbers": schema.ListAttribute{
										ElementType:         types.NumberType,
										Optional:            true,
										MarkdownDescription: "The parameter is one of these numbers. Rendered into the same `allowed` list as `allowed` and `allowed_booleans`.",
										Validators:          []validator.List{listvalidator.SizeAtLeast(1), listvalidator.UniqueValues()},
									},
									"allowed_booleans": schema.ListAttribute{
										ElementType:         types.BoolType,
										Optional:            true,
										MarkdownDescription: "The parameter is one of these booleans. Rendered into the same `allowed` list as `allowed` and `allowed_numbers`.",
										Validators:          []validator.List{listvalidator.SizeAtLeast(1), listvalidator.UniqueValues()},
									},
									"hosts": schema.SetAttribute{
										ElementType:         types.StringType,
										Optional:            true,
										MarkdownDescription: "The parameter is a URL whose host is one of these: an exact name, or `*.name` for any subdomain of `name` (not `name` itself).",
										Validators: []validator.Set{
											setvalidator.SizeBetween(1, 256),
											setvalidator.ValueStringsAre(stringvalidator.LengthAtMost(255), stringvalidator.RegexMatches(hostPattern, "must be a lower-case host name, or *.name")),
										},
									},
									"schemes": schema.SetAttribute{
										ElementType:         types.StringType,
										Optional:            true,
										MarkdownDescription: "The parameter is a URL with one of these schemes (`http`, `https`). With `hosts` and without this, both are accepted.",
										Validators: []validator.Set{
											setvalidator.SizeAtLeast(1),
											setvalidator.ValueStringsAre(stringvalidator.OneOf("http", "https")),
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// The documents below are the JSON format; field order is key order.

type policyDocument struct {
	Version     int          `json:"version"`
	Description string       `json:"description,omitempty"`
	Allow       *policyAllow `json:"allow,omitempty"`
	Deny        *policyDeny  `json:"deny,omitempty"`
}

type policyAllow struct {
	Operations []string     `json:"operations,omitempty"`
	Rules      []policyRule `json:"rules,omitempty"`
}

type policyDeny struct {
	Operations []string `json:"operations"`
}

type policyRule struct {
	Operation string `json:"operation"`
	// A map, so encoding/json writes the parameters in sorted order.
	Constraints map[string]policyConstraint `json:"constraints,omitempty"`
}

type policyConstraint struct {
	Min       json.Number `json:"min,omitempty"`
	Max       json.Number `json:"max,omitempty"`
	MaxLength *int64      `json:"max_length,omitempty"`
	Pattern   *string     `json:"pattern,omitempty"`
	Allowed   []any       `json:"allowed,omitempty"`
	Hosts     []string    `json:"hosts,omitempty"`
	Schemes   []string    `json:"schemes,omitempty"`
}

func (d *policyDocumentDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m policyDocumentModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}

	sorted := func(s types.Set) []string {
		var out []string
		resp.Diagnostics.Append(s.ElementsAs(ctx, &out, false)...)
		sort.Strings(out)
		return out
	}

	doc := policyDocument{Version: 1, Description: m.Description.ValueString()}
	allow := policyAllow{Operations: sorted(m.AllowOperations)}
	for i, rm := range m.Rules {
		rule := policyRule{Operation: rm.Operation.ValueString()}
		for j, cm := range rm.Constraints {
			at := path.Root("rule").AtListIndex(i).AtName("constraint").AtListIndex(j)
			name := cm.Parameter.ValueString()
			if _, dup := rule.Constraints[name]; dup {
				resp.Diagnostics.AddAttributeError(at.AtName("parameter"), "Parameter constrained twice",
					fmt.Sprintf("This rule already has a constraint on %q. Put all its conditions in one constraint block.", name))
				continue
			}
			c := policyConstraint{
				Min:     number(cm.Min),
				Max:     number(cm.Max),
				Hosts:   sorted(cm.Hosts),
				Schemes: sorted(cm.Schemes),
			}
			if !cm.MaxLength.IsNull() {
				c.MaxLength = cm.MaxLength.ValueInt64Pointer()
			}
			if !cm.Pattern.IsNull() {
				c.Pattern = cm.Pattern.ValueStringPointer()
			}
			for _, v := range cm.Allowed.Elements() {
				c.Allowed = append(c.Allowed, v.(types.String).ValueString())
			}
			for _, v := range cm.AllowedNumbers.Elements() {
				c.Allowed = append(c.Allowed, number(v.(types.Number)))
			}
			for _, v := range cm.AllowedBooleans.Elements() {
				c.Allowed = append(c.Allowed, v.(types.Bool).ValueBool())
			}
			if len(c.Allowed) > 256 {
				resp.Diagnostics.AddAttributeError(at, "Too many allowed values", "A constraint allows at most 256 values.")
			}
			if c.Min == "" && c.Max == "" && c.MaxLength == nil && c.Pattern == nil && c.Allowed == nil && c.Hosts == nil && c.Schemes == nil {
				resp.Diagnostics.AddAttributeError(at, "Empty constraint",
					fmt.Sprintf("The constraint on %q has no condition. Give it at least one of min, max, max_length, pattern, allowed, allowed_numbers, allowed_booleans, hosts and schemes.", name))
			}
			if rule.Constraints == nil {
				rule.Constraints = map[string]policyConstraint{}
			}
			rule.Constraints[name] = c
		}
		allow.Rules = append(allow.Rules, rule)
	}
	if allow.Operations != nil || allow.Rules != nil {
		doc.Allow = &allow
	}
	if deny := sorted(m.DenyOperations); deny != nil {
		doc.Deny = &policyDeny{Operations: deny}
	}
	if resp.Diagnostics.HasError() {
		return
	}

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		resp.Diagnostics.AddError("Could not render the policy", err.Error())
		return
	}
	m.JSON = types.StringValue(string(bytes.TrimRight(out.Bytes(), "\n")))
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// number renders a number exactly, without an exponent or a trailing ".0".
func number(n types.Number) json.Number {
	if n.IsNull() || n.IsUnknown() {
		return ""
	}
	f := n.ValueBigFloat()
	if i, acc := f.Int(nil); acc == big.Exact {
		return json.Number(i.String())
	}
	return json.Number(f.Text('g', -1))
}
