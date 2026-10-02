package provider

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/r33drichards/computer-use/terraform-provider-metronome/internal/client"
)

var (
	_ resource.Resource                     = (*billableMetricResource)(nil)
	_ resource.ResourceWithConfigure        = (*billableMetricResource)(nil)
	_ resource.ResourceWithImportState      = (*billableMetricResource)(nil)
	_ resource.ResourceWithConfigValidators = (*billableMetricResource)(nil)
)

func newBillableMetricResource() resource.Resource { return &billableMetricResource{} }

type billableMetricResource struct{ data *providerData }

type eventTypeFilterModel struct {
	InValues    []string `tfsdk:"in_values"`
	NotInValues []string `tfsdk:"not_in_values"`
}

type propertyFilterModel struct {
	Name        types.String `tfsdk:"name"`
	Exists      types.Bool   `tfsdk:"exists"`
	InValues    []string     `tfsdk:"in_values"`
	NotInValues []string     `tfsdk:"not_in_values"`
}

type billableMetricModel struct {
	ID              types.String          `tfsdk:"id"`
	Name            types.String          `tfsdk:"name"`
	EventTypeFilter *eventTypeFilterModel `tfsdk:"event_type_filter"`
	PropertyFilters []propertyFilterModel `tfsdk:"property_filters"`
	AggregationType types.String          `tfsdk:"aggregation_type"`
	AggregationKey  types.String          `tfsdk:"aggregation_key"`
	GroupKeys       [][]string            `tfsdk:"group_keys"`
	SQL             types.String          `tfsdk:"sql"`
	CustomFields    map[string]string     `tfsdk:"custom_fields"`
}

// api is the metric as the create call takes it.
func (m *billableMetricModel) api() client.BillableMetric {
	out := client.BillableMetric{
		Name:            m.Name.ValueString(),
		AggregationType: m.AggregationType.ValueString(),
		AggregationKey:  m.AggregationKey.ValueString(),
		GroupKeys:       m.GroupKeys,
		SQL:             m.SQL.ValueString(),
		CustomFields:    m.CustomFields,
	}
	if f := m.EventTypeFilter; f != nil {
		out.EventTypeFilter = &client.EventTypeFilter{InValues: f.InValues, NotInValues: f.NotInValues}
	}
	for _, f := range m.PropertyFilters {
		out.PropertyFilters = append(out.PropertyFilters, client.PropertyFilter{
			Name: f.Name.ValueString(), Exists: f.Exists.ValueBoolPointer(), InValues: f.InValues, NotInValues: f.NotInValues,
		})
	}
	return out
}

// set writes what the API returned. Where that says the same as the state
// already does, the state is kept as written: the API does not tell an empty
// list from an absent one, and returns the aggregation in upper case.
func (m *billableMetricModel) set(got *client.BillableMetric) {
	m.ID = types.StringValue(got.ID)
	m.Name = types.StringValue(got.Name)
	if sameDefinition(m.api(), *got) {
		return
	}
	m.AggregationType = optionalString(strings.ToUpper(got.AggregationType))
	m.AggregationKey = optionalString(got.AggregationKey)
	m.SQL = optionalString(got.SQL)
	m.GroupKeys = got.GroupKeys
	m.CustomFields = got.CustomFields
	m.EventTypeFilter = nil
	if f := got.EventTypeFilter; f != nil {
		m.EventTypeFilter = &eventTypeFilterModel{InValues: f.InValues, NotInValues: f.NotInValues}
	}
	m.PropertyFilters = nil
	for _, f := range got.PropertyFilters {
		m.PropertyFilters = append(m.PropertyFilters, propertyFilterModel{
			Name: types.StringValue(f.Name), Exists: types.BoolPointerValue(f.Exists), InValues: f.InValues, NotInValues: f.NotInValues,
		})
	}
}

// sameDefinition compares everything about two metrics but their name, ID
// and archival, ignoring the differences JSON's omitempty erases.
func sameDefinition(a, b client.BillableMetric) bool {
	norm := func(m client.BillableMetric) string {
		m.ID, m.Name, m.ArchivedAt = "", "", ""
		m.AggregationType = strings.ToUpper(m.AggregationType)
		if f := m.EventTypeFilter; f != nil && len(f.InValues) == 0 && len(f.NotInValues) == 0 {
			m.EventTypeFilter = nil
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	return norm(a) == norm(b)
}

func (r *billableMetricResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_billable_metric"
}

func (r *billableMetricResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replaceStrings := []planmodifier.List{listplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "A billable metric: a query that filters and aggregates usage events into a quantity that can be priced.\n\n" +
			"~> **Only the name can change.** Metronome fixes a metric's definition when it is created, so changing anything else **replaces** the metric: a new one is created and the old one archived. " +
			"A product that used the old metric keeps metering with it until it is pointed at the new one, which `metronome_product` does in place.\n\n" +
			"~> **Destroying a metric archives it.** Metronome has no delete. An archived metric still meters for the products that already use it and cannot be given to another; it cannot be restored.\n\n" +
			"Define the metric either with `aggregation_type` and the filters (a streaming metric), or with `sql`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The metric's Metronome ID.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The display name. Changed in place.",
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"aggregation_type": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "How matching events are aggregated: `COUNT`, `LATEST`, `MAX`, `SUM` or `UNIQUE`. Changing it replaces the metric.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:          []validator.String{stringvalidator.OneOf("COUNT", "LATEST", "MAX", "SUM", "UNIQUE")},
			},
			"aggregation_key": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The event property that is aggregated. It must be the `name` of one of `property_filters`. Not used with `COUNT`. Changing it replaces the metric.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"event_type_filter": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Which events match, by their `event_type`. Changing it replaces the metric.",
				PlanModifiers:       []planmodifier.Object{objectplanmodifier.RequiresReplace()},
				Attributes: map[string]schema.Attribute{
					"in_values": schema.ListAttribute{
						Optional:            true,
						ElementType:         types.StringType,
						MarkdownDescription: "Only events of these types match.",
						Validators:          []validator.List{listvalidator.SizeAtLeast(1)},
					},
					"not_in_values": schema.ListAttribute{
						Optional:            true,
						ElementType:         types.StringType,
						MarkdownDescription: "Events of these types do not match.",
						Validators:          []validator.List{listvalidator.SizeAtLeast(1)},
					},
				},
			},
			"property_filters": schema.ListNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Rules on the properties of an event; all must pass for the event to match. Changing them replaces the metric.",
				PlanModifiers:       []planmodifier.List{listplanmodifier.RequiresReplace()},
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							Required:            true,
							MarkdownDescription: "The name of the event property.",
						},
						"exists": schema.BoolAttribute{
							Optional:            true,
							MarkdownDescription: "`true`: only events that have the property match. `false`: only events that do not.",
						},
						"in_values": schema.ListAttribute{
							Optional:            true,
							ElementType:         types.StringType,
							MarkdownDescription: "The property's value must be one of these.",
						},
						"not_in_values": schema.ListAttribute{
							Optional:            true,
							ElementType:         types.StringType,
							MarkdownDescription: "The property's value must be none of these.",
						},
					},
				},
			},
			"group_keys": schema.ListAttribute{
				Optional:            true,
				ElementType:         types.ListType{ElemType: types.StringType},
				MarkdownDescription: "Sets of property names by which usage can be grouped, for pricing or for presentation on an invoice. At most five. Changing them replaces the metric.",
				PlanModifiers:       replaceStrings,
				Validators:          []validator.List{listvalidator.SizeAtMost(5)},
			},
			"sql": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The query of a SQL billable metric. Excludes `aggregation_type`, `aggregation_key`, the filters and `group_keys`. Changing it replaces the metric.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"custom_fields": schema.MapAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Custom fields, set when the metric is created. The keys must already exist in the account. Changing them replaces the metric.",
				PlanModifiers:       []planmodifier.Map{mapplanmodifier.RequiresReplace()},
			},
		},
	}
}

func (r *billableMetricResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{metricDefinitionValidator{}}
}

// metricDefinitionValidator holds a metric to one of its two forms.
type metricDefinitionValidator struct{}

func (metricDefinitionValidator) Description(context.Context) string {
	return "a billable metric has either aggregation_type or sql"
}

func (v metricDefinitionValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (metricDefinitionValidator) ValidateResource(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var aggregation, key, sql types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("aggregation_type"), &aggregation)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("aggregation_key"), &key)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("sql"), &sql)...)
	if resp.Diagnostics.HasError() || aggregation.IsUnknown() || key.IsUnknown() || sql.IsUnknown() {
		return
	}
	switch {
	case aggregation.IsNull() && sql.IsNull():
		resp.Diagnostics.AddError("The metric has no definition", "Set aggregation_type (with the filters), or sql.")
	case !aggregation.IsNull() && !sql.IsNull():
		resp.Diagnostics.AddAttributeError(path.Root("sql"), "The metric has two definitions", "sql excludes aggregation_type, aggregation_key, the filters and group_keys.")
	case !aggregation.IsNull() && aggregation.ValueString() != "COUNT" && key.IsNull():
		resp.Diagnostics.AddAttributeError(path.Root("aggregation_key"), "Missing aggregation_key",
			aggregation.ValueString()+" aggregates a property of the event: name it in aggregation_key, and give it a property filter with exists = true.")
	}
}

func (r *billableMetricResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = configured(req.ProviderData, &resp.Diagnostics)
}

func (r *billableMetricResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan billableMetricModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := r.data.client.CreateBillableMetric(ctx, plan.api())
	if err != nil {
		apiError(&resp.Diagnostics, "create the billable metric", err)
		return
	}
	plan.ID = types.StringValue(id)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *billableMetricResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state billableMetricModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	got, err := r.data.client.GetBillableMetric(ctx, state.ID.ValueString())
	// An archived metric cannot be restored or given to a product: for
	// Terraform it is gone, and the next apply makes a new one.
	if client.IsNotFound(err) || (err == nil && got.ArchivedAt != "") {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		apiError(&resp.Diagnostics, "read billable metric "+state.ID.ValueString(), err)
		return
	}
	state.set(got)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *billableMetricResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state billableMetricModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Everything but the name replaces the resource, so this is a rename.
	if !plan.Name.Equal(state.Name) {
		if err := r.data.client.RenameBillableMetric(ctx, state.ID.ValueString(), plan.Name.ValueString()); err != nil {
			apiError(&resp.Diagnostics, "rename billable metric "+state.ID.ValueString(), err)
			return
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *billableMetricResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state billableMetricModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.data.client.ArchiveBillableMetric(ctx, state.ID.ValueString()); err != nil && !client.IsNotFound(err) {
		apiError(&resp.Diagnostics, "archive billable metric "+state.ID.ValueString(), err)
	}
}

func (r *billableMetricResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if importUUID("billable metric", req, resp) {
		resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
	}
}
