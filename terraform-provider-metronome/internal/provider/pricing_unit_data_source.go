package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*pricingUnitDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*pricingUnitDataSource)(nil)
)

func newPricingUnitDataSource() datasource.DataSource { return &pricingUnitDataSource{} }

type pricingUnitDataSource struct{ data *providerData }

type pricingUnitModel struct {
	ID         types.String `tfsdk:"id"`
	Name       types.String `tfsdk:"name"`
	IsCurrency types.Bool   `tfsdk:"is_currency"`
}

func (d *pricingUnitDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_pricing_unit"
}

func (d *pricingUnitDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A pricing unit (the API calls it a credit type), found by name: a currency such as `USD (cents)`, or a custom unit of the account. " +
			"Its ID is what `fiat_credit_type_id` and `credit_type_id` take.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The unit's name, exactly as Metronome shows it, for example `USD (cents)`.",
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The unit's Metronome ID.",
			},
			"is_currency": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether the unit is a currency, as opposed to a custom pricing unit.",
			},
		},
	}
}

func (d *pricingUnitDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.data = configured(req.ProviderData, &resp.Diagnostics)
}

func (d *pricingUnitDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg pricingUnitModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	units, err := d.data.client.ListCreditTypes(ctx)
	if err != nil {
		apiError(&resp.Diagnostics, "list the pricing units", err)
		return
	}
	var names []string
	for _, u := range units {
		if u.Name == cfg.Name.ValueString() {
			cfg.ID = types.StringValue(u.ID)
			cfg.IsCurrency = types.BoolValue(u.IsCurrency)
			resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
			return
		}
		names = append(names, u.Name)
	}
	resp.Diagnostics.AddError("No such pricing unit",
		fmt.Sprintf("The account has no pricing unit named %q. It has: %s.", cfg.Name.ValueString(), strings.Join(names, ", ")))
}
