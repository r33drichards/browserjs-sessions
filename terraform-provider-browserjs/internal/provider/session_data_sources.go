package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/r33drichards/computer-use/terraform-provider-browserjs/internal/client"
)

type sessionModel struct {
	ID     types.String `tfsdk:"id"`
	Name   types.String `tfsdk:"name"`
	MCPURL types.String `tfsdk:"mcp_url"`
	State  types.String `tfsdk:"state"`
	Owner  types.String `tfsdk:"owner"`
}

func sessionModelOf(s client.Session) sessionModel {
	return sessionModel{
		ID:     types.StringValue(s.ID),
		Name:   types.StringValue(s.Name),
		MCPURL: types.StringValue(s.MCPURL),
		State:  types.StringValue(s.State),
		Owner:  types.StringValue(s.Owner),
	}
}

var sessionAttrTypes = map[string]attr.Type{
	"id": types.StringType, "name": types.StringType, "mcp_url": types.StringType,
	"state": types.StringType, "owner": types.StringType,
}

// browserjs_session

var (
	_ datasource.DataSource                     = (*sessionDataSource)(nil)
	_ datasource.DataSourceWithConfigure        = (*sessionDataSource)(nil)
	_ datasource.DataSourceWithConfigValidators = (*sessionDataSource)(nil)
)

func newSessionDataSource() datasource.DataSource { return &sessionDataSource{} }

type sessionDataSource struct{ data *providerData }

func (d *sessionDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_session"
}

func (d *sessionDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One existing session, found by its ID or by its name. Use it to give a policy to a session that was made in the UI, without importing the session.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "The session ID. Exactly one of `id` and `name` is required.",
			},
			"name": schema.StringAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "The session's name, which must match exactly one of the token owner's sessions. Exactly one of `id` and `name` is required.",
			},
			"mcp_url": schema.StringAttribute{Computed: true, MarkdownDescription: "What an MCP client is pointed at."},
			"state":   schema.StringAttribute{Computed: true, MarkdownDescription: "The session's state as the API reports it."},
			"owner":   schema.StringAttribute{Computed: true, MarkdownDescription: "Who owns the session."},
		},
	}
}

func (d *sessionDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(path.MatchRoot("id"), path.MatchRoot("name")),
	}
}

func (d *sessionDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.data = configured(req.ProviderData, &resp.Diagnostics)
}

func (d *sessionDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg sessionModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !cfg.ID.IsNull() {
		id := cfg.ID.ValueString()
		s, err := d.data.client.GetSession(ctx, id)
		if client.IsNotFound(err) {
			resp.Diagnostics.AddAttributeError(path.Root("id"), "No such session",
				fmt.Sprintf("Session %s does not exist, or is not the API token owner's.", id))
			return
		}
		if err != nil {
			apiError(&resp.Diagnostics, "read session "+id, err)
			return
		}
		resp.Diagnostics.Append(resp.State.Set(ctx, sessionModelOf(*s))...)
		return
	}

	name := cfg.Name.ValueString()
	all, err := d.data.client.ListSessions(ctx)
	if err != nil {
		apiError(&resp.Diagnostics, "list sessions", err)
		return
	}
	var found []client.Session
	for _, s := range all {
		if s.Name == name {
			found = append(found, s)
		}
	}
	switch len(found) {
	case 1:
		resp.Diagnostics.Append(resp.State.Set(ctx, sessionModelOf(found[0]))...)
	case 0:
		resp.Diagnostics.AddAttributeError(path.Root("name"), "No such session", fmt.Sprintf("No session is named %q.", name))
	default:
		ids := make([]string, len(found))
		for i, s := range found {
			ids[i] = s.ID
		}
		resp.Diagnostics.AddAttributeError(path.Root("name"), "More than one session has this name",
			fmt.Sprintf("%d sessions are named %q: %s. Use id instead.", len(found), name, strings.Join(ids, ", ")))
	}
}

// browserjs_sessions

var (
	_ datasource.DataSource              = (*sessionsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*sessionsDataSource)(nil)
)

func newSessionsDataSource() datasource.DataSource { return &sessionsDataSource{} }

type sessionsDataSource struct{ data *providerData }

type sessionsModel struct {
	Sessions types.List `tfsdk:"sessions"`
}

func (d *sessionsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_sessions"
}

func (d *sessionsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every session of the API token's owner.",
		Attributes: map[string]schema.Attribute{
			"sessions": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The sessions, in the order the API lists them.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id":      schema.StringAttribute{Computed: true, MarkdownDescription: "The session ID."},
						"name":    schema.StringAttribute{Computed: true, MarkdownDescription: "The session's name."},
						"mcp_url": schema.StringAttribute{Computed: true, MarkdownDescription: "What an MCP client is pointed at."},
						"state":   schema.StringAttribute{Computed: true, MarkdownDescription: "The session's state as the API reports it."},
						"owner":   schema.StringAttribute{Computed: true, MarkdownDescription: "Who owns the session."},
					},
				},
			},
		},
	}
}

func (d *sessionsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.data = configured(req.ProviderData, &resp.Diagnostics)
}

func (d *sessionsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	all, err := d.data.client.ListSessions(ctx)
	if err != nil {
		apiError(&resp.Diagnostics, "list sessions", err)
		return
	}
	models := make([]sessionModel, len(all))
	for i, s := range all {
		models[i] = sessionModelOf(s)
	}
	list, diags := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: sessionAttrTypes}, models)
	resp.Diagnostics.Append(diags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, sessionsModel{Sessions: list})...)
}
