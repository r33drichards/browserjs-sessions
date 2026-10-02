package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/float64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/r33drichards/computer-use/terraform-provider-metronome/internal/client"
)

var (
	_ resource.Resource              = (*alertResource)(nil)
	_ resource.ResourceWithConfigure = (*alertResource)(nil)
)

// alertTypes are the threshold notification types of the API.
var alertTypes = []string{
	"spend_threshold_reached",
	"monthly_invoice_total_spend_threshold_reached",
	"usage_threshold_reached",
	"low_remaining_days_for_commit_segment_reached",
	"low_remaining_commit_balance_reached",
	"low_remaining_commit_percentage_reached",
	"low_remaining_days_for_contract_credit_segment_reached",
	"low_remaining_contract_credit_balance_reached",
	"low_remaining_contract_credit_percentage_reached",
	"low_remaining_contract_credit_and_commit_balance_reached",
	"low_remaining_contract_credit_and_commit_percentage_reached",
	"invoice_total_reached",
}

func newAlertResource() resource.Resource { return &alertResource{} }

type alertResource struct{ data *providerData }

type alertModel struct {
	ID                     types.String  `tfsdk:"id"`
	Name                   types.String  `tfsdk:"name"`
	AlertType              types.String  `tfsdk:"alert_type"`
	Threshold              types.Float64 `tfsdk:"threshold"`
	CreditTypeID           types.String  `tfsdk:"credit_type_id"`
	CustomerID             types.String  `tfsdk:"customer_id"`
	BillableMetricID       types.String  `tfsdk:"billable_metric_id"`
	UniquenessKey          types.String  `tfsdk:"uniqueness_key"`
	EvaluateOnCreate       types.Bool    `tfsdk:"evaluate_on_create"`
	CreditGrantTypeFilters []string      `tfsdk:"credit_grant_type_filters"`
	ReleaseUniquenessKey   types.Bool    `tfsdk:"release_uniqueness_key_on_destroy"`
}

func (r *alertResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_alert"
}

func (r *alertResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replaceString := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "A threshold notification (the API calls it an alert): Metronome sends a webhook when a customer's spend, usage or remaining balance crosses a threshold. Without `customer_id` it applies to every customer.\n\n" +
			"Where the webhook goes is not part of the API: a webhook destination is added by hand in the Metronome app (Developer, Notifications, Webhooks).\n\n" +
			"~> **Nothing about a notification can change**; changing any attribute replaces it: the old one is archived and a new one created.\n\n" +
			"~> **Destroying a notification archives it.** It stops evaluating at once and cannot be re-enabled.\n\n" +
			"~> **Metronome's API cannot read a notification except through a customer.** A notification for all customers is therefore never refreshed: one archived in the Metronome app stays in the state as if it were live. With `customer_id` set, it is read, and re-created if it was archived. For the same reason **import is not supported**.\n\n" +
			"Seat-balance notifications, custom-field filters and group filters are not supported by this resource.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The notification's Metronome ID. Webhooks carry it as `alert_id`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The name. Webhooks carry it as `alert_name`.",
				PlanModifiers:       replaceString,
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"alert_type": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "What is watched, for example `low_remaining_contract_credit_and_commit_balance_reached` or `spend_threshold_reached`.",
				PlanModifiers:       replaceString,
				Validators:          []validator.String{stringvalidator.OneOf(alertTypes...)},
			},
			"threshold": schema.Float64Attribute{
				Required:            true,
				MarkdownDescription: "The threshold. By type: an amount in the pricing unit (USD: cents), a number of days, or a percentage.",
				PlanModifiers:       []planmodifier.Float64{float64planmodifier.RequiresReplace()},
			},
			"credit_type_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The pricing unit of the threshold, for the types that have one. Left out, Metronome uses USD (cents).",
				PlanModifiers:       replaceString,
				Validators:          []validator.String{isUUID()},
			},
			"customer_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The one customer the notification is for. Left out, it is for every customer.",
				PlanModifiers:       replaceString,
				Validators:          []validator.String{isUUID()},
			},
			"billable_metric_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "For `usage_threshold_reached`: the metric whose usage is watched.",
				PlanModifiers:       replaceString,
				Validators:          []validator.String{isUUID()},
			},
			"uniqueness_key": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "A key Metronome refuses to see twice (409): it keeps an apply that is run again after a lost answer from creating a second notification.",
				PlanModifiers:       replaceString,
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"evaluate_on_create": schema.BoolAttribute{
				Optional:            true,
				MarkdownDescription: "Whether customers who are already past the threshold are notified when the notification is created. Left out, Metronome's default (`true`).",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
			"credit_grant_type_filters": schema.ListAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "For the low-credit-balance types: only credit grants of these types count.",
				PlanModifiers:       []planmodifier.List{listplanmodifier.RequiresReplace()},
			},
			"release_uniqueness_key_on_destroy": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "Whether archiving frees `uniqueness_key` for another notification. Defaults to `true`, without which a replaced notification could not keep its key. Changed in place.",
			},
		},
	}
}

func (r *alertResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.data = configured(req.ProviderData, &resp.Diagnostics)
}

func (r *alertResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan alertModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := r.data.client.CreateAlert(ctx, client.AlertInput{
		AlertType:              plan.AlertType.ValueString(),
		Name:                   plan.Name.ValueString(),
		Threshold:              plan.Threshold.ValueFloat64(),
		CreditTypeID:           plan.CreditTypeID.ValueString(),
		CustomerID:             plan.CustomerID.ValueString(),
		BillableMetricID:       plan.BillableMetricID.ValueString(),
		UniquenessKey:          plan.UniquenessKey.ValueString(),
		EvaluateOnCreate:       plan.EvaluateOnCreate.ValueBoolPointer(),
		CreditGrantTypeFilters: plan.CreditGrantTypeFilters,
	})
	if err != nil {
		if client.StatusOf(err) == 409 {
			resp.Diagnostics.AddError("The uniqueness key is taken",
				err.Error()+"\n\nA threshold notification with uniqueness_key "+plan.UniquenessKey.String()+" exists already, perhaps from an apply whose answer was lost. Archive it in the Metronome app (releasing its key), or choose another key.")
			return
		}
		apiError(&resp.Diagnostics, "create the threshold notification", err)
		return
	}
	plan.ID = types.StringValue(id)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *alertResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state alertModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// The API reads a notification only through a customer. Without one the
	// state is all there is.
	if state.CustomerID.IsNull() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		return
	}
	got, err := r.data.client.CustomerAlert(ctx, state.CustomerID.ValueString(), state.ID.ValueString())
	if client.IsNotFound(err) || (err == nil && got.Status == "archived") {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		apiError(&resp.Diagnostics, "read threshold notification "+state.ID.ValueString(), err)
		return
	}
	state.Name = types.StringValue(got.Name)
	state.AlertType = types.StringValue(got.Type)
	state.Threshold = types.Float64Value(got.Threshold)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is reached only for release_uniqueness_key_on_destroy, which is
// the provider's own setting.
func (r *alertResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan alertModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *alertResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state alertModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	err := r.data.client.ArchiveAlert(ctx, state.ID.ValueString(), state.ReleaseUniquenessKey.ValueBool())
	if err != nil && !client.IsNotFound(err) {
		apiError(&resp.Diagnostics, "archive threshold notification "+state.ID.ValueString(), err)
	}
}
