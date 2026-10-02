package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/float64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/float64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/r33drichards/browserjs-sessions/terraform-provider-metronome/internal/client"
)

var (
	_ resource.Resource                     = (*rateResource)(nil)
	_ resource.ResourceWithConfigure        = (*rateResource)(nil)
	_ resource.ResourceWithImportState      = (*rateResource)(nil)
	_ resource.ResourceWithConfigValidators = (*rateResource)(nil)
)

func newRateResource() resource.Resource { return &rateResource{} }

type rateResource struct{ data *providerData }

type tierModel struct {
	Size  types.Float64 `tfsdk:"size"`
	Price types.Float64 `tfsdk:"price"`
}

type commitRateModel struct {
	RateType types.String  `tfsdk:"rate_type"`
	Price    types.Float64 `tfsdk:"price"`
}

type rateModel struct {
	ID                 types.String      `tfsdk:"id"`
	RateCardID         types.String      `tfsdk:"rate_card_id"`
	ProductID          types.String      `tfsdk:"product_id"`
	StartingAt         types.String      `tfsdk:"starting_at"`
	EndingBefore       types.String      `tfsdk:"ending_before"`
	Entitled           types.Bool        `tfsdk:"entitled"`
	RateType           types.String      `tfsdk:"rate_type"`
	Price              types.Float64     `tfsdk:"price"`
	CreditTypeID       types.String      `tfsdk:"credit_type_id"`
	Tiers              []tierModel       `tfsdk:"tiers"`
	Quantity           types.Float64     `tfsdk:"quantity"`
	IsProrated         types.Bool        `tfsdk:"is_prorated"`
	BillingFrequency   types.String      `tfsdk:"billing_frequency"`
	PricingGroupValues map[string]string `tfsdk:"pricing_group_values"`
	CommitRate         *commitRateModel  `tfsdk:"commit_rate"`
}

func rateID(rateCardID, productID, startingAt string) string {
	return rateCardID + "/" + productID + "/" + startingAt
}

func (m *rateModel) input() client.RateInput {
	in := client.RateInput{
		RateCardID:         m.RateCardID.ValueString(),
		ProductID:          m.ProductID.ValueString(),
		StartingAt:         m.StartingAt.ValueString(),
		EndingBefore:       m.EndingBefore.ValueString(),
		Entitled:           m.Entitled.ValueBool(),
		RateType:           m.RateType.ValueString(),
		Price:              m.Price.ValueFloat64Pointer(),
		Quantity:           m.Quantity.ValueFloat64Pointer(),
		IsProrated:         m.IsProrated.ValueBoolPointer(),
		BillingFrequency:   m.BillingFrequency.ValueString(),
		PricingGroupValues: m.PricingGroupValues,
	}
	if !m.CreditTypeID.IsUnknown() {
		in.CreditTypeID = m.CreditTypeID.ValueString()
	}
	for _, t := range m.Tiers {
		in.Tiers = append(in.Tiers, client.Tier{Size: t.Size.ValueFloat64Pointer(), Price: t.Price.ValueFloat64()})
	}
	if c := m.CommitRate; c != nil {
		in.CommitRate = &client.CommitRate{RateType: c.RateType.ValueString(), Price: c.Price.ValueFloat64Pointer()}
	}
	return in
}

// set writes the rate the API reports in force at this rate's start.
func (m *rateModel) set(e client.RateScheduleEntry) {
	m.StartingAt = keepInstant(m.StartingAt, e.StartingAt)
	m.EndingBefore = keepInstant(m.EndingBefore, e.EndingBefore)
	m.ID = types.StringValue(rateID(m.RateCardID.ValueString(), m.ProductID.ValueString(), m.StartingAt.ValueString()))
	m.Entitled = types.BoolValue(e.Entitled)
	m.RateType = types.StringValue(strings.ToUpper(e.Rate.RateType))
	m.Price = types.Float64PointerValue(e.Rate.Price)
	m.Quantity = types.Float64PointerValue(e.Rate.Quantity)
	m.IsProrated = types.BoolPointerValue(e.Rate.IsProrated)
	m.BillingFrequency = optionalString(strings.ToUpper(e.BillingFrequency))
	if e.Rate.CreditType != nil {
		m.CreditTypeID = types.StringValue(e.Rate.CreditType.ID)
	}
	m.CommitRate = nil
	if c := e.CommitRate; c != nil {
		m.CommitRate = &commitRateModel{RateType: types.StringValue(strings.ToUpper(c.RateType)), Price: types.Float64PointerValue(c.Price)}
	}
	if !(len(e.PricingGroupValues) == 0 && len(m.PricingGroupValues) == 0) {
		m.PricingGroupValues = e.PricingGroupValues
	}
	if !(len(e.Rate.Tiers) == 0 && len(m.Tiers) == 0) {
		m.Tiers = nil
		for _, t := range e.Rate.Tiers {
			m.Tiers = append(m.Tiers, tierModel{Size: types.Float64PointerValue(t.Size), Price: types.Float64Value(t.Price)})
		}
	}
}

func (r *rateResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_rate"
}

func (r *rateResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replaceString := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	replaceFloat := []planmodifier.Float64{float64planmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "A rate: the price of one product on one rate card, from a moment on.\n\n" +
			"~> **A rate card's rates are a schedule that is only ever added to.** Metronome has no call that edits or removes a rate, so:\n\n" +
			"* **Every attribute replaces the resource**: the old rate is dropped from the state and a new one added to the schedule.\n" +
			"* **To change a price, change `starting_at` with it.** The new rate takes over from its own `starting_at`; usage before that keeps the old price. A new rate with the *same* `starting_at` as an existing one is sent to Metronome as it is; what Metronome then does is not documented.\n" +
			"* **Destroying a rate removes it from the state and leaves it in Metronome**, with a warning. To stop charging for a product, add a rate with `entitled = false`.\n\n" +
			"A price in USD is in **cents**; fractions of a cent are allowed. Tiered commit rates, custom rates and percentage minimums are not supported by this resource.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`<rate_card_id>/<product_id>/<starting_at>`. Metronome gives rates no ID of their own.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"rate_card_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The rate card.",
				PlanModifiers:       replaceString,
				Validators:          []validator.String{isUUID()},
			},
			"product_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The product priced.",
				PlanModifiers:       replaceString,
				Validators:          []validator.String{isUUID()},
			},
			"starting_at": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "From when the rate applies (RFC 3339, inclusive, on an hour boundary).",
				PlanModifiers:       replaceString,
				Validators:          []validator.String{timestampValidator{hourly: true}},
			},
			"ending_before": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Until when the rate applies (RFC 3339, exclusive, on an hour boundary). Left out, until a later rate takes over.",
				PlanModifiers:       replaceString,
				Validators:          []validator.String{timestampValidator{hourly: true}},
			},
			"entitled": schema.BoolAttribute{
				Required:            true,
				MarkdownDescription: "Whether customers on the rate card may use the product. `false` withdraws it: its usage is not charged.",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
			"rate_type": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "`FLAT` (a price per unit), `TIERED`, `PERCENTAGE` or `SUBSCRIPTION`.",
				PlanModifiers:       replaceString,
				Validators:          []validator.String{stringvalidator.OneOf("FLAT", "TIERED", "PERCENTAGE", "SUBSCRIPTION")},
			},
			"price": schema.Float64Attribute{
				Optional:            true,
				MarkdownDescription: "For `FLAT` and `SUBSCRIPTION`: the price of one unit, in the pricing unit (USD: cents). For `PERCENTAGE`: a fraction, `0.1` for 10%.",
				PlanModifiers:       replaceFloat,
				Validators:          []validator.Float64{float64validator.AtLeast(0)},
			},
			"credit_type_id": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "The pricing unit of the price. Defaults to USD (cents).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown(), stringplanmodifier.RequiresReplace()},
				Validators:          []validator.String{isUUID()},
			},
			"tiers": schema.ListNestedAttribute{
				Optional:            true,
				MarkdownDescription: "For `TIERED`: the tiers, in order.",
				PlanModifiers:       []planmodifier.List{listplanmodifier.RequiresReplace()},
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"size": schema.Float64Attribute{
							Optional:            true,
							MarkdownDescription: "How many units the tier holds. Left out on the last tier, which has no end.",
						},
						"price": schema.Float64Attribute{
							Required:            true,
							MarkdownDescription: "The price of one unit in the tier.",
						},
					},
				},
			},
			"quantity": schema.Float64Attribute{
				Optional:            true,
				MarkdownDescription: "For `SUBSCRIPTION`: the default quantity.",
				PlanModifiers:       replaceFloat,
			},
			"is_prorated": schema.BoolAttribute{
				Optional:            true,
				MarkdownDescription: "For `SUBSCRIPTION`: whether a part period is charged in proportion.",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
			"billing_frequency": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "For `SUBSCRIPTION`: `MONTHLY`, `QUARTERLY`, `ANNUAL` or `WEEKLY`.",
				PlanModifiers:       replaceString,
				Validators:          []validator.String{stringvalidator.OneOf("MONTHLY", "QUARTERLY", "ANNUAL", "WEEKLY")},
			},
			"commit_rate": schema.SingleNestedAttribute{
				Optional: true,
				MarkdownDescription: "A second price, used in place of the list price while usage is paid for from a commit or a credit that asks for it (`rate_type: COMMIT_RATE`). " +
					"With a list `price` of 0 and the real price here, usage draws credit down while there is credit and costs nothing when there is none.",
				PlanModifiers: []planmodifier.Object{objectplanmodifier.RequiresReplace()},
				Attributes: map[string]schema.Attribute{
					"rate_type": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "`FLAT`.",
						Validators:          []validator.String{stringvalidator.OneOf("FLAT")},
					},
					"price": schema.Float64Attribute{
						Required:            true,
						MarkdownDescription: "The price of one unit, in the pricing unit (USD: cents).",
						Validators:          []validator.Float64{float64validator.AtLeast(0)},
					},
				},
			},
			"pricing_group_values": schema.MapAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "For a product with a `pricing_group_key`: the values of the key this rate is for.",
				PlanModifiers:       []planmodifier.Map{mapplanmodifier.RequiresReplace()},
			},
		},
	}
}

func (r *rateResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{rateTypeValidator{}}
}

// rateTypeValidator asks each rate type for what it needs.
type rateTypeValidator struct{}

func (rateTypeValidator) Description(context.Context) string {
	return "a rate has the attributes its rate_type needs"
}

func (v rateTypeValidator) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (rateTypeValidator) ValidateResource(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var typ, frequency types.String
	var price types.Float64
	var tiers types.List
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("rate_type"), &typ)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("price"), &price)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("tiers"), &tiers)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("billing_frequency"), &frequency)...)
	if resp.Diagnostics.HasError() || typ.IsNull() || typ.IsUnknown() {
		return
	}
	switch t := typ.ValueString(); t {
	case "TIERED":
		if tiers.IsNull() {
			resp.Diagnostics.AddAttributeError(path.Root("tiers"), "Missing tiers", "A TIERED rate has tiers.")
		}
		if !price.IsNull() {
			resp.Diagnostics.AddAttributeError(path.Root("price"), "A TIERED rate has no price", "Its prices are in tiers.")
		}
	default:
		if price.IsNull() {
			resp.Diagnostics.AddAttributeError(path.Root("price"), "Missing price", "A "+t+" rate has a price.")
		}
		if !tiers.IsNull() {
			resp.Diagnostics.AddAttributeError(path.Root("tiers"), "Only for TIERED rates", "tiers is not allowed on a "+t+" rate.")
		}
		if t == "SUBSCRIPTION" && frequency.IsNull() {
			resp.Diagnostics.AddAttributeError(path.Root("billing_frequency"), "Missing billing_frequency", "A SUBSCRIPTION rate is billed at a frequency.")
		}
	}
}

func (r *rateResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = configured(req.ProviderData, &resp.Diagnostics)
}

func (r *rateResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan rateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	in := plan.input()
	if err := r.data.client.AddRate(ctx, in); err != nil {
		apiError(&resp.Diagnostics, "add the rate", err)
		return
	}
	plan.ID = types.StringValue(rateID(in.RateCardID, in.ProductID, in.StartingAt))
	if plan.CreditTypeID.IsUnknown() {
		plan.CreditTypeID = types.StringValue(client.USDCreditTypeID)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// find is the rate of the schedule that is this resource's: the one in force
// at its own start, for its pricing group values, that starts there.
func (m *rateModel) find(entries []client.RateScheduleEntry) (client.RateScheduleEntry, bool) {
	want, _ := json.Marshal(m.PricingGroupValues)
	for _, e := range entries {
		have, _ := json.Marshal(e.PricingGroupValues)
		sameGroup := string(want) == string(have) || (len(m.PricingGroupValues) == 0 && len(e.PricingGroupValues) == 0)
		if e.ProductID == m.ProductID.ValueString() && sameGroup && sameInstant(e.StartingAt, m.StartingAt.ValueString()) {
			return e, true
		}
	}
	return client.RateScheduleEntry{}, false
}

func (r *rateResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state rateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	entries, err := r.data.client.RatesAt(ctx, state.RateCardID.ValueString(), state.ProductID.ValueString(), state.StartingAt.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		apiError(&resp.Diagnostics, "read rate "+state.ID.ValueString(), err)
		return
	}
	e, ok := state.find(entries)
	if !ok {
		// No rate starts at that moment: it was never added, or its rate
		// card lost it. The next apply adds it.
		resp.State.RemoveResource(ctx)
		return
	}
	state.set(e)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is never reached: every attribute replaces the resource.
func (r *rateResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan rateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *rateResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state rateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.AddWarning("The rate stays in Metronome",
		fmt.Sprintf("Metronome has no call that removes a rate. Rate %s was removed from the state only: it stays on its rate card's schedule, in force until a later rate for the same product takes over (or its rate card is archived).", state.ID.ValueString()))
}

func (r *rateResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, "/", 3)
	if len(parts) != 3 || !uuidPattern.MatchString(parts[0]) || !uuidPattern.MatchString(parts[1]) {
		resp.Diagnostics.AddError("Not a rate ID", fmt.Sprintf("%q is not <rate_card_id>/<product_id>/<starting_at>, such as d7abd0cd-4ae9-4db7-8676-e986a4ebd8dc/13117714-3f05-48e5-a6e9-a66093f13b4d/2026-01-01T00:00:00Z.", req.ID))
		return
	}
	if _, err := time.Parse(time.RFC3339, parts[2]); err != nil {
		resp.Diagnostics.AddError("Not a rate ID", fmt.Sprintf("%q is not an RFC 3339 timestamp.", parts[2]))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("rate_card_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("product_id"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("starting_at"), parts[2])...)
}
