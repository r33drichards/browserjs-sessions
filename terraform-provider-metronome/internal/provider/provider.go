// Package provider is the Metronome Terraform provider: the pricing
// configuration of a Metronome account, as code.
package provider

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/r33drichards/browserjs-sessions/terraform-provider-metronome/internal/client"
)

const (
	defaultEndpoint = "https://api.metronome.com"
	envEndpoint     = "METRONOME_ENDPOINT"
	// The name Metronome's own SDKs read.
	envToken = "METRONOME_BEARER_TOKEN"
)

// New returns the provider's constructor.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &metronomeProvider{version: version, now: time.Now}
	}
}

type metronomeProvider struct {
	version string
	now     func() time.Time
	// retryWait, when set, replaces the client's wait before a retry.
	retryWait func(n int) time.Duration
}

// providerData is what resources and data sources get from Configure.
type providerData struct {
	client *client.Client
	now    func() time.Time
}

type providerModel struct {
	Endpoint    types.String `tfsdk:"endpoint"`
	BearerToken types.String `tfsdk:"bearer_token"`
}

func (p *metronomeProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "metronome"
	resp.Version = p.version
}

func (p *metronomeProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the pricing configuration of a [Metronome](https://docs.metronome.com) account: billable metrics, products, rate cards, rates, threshold notifications and custom field keys. " +
			"Customers, contracts, commits and credits are created by an application at run time and are not managed here.\n\n" +
			"Most of Metronome's configuration cannot be deleted, and much of it cannot be changed: destroying a resource **archives** it, and changing an attribute Metronome holds fixed **replaces** the resource. Each resource says what it does.\n\n" +
			"A token belongs to one environment of the account (sandbox or production); so does everything it creates.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Base URL of the API. May be set with the environment variable `METRONOME_ENDPOINT`. Defaults to `" + defaultEndpoint + "`.",
			},
			"bearer_token": schema.StringAttribute{
				Optional:            true,
				Sensitive:           true,
				MarkdownDescription: "An API token, created in the Metronome app under Developer, API tokens. May be set with the environment variable `METRONOME_BEARER_TOKEN`, which keeps it out of the configuration.",
			},
		},
	}
}

func (p *metronomeProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if cfg.Endpoint.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"), "Unknown Metronome endpoint",
			"The endpoint depends on a value that is not known until apply. Set it to a known value, or use the environment variable "+envEndpoint+".")
	}
	if cfg.BearerToken.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("bearer_token"), "Unknown Metronome token",
			"The token depends on a value that is not known until apply. Set it to a known value, or use the environment variable "+envToken+".")
	}
	if resp.Diagnostics.HasError() {
		return
	}

	endpoint := firstNonEmpty(cfg.Endpoint.ValueString(), os.Getenv(envEndpoint), defaultEndpoint)
	token := firstNonEmpty(cfg.BearerToken.ValueString(), os.Getenv(envToken))
	if token == "" {
		resp.Diagnostics.AddAttributeError(path.Root("bearer_token"), "Missing Metronome token",
			"Set the provider's bearer_token attribute or the environment variable "+envToken+". Tokens are created in the Metronome app under Developer, API tokens.")
		return
	}
	c, err := client.New(endpoint, token, p.version)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"), "Invalid Metronome endpoint", err.Error())
		return
	}
	if p.retryWait != nil {
		c.SetRetryWait(p.retryWait)
	}
	if u, err := url.Parse(endpoint); err == nil && u.Scheme == "http" && !isLoopback(u.Hostname()) {
		resp.Diagnostics.AddAttributeWarning(path.Root("endpoint"), "The Metronome endpoint is not https",
			"The API token is sent to "+u.Host+" without encryption.")
	}
	data := &providerData{client: c, now: p.now}
	resp.ResourceData = data
	resp.DataSourceData = data
}

func (p *metronomeProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		newBillableMetricResource, newProductResource, newRateCardResource, newRateResource, newAlertResource, newCustomFieldKeyResource,
	}
}

func (p *metronomeProvider) DataSources(context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{newPricingUnitDataSource}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// configured hands a resource or data source the provider's data. It is nil
// before the provider is configured (during validation), which is not an
// error.
func configured(v any, diags *diag.Diagnostics) *providerData {
	if v == nil {
		return nil
	}
	d, ok := v.(*providerData)
	if !ok {
		diags.AddError("Unexpected provider data", fmt.Sprintf("Got %T. This is a bug in the provider.", v))
		return nil
	}
	return d
}

// apiError adds err as a diagnostic, naming what was being done.
func apiError(diags *diag.Diagnostics, doing string, err error) {
	detail := err.Error()
	switch client.StatusOf(err) {
	case 401, 403:
		detail += "\n\nThe API token is missing, wrong or revoked. Create one in the Metronome app under Developer, API tokens, and set " + envToken + "."
	case 429:
		detail += "\n\nMetronome's rate limit was still exceeded after several retries. Run again, or lower -parallelism."
	}
	diags.AddError("Could not "+doing, detail)
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// isUUID validates a Metronome ID.
func isUUID() validator.String {
	return stringvalidator.RegexMatches(uuidPattern, "must be a Metronome ID (a UUID)")
}

// importUUID checks an import ID before it is put in the state.
func importUUID(what string, req resource.ImportStateRequest, resp *resource.ImportStateResponse) bool {
	if !uuidPattern.MatchString(req.ID) {
		resp.Diagnostics.AddError("Not a Metronome ID", fmt.Sprintf("%q is not a UUID. Import a %s by its Metronome ID.", req.ID, what))
		return false
	}
	return true
}

// timestamp validates an RFC 3339 time.
type timestampValidator struct{ hourly bool }

func (v timestampValidator) Description(context.Context) string {
	if v.hourly {
		return "must be an RFC 3339 timestamp on an hour boundary, such as 2026-01-01T00:00:00Z"
	}
	return "must be an RFC 3339 timestamp, such as 2026-01-01T00:00:00Z"
}

func (v timestampValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v timestampValidator) ValidateString(ctx context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	t, err := time.Parse(time.RFC3339, req.ConfigValue.ValueString())
	if err != nil || (v.hourly && !t.Equal(t.Truncate(time.Hour))) {
		resp.Diagnostics.AddAttributeError(req.Path, "Not a timestamp Metronome accepts",
			fmt.Sprintf("%q %s.", req.ConfigValue.ValueString(), v.Description(ctx)))
	}
}

// sameInstant reports whether two RFC 3339 timestamps name the same moment,
// however they are written. Two empty strings are the same.
func sameInstant(a, b string) bool {
	if a == b {
		return true
	}
	ta, errA := time.Parse(time.RFC3339, a)
	tb, errB := time.Parse(time.RFC3339, b)
	return errA == nil && errB == nil && ta.Equal(tb)
}

// keepInstant is what to put in the state for a timestamp the API returned:
// the prior value when it names the same moment (the API rewrites
// timestamps with milliseconds), and the API's otherwise. Empty is null.
func keepInstant(prior types.String, api string) types.String {
	if !prior.IsNull() && !prior.IsUnknown() && sameInstant(prior.ValueString(), api) {
		return prior
	}
	return optionalString(api)
}

// optionalString is null for "".
func optionalString(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}

// stringsOf reads a list of strings; null and unknown are nil.
func stringsOf(ctx context.Context, l types.List, diags *diag.Diagnostics) []string {
	if l.IsNull() || l.IsUnknown() {
		return nil
	}
	var out []string
	diags.Append(l.ElementsAs(ctx, &out, false)...)
	return out
}

// keepList is what to put in the state for a list of strings the API
// returned. An absent or empty list from the API keeps a prior null or
// empty list as it was: the API does not tell the two apart.
func keepList(prior types.List, api []string) types.List {
	if len(api) == 0 && (prior.IsNull() || len(prior.Elements()) == 0) && !prior.IsUnknown() {
		return prior
	}
	elems := make([]attr.Value, len(api))
	for i, s := range api {
		elems[i] = types.StringValue(s)
	}
	return types.ListValueMust(types.StringType, elems)
}
