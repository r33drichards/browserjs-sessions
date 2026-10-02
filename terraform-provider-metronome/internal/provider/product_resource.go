package provider

import (
	"context"
	"encoding/json"
	"strings"
	"time"

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
	_ resource.Resource                     = (*productResource)(nil)
	_ resource.ResourceWithConfigure        = (*productResource)(nil)
	_ resource.ResourceWithImportState      = (*productResource)(nil)
	_ resource.ResourceWithConfigValidators = (*productResource)(nil)
)

func newProductResource() resource.Resource { return &productResource{} }

type productResource struct{ data *providerData }

type quantityConversionModel struct {
	Name             types.String  `tfsdk:"name"`
	ConversionFactor types.Float64 `tfsdk:"conversion_factor"`
	Operation        types.String  `tfsdk:"operation"`
}

type quantityRoundingModel struct {
	RoundingMethod types.String `tfsdk:"rounding_method"`
	DecimalPlaces  types.Int64  `tfsdk:"decimal_places"`
}

type productModel struct {
	ID                   types.String             `tfsdk:"id"`
	Name                 types.String             `tfsdk:"name"`
	Type                 types.String             `tfsdk:"type"`
	BillableMetricID     types.String             `tfsdk:"billable_metric_id"`
	Tags                 []string                 `tfsdk:"tags"`
	QuantityConversion   *quantityConversionModel `tfsdk:"quantity_conversion"`
	QuantityRounding     *quantityRoundingModel   `tfsdk:"quantity_rounding"`
	PricingGroupKey      []string                 `tfsdk:"pricing_group_key"`
	PresentationGroupKey []string                 `tfsdk:"presentation_group_key"`
}

// api is the product as the API holds it.
func (m *productModel) api() client.ProductState {
	out := client.ProductState{
		Name:                 m.Name.ValueString(),
		BillableMetricID:     m.BillableMetricID.ValueString(),
		Tags:                 m.Tags,
		PricingGroupKey:      m.PricingGroupKey,
		PresentationGroupKey: m.PresentationGroupKey,
	}
	if c := m.QuantityConversion; c != nil {
		out.QuantityConversion = &client.QuantityConversion{
			Name: c.Name.ValueString(), ConversionFactor: c.ConversionFactor.ValueFloat64(), Operation: c.Operation.ValueString(),
		}
	}
	if q := m.QuantityRounding; q != nil {
		out.QuantityRounding = &client.QuantityRounding{
			RoundingMethod: q.RoundingMethod.ValueString(), DecimalPlaces: float64(q.DecimalPlaces.ValueInt64()),
		}
	}
	return out
}

// set writes what the API returned, keeping the state as written where it
// says the same (see billableMetricModel.set).
func (m *productModel) set(got *client.Product) {
	m.ID = types.StringValue(got.ID)
	m.Type = types.StringValue(strings.ToUpper(got.Type))
	cur := got.Current
	if sameProduct(m.api(), cur) {
		return
	}
	m.Name = types.StringValue(cur.Name)
	m.BillableMetricID = optionalString(cur.BillableMetricID)
	m.Tags = cur.Tags
	m.PricingGroupKey = cur.PricingGroupKey
	m.PresentationGroupKey = cur.PresentationGroupKey
	m.QuantityConversion = nil
	if c := cur.QuantityConversion; c != nil {
		m.QuantityConversion = &quantityConversionModel{
			Name: optionalString(c.Name), ConversionFactor: types.Float64Value(c.ConversionFactor), Operation: types.StringValue(strings.ToUpper(c.Operation)),
		}
	}
	m.QuantityRounding = nil
	if q := cur.QuantityRounding; q != nil {
		m.QuantityRounding = &quantityRoundingModel{
			RoundingMethod: types.StringValue(strings.ToUpper(q.RoundingMethod)), DecimalPlaces: types.Int64Value(int64(q.DecimalPlaces)),
		}
	}
}

func sameProduct(a, b client.ProductState) bool {
	norm := func(p client.ProductState) string {
		if c := p.QuantityConversion; c != nil {
			cc := *c
			cc.Operation = strings.ToUpper(cc.Operation)
			p.QuantityConversion = &cc
		}
		if q := p.QuantityRounding; q != nil {
			qq := *q
			qq.RoundingMethod = strings.ToUpper(qq.RoundingMethod)
			p.QuantityRounding = &qq
		}
		b, _ := json.Marshal(p)
		return string(b)
	}
	return norm(a) == norm(b)
}

func (r *productResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_product"
}

func (r *productResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A product: what a line of an invoice is for. A `USAGE` product prices a billable metric; a `FIXED` product is what a commit or a credit is sold as; a `SUBSCRIPTION` product is a recurring charge.\n\n" +
			"A product keeps a history. A change made here is added to it as an update that takes effect **from the start of the current hour** (Metronome wants an hour boundary), so usage already invoiced is not repriced.\n\n" +
			"~> **`type` cannot change**; changing it replaces the product.\n\n" +
			"~> **Destroying a product archives it.** Metronome has no delete, and an archived product cannot be restored. Rates that already name it keep working; no new rate can.\n\n" +
			"`COMPOSITE` and professional-service products are not supported by this resource.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The product's Metronome ID.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The name, shown on invoices. Changed in place.",
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"type": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "`USAGE`, `FIXED` or `SUBSCRIPTION`. Changing it replaces the product.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:          []validator.String{stringvalidator.OneOf("USAGE", "FIXED", "SUBSCRIPTION")},
			},
			"billable_metric_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The billable metric a `USAGE` product prices. Required for `USAGE`, not allowed otherwise. Changed in place: the product meters with the new metric from the start of the current hour.",
				Validators:          []validator.String{isUUID()},
			},
			"tags": schema.ListAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Tags, by which commits and credits can be limited to some products. Changed in place.",
			},
			"quantity_conversion": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "For `USAGE` products: converts the metric's quantity before it is priced, for example seconds to hours. Changed in place.",
				Attributes: map[string]schema.Attribute{
					"name": schema.StringAttribute{
						Optional:            true,
						MarkdownDescription: "A name for the conversion.",
					},
					"conversion_factor": schema.Float64Attribute{
						Required:            true,
						MarkdownDescription: "What the quantity is multiplied or divided by.",
					},
					"operation": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "`MULTIPLY` or `DIVIDE`.",
						Validators:          []validator.String{stringvalidator.OneOf("MULTIPLY", "DIVIDE")},
					},
				},
			},
			"quantity_rounding": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "For `USAGE` products: rounds the quantity (after conversion) before it is priced. Changed in place.",
				Attributes: map[string]schema.Attribute{
					"rounding_method": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "`ROUND_UP`, `ROUND_DOWN` or `ROUND_HALF_UP`.",
						Validators:          []validator.String{stringvalidator.OneOf("ROUND_UP", "ROUND_DOWN", "ROUND_HALF_UP")},
					},
					"decimal_places": schema.Int64Attribute{
						Required:            true,
						MarkdownDescription: "How many decimal places to keep.",
					},
				},
			},
			"pricing_group_key": schema.ListAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "For `USAGE` products: the group key of the metric by whose values the product is priced separately. Changed in place.",
			},
			"presentation_group_key": schema.ListAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "For `USAGE` products: the group key of the metric by which usage is broken down on an invoice. Changed in place.",
			},
		},
	}
}

func (r *productResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{productTypeValidator{}}
}

// productTypeValidator keeps the attributes of usage products to usage
// products.
type productTypeValidator struct{}

func (productTypeValidator) Description(context.Context) string {
	return "a USAGE product has a billable metric, and no other product has usage settings"
}

func (v productTypeValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (productTypeValidator) ValidateResource(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var typ, metric types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("type"), &typ)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("billable_metric_id"), &metric)...)
	if resp.Diagnostics.HasError() || typ.IsNull() || typ.IsUnknown() {
		return
	}
	if typ.ValueString() == "USAGE" {
		if metric.IsNull() {
			resp.Diagnostics.AddAttributeError(path.Root("billable_metric_id"), "Missing billable_metric_id", "A USAGE product prices a billable metric.")
		}
		return
	}
	var conversion, rounding types.Object
	var pricing, presentation types.List
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("quantity_conversion"), &conversion)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("quantity_rounding"), &rounding)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("pricing_group_key"), &pricing)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("presentation_group_key"), &presentation)...)
	for name, set := range map[string]bool{
		"billable_metric_id":     !metric.IsNull(),
		"quantity_conversion":    !conversion.IsNull(),
		"quantity_rounding":      !rounding.IsNull(),
		"pricing_group_key":      !pricing.IsNull(),
		"presentation_group_key": !presentation.IsNull(),
	} {
		if set {
			resp.Diagnostics.AddAttributeError(path.Root(name), "Only for USAGE products", name+" is not allowed on a "+typ.ValueString()+" product.")
		}
	}
}

func (r *productResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = configured(req.ProviderData, &resp.Diagnostics)
}

func (r *productResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan productModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p := plan.api()
	id, err := r.data.client.CreateProduct(ctx, client.ProductInput{
		Name: p.Name, Type: plan.Type.ValueString(), BillableMetricID: p.BillableMetricID, Tags: p.Tags,
		QuantityConversion: p.QuantityConversion, QuantityRounding: p.QuantityRounding,
		PricingGroupKey: p.PricingGroupKey, PresentationGroupKey: p.PresentationGroupKey,
	})
	if err != nil {
		apiError(&resp.Diagnostics, "create the product", err)
		return
	}
	plan.ID = types.StringValue(id)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *productResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state productModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	got, err := r.data.client.GetProduct(ctx, state.ID.ValueString())
	// An archived product cannot be restored or given a rate: for Terraform
	// it is gone, and the next apply makes a new one.
	if client.IsNotFound(err) || (err == nil && got.ArchivedAt != "") {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		apiError(&resp.Diagnostics, "read product "+state.ID.ValueString(), err)
		return
	}
	state.set(got)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *productResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state productModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	want, have := plan.api(), state.api()
	if !sameProduct(want, have) {
		in := client.ProductUpdate{
			ProductID: state.ID.ValueString(),
			// The start of the current hour: the latest moment Metronome
			// accepts that is not in the future.
			StartingAt: r.data.now().UTC().Truncate(time.Hour).Format(time.RFC3339),
			Name:       want.Name,
		}
		// Only what differs is sent: a field left out keeps its value, and
		// the usage-only fields are refused on other products.
		if want.BillableMetricID != have.BillableMetricID {
			in.BillableMetricID = want.BillableMetricID
		}
		if !sameJSON(want.Tags, have.Tags) {
			in.Tags = listOrEmpty(want.Tags)
		}
		if !sameJSON(want.PricingGroupKey, have.PricingGroupKey) {
			in.PricingGroupKey = listOrEmpty(want.PricingGroupKey)
		}
		if !sameJSON(want.PresentationGroupKey, have.PresentationGroupKey) {
			in.PresentationGroupKey = listOrEmpty(want.PresentationGroupKey)
		}
		if !sameJSON(want.QuantityConversion, have.QuantityConversion) {
			in.QuantityConversion = &want.QuantityConversion
		}
		if !sameJSON(want.QuantityRounding, have.QuantityRounding) {
			in.QuantityRounding = &want.QuantityRounding
		}
		if err := r.data.client.UpdateProduct(ctx, in); err != nil {
			apiError(&resp.Diagnostics, "update product "+state.ID.ValueString(), err)
			return
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y) || (isEmptyJSON(x) && isEmptyJSON(y))
}

func isEmptyJSON(b []byte) bool { return string(b) == "null" || string(b) == "[]" }

// listOrEmpty is a pointer to l, with nil made an empty list: what clears a
// list in an update.
func listOrEmpty(l []string) *[]string {
	if l == nil {
		l = []string{}
	}
	return &l
}

func (r *productResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state productModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.data.client.ArchiveProduct(ctx, state.ID.ValueString()); err != nil && !client.IsNotFound(err) {
		apiError(&resp.Diagnostics, "archive product "+state.ID.ValueString(), err)
	}
}

func (r *productResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if importUUID("product", req, resp) {
		resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
	}
}
