package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/r33drichards/browserjs-sessions/terraform-provider-metronome/internal/client"
)

var (
	_ resource.Resource                = (*rateCardResource)(nil)
	_ resource.ResourceWithConfigure   = (*rateCardResource)(nil)
	_ resource.ResourceWithImportState = (*rateCardResource)(nil)
)

func newRateCardResource() resource.Resource { return &rateCardResource{} }

type rateCardResource struct{ data *providerData }

type aliasModel struct {
	Name         types.String `tfsdk:"name"`
	StartingAt   types.String `tfsdk:"starting_at"`
	EndingBefore types.String `tfsdk:"ending_before"`
}

type rateCardModel struct {
	ID               types.String `tfsdk:"id"`
	Name             types.String `tfsdk:"name"`
	Description      types.String `tfsdk:"description"`
	FiatCreditTypeID types.String `tfsdk:"fiat_credit_type_id"`
	Aliases          []aliasModel `tfsdk:"aliases"`
}

func (m *rateCardModel) aliases() []client.Alias {
	var out []client.Alias
	for _, a := range m.Aliases {
		out = append(out, client.Alias{Name: a.Name.ValueString(), StartingAt: a.StartingAt.ValueString(), EndingBefore: a.EndingBefore.ValueString()})
	}
	return out
}

// set writes what the API returned. A timestamp is kept as the state wrote
// it when it names the same moment.
func (m *rateCardModel) set(got *client.RateCard) {
	m.ID = types.StringValue(got.ID)
	m.Name = types.StringValue(got.Name)
	if !(m.Description.IsNull() && got.Description == "") {
		m.Description = types.StringValue(got.Description)
	}
	if got.FiatCreditType != nil {
		m.FiatCreditTypeID = types.StringValue(got.FiatCreditType.ID)
	}
	if len(got.Aliases) == 0 && len(m.Aliases) == 0 {
		return
	}
	prior := m.Aliases
	m.Aliases = nil
	for i, a := range got.Aliases {
		next := aliasModel{Name: types.StringValue(a.Name), StartingAt: optionalString(a.StartingAt), EndingBefore: optionalString(a.EndingBefore)}
		if i < len(prior) && prior[i].Name.ValueString() == a.Name {
			next.StartingAt = keepInstant(prior[i].StartingAt, a.StartingAt)
			next.EndingBefore = keepInstant(prior[i].EndingBefore, a.EndingBefore)
		}
		m.Aliases = append(m.Aliases, next)
	}
}

func (r *rateCardResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_rate_card"
}

func (r *rateCardResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A rate card: the price list a contract refers to. Its prices are `metronome_rate` resources.\n\n" +
			"The name, the description and the aliases change in place and affect no price.\n\n" +
			"~> **Destroying a rate card archives it.** Metronome has no delete, and an archived rate card cannot be restored. Contracts that already use it keep their prices; no new contract can use it. " +
			"Metronome's API does not say whether a rate card is archived, so one archived outside Terraform is not noticed until a rate is added to it.\n\n" +
			"Custom pricing units (`credit_type_conversions`) are not supported by this resource: rates are in the rate card's currency.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The rate card's Metronome ID.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The name, shown only in Metronome's app and API. Changed in place.",
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "A description. Changed in place.",
			},
			"fiat_credit_type_id": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "The currency of the rate card, as a pricing unit ID (see the `metronome_pricing_unit` data source). Defaults to USD (cents). Changing it replaces the rate card.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown(), stringplanmodifier.RequiresReplace()},
				Validators:          []validator.String{isUUID()},
			},
			"aliases": schema.ListNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Names a contract can give in place of the rate card's ID (`rate_card_alias`), each optionally for a period. Moving an alias to a new rate card changes which prices **new** contracts get; existing contracts keep theirs. Changed in place.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							Required:            true,
							MarkdownDescription: "The alias.",
							Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
						},
						"starting_at": schema.StringAttribute{
							Optional:            true,
							MarkdownDescription: "From when the alias means this rate card (RFC 3339, inclusive).",
							Validators:          []validator.String{timestampValidator{}},
						},
						"ending_before": schema.StringAttribute{
							Optional:            true,
							MarkdownDescription: "Until when (RFC 3339, exclusive).",
							Validators:          []validator.String{timestampValidator{}},
						},
					},
				},
			},
		},
	}
}

func (r *rateCardResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = configured(req.ProviderData, &resp.Diagnostics)
}

func (r *rateCardResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan rateCardModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	in := client.RateCardInput{Name: plan.Name.ValueString(), Description: plan.Description.ValueString(), Aliases: plan.aliases()}
	if !plan.FiatCreditTypeID.IsUnknown() {
		in.FiatCreditTypeID = plan.FiatCreditTypeID.ValueString()
	}
	id, err := r.data.client.CreateRateCard(ctx, in)
	if err != nil {
		apiError(&resp.Diagnostics, "create the rate card", err)
		return
	}
	// In state before the read, so a read that fails leaves a rate card
	// Terraform knows about.
	plan.ID = types.StringValue(id)
	if plan.FiatCreditTypeID.IsUnknown() {
		plan.FiatCreditTypeID = types.StringValue(client.USDCreditTypeID)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	got, err := r.data.client.GetRateCard(ctx, id)
	if err != nil {
		apiError(&resp.Diagnostics, "read the new rate card "+id, err)
		return
	}
	plan.set(got)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *rateCardResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state rateCardModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	got, err := r.data.client.GetRateCard(ctx, state.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		apiError(&resp.Diagnostics, "read rate card "+state.ID.ValueString(), err)
		return
	}
	state.set(got)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *rateCardResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state rateCardModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	description := plan.Description.ValueString()
	err := r.data.client.UpdateRateCard(ctx, client.RateCardUpdate{
		RateCardID: state.ID.ValueString(), Name: plan.Name.ValueString(), Description: &description, Aliases: plan.aliases(),
	})
	if err != nil {
		apiError(&resp.Diagnostics, "update rate card "+state.ID.ValueString(), err)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *rateCardResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state rateCardModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.data.client.ArchiveRateCard(ctx, state.ID.ValueString()); err != nil && !client.IsNotFound(err) {
		apiError(&resp.Diagnostics, "archive rate card "+state.ID.ValueString(), err)
	}
}

func (r *rateCardResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if importUUID("rate card", req, resp) {
		resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
	}
}
