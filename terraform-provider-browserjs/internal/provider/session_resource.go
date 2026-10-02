package provider

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/r33drichards/browserjs-sessions/terraform-provider-browserjs/internal/client"
)

// sessionID is the id parameter of backend-api.yaml.
var sessionID = regexp.MustCompile(`^s-([a-z2-7]{10}|[a-z0-9]{5})$`)

const sessionCreateTimeout = 5 * time.Minute

var (
	_ resource.Resource                = (*sessionResource)(nil)
	_ resource.ResourceWithConfigure   = (*sessionResource)(nil)
	_ resource.ResourceWithImportState = (*sessionResource)(nil)
)

func newSessionResource() resource.Resource { return &sessionResource{} }

type sessionResource struct{ data *providerData }

type sessionResourceModel struct {
	ID       types.String   `tfsdk:"id"`
	Name     types.String   `tfsdk:"name"`
	MCPURL   types.String   `tfsdk:"mcp_url"`
	State    types.String   `tfsdk:"state"`
	Owner    types.String   `tfsdk:"owner"`
	Timeouts timeouts.Value `tfsdk:"timeouts"`
}

func (m *sessionResourceModel) set(s *client.Session) {
	m.ID = types.StringValue(s.ID)
	m.Name = types.StringValue(s.Name)
	m.MCPURL = types.StringValue(s.MCPURL)
	m.State = types.StringValue(s.State)
	m.Owner = types.StringValue(s.Owner)
}

func (r *sessionResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_session"
}

func (r *sessionResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A browserjs session: one persistent browser with its own disk, driven by agents over MCP.\n\n" +
			"~> **Destroying a session deletes its disk and the browser's logins.** Nothing brings them back. " +
			"Protect sessions you care about with `lifecycle { prevent_destroy = true }`.\n\n" +
			"A new session has the unrestricted policy. Give it another with `browserjs_session_policy`; " +
			"changing a policy never replaces the session.\n\n" +
			"The API token needs the scopes `sessions:read` and `sessions:write`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The session ID, such as `s-ab2cd`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "The session's name. Changed in place. Left out, the server names the session.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"mcp_url": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "What an MCP client is pointed at.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"state": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The session's state as the API reported it when last read (for example `running`). Not waited on after creation.",
			},
			"owner": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Who owns the session: the owner of the API token that created it.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx, timeouts.Opts{
				Create:            true,
				CreateDescription: "How long to wait for a new session's policy to be loaded. A duration such as `10m`; the default is 5 minutes.",
			}),
		},
	}
}

func (r *sessionResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = configured(req.ProviderData, &resp.Diagnostics)
}

func (r *sessionResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan sessionResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, diags := plan.Timeouts.Create(ctx, sessionCreateTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	s, err := r.data.client.CreateSession(ctx, plan.Name.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "create the session", err)
		return
	}
	// In state before the wait, so a wait that fails leaves a session that
	// Terraform knows about instead of one nobody does.
	plan.set(s)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	last := policyStateOf(s)
	err = poll(ctx, r.data.pollEvery, func() (bool, error) {
		if last == client.StateReady {
			return true, nil
		}
		got, err := r.data.client.GetSession(ctx, s.ID)
		if err != nil {
			return false, err
		}
		s, last = got, policyStateOf(got)
		return last == client.StateReady, nil
	})
	if err != nil {
		if ctx.Err() != nil {
			resp.Diagnostics.AddError("The session's policy was not loaded in time",
				fmt.Sprintf("Session %s was created, and after %s its policy is %q, not \"ready\". It is in the state as tainted; the next apply replaces it.", s.ID, timeout, last))
			return
		}
		apiError(&resp.Diagnostics, "read the new session "+s.ID, err)
		return
	}
	plan.set(s)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func policyStateOf(s *client.Session) string {
	if s.Policy == nil {
		return "not reported"
	}
	return s.Policy.State
}

func (r *sessionResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state sessionResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	s, err := r.data.client.GetSession(ctx, state.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		apiError(&resp.Diagnostics, "read session "+state.ID.ValueString(), err)
		return
	}
	state.set(s)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *sessionResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state sessionResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	var s *client.Session
	var err error
	if plan.Name.Equal(state.Name) {
		// Only the timeouts changed.
		s, err = r.data.client.GetSession(ctx, id)
	} else {
		s, err = r.data.client.RenameSession(ctx, id, plan.Name.ValueString())
	}
	if err != nil {
		apiError(&resp.Diagnostics, "update session "+id, err)
		return
	}
	plan.set(s)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *sessionResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state sessionResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.data.client.DeleteSession(ctx, state.ID.ValueString()); err != nil && !client.IsNotFound(err) {
		apiError(&resp.Diagnostics, "delete session "+state.ID.ValueString(), err)
	}
}

func (r *sessionResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if !sessionID.MatchString(req.ID) {
		resp.Diagnostics.AddError("Not a session ID", fmt.Sprintf("%q is not a session ID. Import a session by its ID, such as s-ab2cd.", req.ID))
		return
	}
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// parameterName and hostPattern are the patterns of json-policy.schema.json.
var (
	parameterName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	hostPattern   = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
)
