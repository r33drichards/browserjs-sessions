package provider

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/r33drichards/browserjs-sessions/terraform-provider-browserjs/internal/client"
)

const policyWriteTimeout = 2 * time.Minute

var (
	_ resource.Resource                     = (*sessionPolicyResource)(nil)
	_ resource.ResourceWithConfigure        = (*sessionPolicyResource)(nil)
	_ resource.ResourceWithConfigValidators = (*sessionPolicyResource)(nil)
	_ resource.ResourceWithModifyPlan       = (*sessionPolicyResource)(nil)
	_ resource.ResourceWithImportState      = (*sessionPolicyResource)(nil)
)

func newSessionPolicyResource() resource.Resource { return &sessionPolicyResource{} }

type sessionPolicyResource struct{ data *providerData }

type sessionPolicyModel struct {
	ID           types.String         `tfsdk:"id"`
	SessionID    types.String         `tfsdk:"session_id"`
	JSON         jsontypes.Normalized `tfsdk:"json"`
	Rego         types.String         `tfsdk:"rego"`
	ManagedURL   types.String         `tfsdk:"managed_url"`
	WaitForReady types.Bool           `tfsdk:"wait_for_ready"`
	Version      types.Int64          `tfsdk:"version"`
	Hash         types.String         `tfsdk:"hash"`
	CompiledRego types.String         `tfsdk:"compiled_rego"`
	State        types.String         `tfsdk:"state"`
	Timeouts     timeouts.Value       `tfsdk:"timeouts"`
}

// source is the policy's kind and text, and whether they are known yet.
func (m *sessionPolicyModel) source() (kind, source string, known bool) {
	switch {
	case !m.JSON.IsNull():
		return "json", m.JSON.ValueString(), !m.JSON.IsUnknown()
	case !m.Rego.IsNull():
		return "rego", m.Rego.ValueString(), !m.Rego.IsUnknown()
	}
	return "", "", false
}

// sourceAttr is the attribute a policy of this kind is written in.
func sourceAttr(kind string) path.Path {
	if kind == "rego" {
		return path.Root("rego")
	}
	return path.Root("json")
}

// setComputed takes what the server decides: version, hash, module, state.
func (m *sessionPolicyModel) setComputed(p *client.Policy) {
	m.Version = types.Int64Value(p.Version)
	m.Hash = types.StringValue(p.Hash)
	m.CompiledRego = types.StringValue(p.Rego)
	m.State = types.StringValue(p.State)
}

// setAll takes everything from the server, for a refresh.
func (m *sessionPolicyModel) setAll(id string, p *client.Policy) {
	m.ID = types.StringValue(id)
	m.SessionID = types.StringValue(id)
	if p.Kind == "rego" {
		m.JSON = jsontypes.NewNormalizedNull()
		m.Rego = types.StringValue(p.Source)
	} else {
		m.JSON = jsontypes.NewNormalizedValue(p.Source)
		m.Rego = types.StringNull()
	}
	// A policy somebody took back in the UI is no longer managed as code.
	// Reading its URL as empty makes the next plan an update that takes the
	// policy back.
	m.ManagedURL = types.StringValue("")
	if p.Management != nil && p.Management.Mode == client.ModeIaC {
		m.ManagedURL = types.StringValue(p.Management.ManagedURL)
	}
	m.setComputed(p)
}

func (r *sessionPolicyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_session_policy"
}

func (r *sessionPolicyResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The policy of one session, managed as code: what an agent connected over MCP may ask the session's browser to do.\n\n" +
			"Creating this resource puts the session's policy in managed-as-code mode: the UI shows it read-only with a link to `managed_url`. " +
			"Destroying it resets the session to the unrestricted policy, editable in the UI again.\n\n" +
			"A change to the policy is applied in place and restarts nothing; it never replaces the session. " +
			"The policy is checked at plan time, and errors are reported with their line and column.\n\n" +
			"If somebody chooses \"Manage here instead\" in the UI, the next plan shows `managed_url` changing from empty, and the apply takes the policy back.\n\n" +
			"The API token needs the scopes `policies:read` and `policies:write`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Equal to `session_id`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"session_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The session whose policy this is. Changing it forces a new resource (the old session's policy is reset).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:          []validator.String{stringvalidator.RegexMatches(sessionID, "must be a session ID such as s-ab2cd")},
			},
			"json": schema.StringAttribute{
				CustomType:          jsontypes.NormalizedType{},
				Optional:            true,
				MarkdownDescription: "A policy in the JSON format (see `browserjs_policy_document`, or `jsonencode`). Compared as parsed JSON, so formatting and key order are not a change. Exactly one of `json` and `rego` is required.",
			},
			"rego": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "A policy as a Rego module of package `browserjs.policy`. Exactly one of `json` and `rego` is required.",
				Validators:          []validator.String{stringvalidator.LengthBetween(1, 65536)},
			},
			"managed_url": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The `https` URL of where this configuration lives, such as the repository directory. The UI links to it from the read-only policy.",
				Validators:          []validator.String{httpsURL{}},
			},
			"wait_for_ready": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "Whether apply waits for the policy to be in force. Defaults to `true`.",
			},
			"version": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "The policy's version; it rises with each change.",
			},
			"hash": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The hash of the policy in force.",
			},
			"compiled_rego": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The Rego module in force. For `json`, the module generated from it.",
			},
			"state": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`ready`, `loading` or `invalid`.",
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx, timeouts.Opts{
				Create:            true,
				Update:            true,
				CreateDescription: "How long to wait for the policy to be in force. A duration such as `5m`; the default is 2 minutes.",
				UpdateDescription: "How long to wait for a changed policy to be in force. A duration such as `5m`; the default is 2 minutes.",
			}),
		},
	}
}

// httpsURL checks managed_url offline.
type httpsURL struct{}

func (httpsURL) Description(context.Context) string         { return "must be an https URL" }
func (httpsURL) MarkdownDescription(context.Context) string { return "must be an `https` URL" }

func (httpsURL) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	u, err := url.Parse(req.ConfigValue.ValueString())
	if err != nil || u.Scheme != "https" || u.Host == "" {
		resp.Diagnostics.AddAttributeError(req.Path, "Not an https URL",
			fmt.Sprintf("managed_url must be an https URL of where this configuration lives, got %q.", req.ConfigValue.ValueString()))
	}
}

func (r *sessionPolicyResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.ExactlyOneOf(path.MatchRoot("json"), path.MatchRoot("rego")),
	}
}

func (r *sessionPolicyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = configured(req.ProviderData, &resp.Diagnostics)
}

// sameJSON reports whether a and b are both known JSON texts with the same
// meaning.
func sameJSON(ctx context.Context, a, b jsontypes.Normalized, diags *diag.Diagnostics) bool {
	if a.IsNull() || b.IsNull() || a.IsUnknown() || b.IsUnknown() {
		return false
	}
	eq, d := a.StringSemanticEquals(ctx, b)
	diags.Append(d...)
	return eq
}

// sameSource reports whether a and b ask for the same policy under the same
// management, JSON compared as parsed.
func sameSource(ctx context.Context, a, b *sessionPolicyModel, diags *diag.Diagnostics) bool {
	if a.JSON.IsUnknown() || b.JSON.IsUnknown() || a.Rego.IsUnknown() || b.Rego.IsUnknown() || a.ManagedURL.IsUnknown() {
		return false
	}
	if a.JSON.IsNull() != b.JSON.IsNull() {
		return false
	}
	if !a.JSON.IsNull() && !sameJSON(ctx, a.JSON, b.JSON, diags) {
		return false
	}
	return a.Rego.Equal(b.Rego) && a.ManagedURL.Equal(b.ManagedURL)
}

func (r *sessionPolicyResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // destroy
	}
	var plan sessionPolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// What the server decides changes only when the policy does. Without
	// this, a change to wait_for_ready alone would plan them all as unknown.
	if !req.State.Raw.IsNull() {
		var state sessionPolicyModel
		resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
		if resp.Diagnostics.HasError() {
			return
		}
		// The same JSON written differently is not a change: plan the text
		// already in the state, which Terraform accepts in place of the
		// configuration's for exactly this purpose.
		if sameJSON(ctx, plan.JSON, state.JSON, &resp.Diagnostics) && !plan.JSON.Equal(state.JSON) {
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("json"), state.JSON)...)
		}
		if sameSource(ctx, &plan, &state, &resp.Diagnostics) {
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("version"), state.Version)...)
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("hash"), state.Hash)...)
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("compiled_rego"), state.CompiledRego)...)
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("state"), state.State)...)
		}
	}

	kind, source, known := plan.source()
	if !known || r.data == nil {
		return
	}
	v, err := r.data.client.ValidatePolicy(ctx, kind, source)
	if client.StatusOf(err) == 503 {
		resp.Diagnostics.AddAttributeWarning(sourceAttr(kind), "The policy was not checked at plan time",
			err.Error()+"\n\nThe apply checks it again before anything is saved.")
		return
	}
	if err != nil {
		apiError(&resp.Diagnostics, "check the policy", err)
		return
	}
	policyDiagnostics(&resp.Diagnostics, kind, v.Errors, v.Warnings)
	if !v.OK && len(v.Errors) == 0 {
		resp.Diagnostics.AddAttributeError(sourceAttr(kind), "Invalid policy", "The API says the policy is not valid and gave no reason.")
	}
}

// policyDiagnostics turns the API's errors and warnings about a policy into
// diagnostics on the attribute it was written in, each with its position.
func policyDiagnostics(diags *diag.Diagnostics, kind string, errs, warns []client.Diagnostic) {
	attr := sourceAttr(kind)
	for _, d := range errs {
		diags.AddAttributeError(attr, "Invalid policy", diagnosticText(kind, d))
	}
	for _, d := range warns {
		diags.AddAttributeWarning(attr, "Policy warning", diagnosticText(kind, d))
	}
}

func diagnosticText(kind string, d client.Diagnostic) string {
	var b strings.Builder
	if d.Row > 0 {
		fmt.Fprintf(&b, "%s line %d, column %d: ", kind, d.Row, d.Col)
	}
	b.WriteString(d.Message)
	if d.Code != "" {
		fmt.Fprintf(&b, " (%s)", d.Code)
	}
	return b.String()
}

// write saves the plan's policy in iac mode and, when asked, waits for it to
// be in force. It returns nil after adding an error.
func (r *sessionPolicyResource) write(ctx context.Context, plan *sessionPolicyModel, timeout time.Duration, diags *diag.Diagnostics) *client.Policy {
	id := plan.SessionID.ValueString()
	kind, source, _ := plan.source()
	p, loading, err := r.data.client.PutPolicy(ctx, id, client.PolicyInput{
		Kind:       kind,
		Source:     source,
		Management: &client.Management{Mode: client.ModeIaC, ManagedURL: plan.ManagedURL.ValueString()},
	})
	if err != nil {
		r.writeError(ctx, id, kind, err, diags)
		return nil
	}
	policyDiagnostics(diags, kind, nil, p.Warnings)
	if !loading || !plan.WaitForReady.ValueBool() {
		return p
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err = poll(ctx, r.data.pollEvery, func() (bool, error) {
		got, err := r.data.client.GetPolicy(ctx, id)
		if err != nil {
			return false, err
		}
		p = got
		return p.State != client.StateLoading, nil
	})
	switch {
	case err != nil && ctx.Err() != nil:
		diags.AddError("The policy was not in force in time",
			fmt.Sprintf("The policy of session %s was saved (version %d), and after %s it is still loading. Apply again to wait longer, or set wait_for_ready = false.", id, p.Version, timeout))
		return nil
	case err != nil:
		apiError(diags, "read the policy of session "+id, err)
		return nil
	case p.State == client.StateInvalid:
		policyDiagnostics(diags, kind, p.Errors, nil)
		if len(p.Errors) == 0 {
			diags.AddAttributeError(sourceAttr(kind), "Invalid policy", "The policy was saved and did not compile; the API gave no reason.")
		}
		diags.AddError("The policy is not in force",
			fmt.Sprintf("The policy of session %s was saved (version %d) and did not compile. The policy before it, if there was one, is still in force.", id, p.Version))
		return nil
	case p.State != client.StateReady:
		diags.AddError("The policy is not in force", fmt.Sprintf("The policy of session %s is %q.", id, p.State))
		return nil
	}
	return p
}

func (r *sessionPolicyResource) writeError(ctx context.Context, id, kind string, err error, diags *diag.Diagnostics) {
	e, _ := err.(*client.APIError)
	switch client.StatusOf(err) {
	case 404:
		diags.AddAttributeError(path.Root("session_id"), "No such session",
			fmt.Sprintf("Session %s does not exist, or is not the API token owner's.", id))
	case 409:
		// Either the session predates policies, or this credential may not
		// write in the policy's mode. Only the first makes a read a 409 too.
		if _, readErr := r.data.client.GetPolicy(ctx, id); client.StatusOf(readErr) == 409 {
			unsupported(diags, id, err)
			return
		}
		detail := err.Error()
		if e.ManagedURL != "" {
			detail += "\n\nIt is managed at " + e.ManagedURL + "."
		}
		diags.AddError("The policy may not be written", detail)
	case 422:
		policyDiagnostics(diags, kind, e.Errors, e.Warnings)
		if len(e.Errors) == 0 {
			diags.AddAttributeError(sourceAttr(kind), "Invalid policy", err.Error())
		}
	default:
		apiError(diags, "save the policy of session "+id, err)
	}
}

func unsupported(diags *diag.Diagnostics, id string, err error) {
	diags.AddAttributeError(path.Root("session_id"), "The session predates policies",
		fmt.Sprintf("Session %s was created before session policies existed and cannot be given one. Recreate the session (its disk and the browser's logins do not carry over), then apply again.\n\n%s", id, err))
}

func (r *sessionPolicyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan sessionPolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, diags := plan.Timeouts.Create(ctx, policyWriteTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	p := r.write(ctx, &plan, timeout, &resp.Diagnostics)
	if p == nil {
		return
	}
	plan.ID = plan.SessionID
	plan.setComputed(p)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *sessionPolicyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state sessionPolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	p, err := r.data.client.GetPolicy(ctx, id)
	switch client.StatusOf(err) {
	case 404:
		resp.State.RemoveResource(ctx)
		return
	case 409:
		unsupported(&resp.Diagnostics, id, err)
		return
	}
	if err != nil {
		apiError(&resp.Diagnostics, "read the policy of session "+id, err)
		return
	}
	state.setAll(id, p)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *sessionPolicyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state sessionPolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if sameSource(ctx, &plan, &state, &resp.Diagnostics) {
		// Only wait_for_ready or the timeouts changed; nothing to send.
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}
	timeout, diags := plan.Timeouts.Update(ctx, policyWriteTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	p := r.write(ctx, &plan, timeout, &resp.Diagnostics)
	if p == nil {
		return
	}
	plan.setComputed(p)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *sessionPolicyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state sessionPolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	err := r.data.client.ResetPolicy(ctx, id)
	switch client.StatusOf(err) {
	case 404:
		return // the session is gone, and its policy with it
	case 409:
		// A token may not reset a policy that is managed in the editor, and
		// there it is already no longer managed as code: nothing is left
		// for this resource to give up.
		p, readErr := r.data.client.GetPolicy(ctx, id)
		if readErr == nil && (p.Management == nil || p.Management.Mode != client.ModeIaC) {
			resp.Diagnostics.AddWarning("The policy was left as it is",
				fmt.Sprintf("The policy of session %s is managed in the browserjs UI, so it was not reset to the unrestricted policy. It is no longer in the Terraform state.", id))
			return
		}
		if client.StatusOf(readErr) == 409 || client.IsNotFound(readErr) {
			return // no policy object to reset
		}
	}
	if err != nil {
		apiError(&resp.Diagnostics, "reset the policy of session "+id, err)
	}
}

func (r *sessionPolicyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if !sessionID.MatchString(req.ID) {
		resp.Diagnostics.AddError("Not a session ID", fmt.Sprintf("%q is not a session ID. Import a session's policy by the session's ID, such as s-ab2cd.", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("session_id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("wait_for_ready"), true)...)
}
