package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/r33drichards/computer-use/terraform-provider-metronome/internal/client"
)

var (
	_ resource.Resource                = (*customFieldKeyResource)(nil)
	_ resource.ResourceWithConfigure   = (*customFieldKeyResource)(nil)
	_ resource.ResourceWithImportState = (*customFieldKeyResource)(nil)
)

// customFieldEntities are the kinds of object a custom field can be on.
var customFieldEntities = []string{
	"alert", "billable_metric", "charge", "commit", "contract_credit", "contract_product", "contract", "customer",
	"discount", "invoice", "professional_service", "product", "rate_card", "scheduled_charge", "subscription",
	"package_commit", "package_credit", "package_subscription", "package_scheduled_charge",
}

func newCustomFieldKeyResource() resource.Resource { return &customFieldKeyResource{} }

type customFieldKeyResource struct{ data *providerData }

type customFieldKeyModel struct {
	ID                types.String `tfsdk:"id"`
	Entity            types.String `tfsdk:"entity"`
	Key               types.String `tfsdk:"key"`
	EnforceUniqueness types.Bool   `tfsdk:"enforce_uniqueness"`
}

func (r *customFieldKeyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_custom_field_key"
}

func (r *customFieldKeyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A custom field key: permission to set a custom field of that name on one kind of object. Metronome refuses a custom field whose key has not been added.\n\n" +
			"~> **Nothing about a key can change**; changing an attribute replaces it.\n\n" +
			"~> **Destroying a key deletes it** (this one thing Metronome does delete), and **every value stored under it stops being readable**. Replacing a key does the same.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`<entity>/<key>`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"entity": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The kind of object, for example `customer`, `contract`, `commit` or `contract_credit` (a credit).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:          []validator.String{stringvalidator.OneOf(customFieldEntities...)},
			},
			"key": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The custom field's name.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"enforce_uniqueness": schema.BoolAttribute{
				Required:            true,
				MarkdownDescription: "Whether Metronome refuses a value that another object of the kind already has under this key.",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
		},
	}
}

func (r *customFieldKeyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = configured(req.ProviderData, &resp.Diagnostics)
}

func (r *customFieldKeyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan customFieldKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	k := client.CustomFieldKey{Entity: plan.Entity.ValueString(), Key: plan.Key.ValueString(), EnforceUniqueness: plan.EnforceUniqueness.ValueBool()}
	if err := r.data.client.AddCustomFieldKey(ctx, k); err != nil {
		apiError(&resp.Diagnostics, "add the custom field key", err)
		return
	}
	plan.ID = types.StringValue(k.Entity + "/" + k.Key)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *customFieldKeyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state customFieldKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	keys, err := r.data.client.CustomFieldKeys(ctx, state.Entity.ValueString())
	if err != nil {
		apiError(&resp.Diagnostics, "list the custom field keys of "+state.Entity.ValueString(), err)
		return
	}
	for _, k := range keys {
		if k.Entity == state.Entity.ValueString() && k.Key == state.Key.ValueString() {
			state.EnforceUniqueness = types.BoolValue(k.EnforceUniqueness)
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}
	}
	resp.State.RemoveResource(ctx)
}

// Update is never reached: every attribute replaces the resource.
func (r *customFieldKeyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan customFieldKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *customFieldKeyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state customFieldKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.data.client.RemoveCustomFieldKey(ctx, state.Entity.ValueString(), state.Key.ValueString()); err != nil && !client.IsNotFound(err) {
		apiError(&resp.Diagnostics, "remove custom field key "+state.ID.ValueString(), err)
	}
}

func (r *customFieldKeyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	entity, key, ok := strings.Cut(req.ID, "/")
	if !ok || entity == "" || key == "" {
		resp.Diagnostics.AddError("Not a custom field key ID", fmt.Sprintf("%q is not <entity>/<key>, such as contract_credit/grant_key.", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("entity"), entity)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("key"), key)...)
}
