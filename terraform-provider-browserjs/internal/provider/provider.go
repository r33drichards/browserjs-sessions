// Package provider is the browserjs Terraform provider, as fixed by
// docs/contracts/policy/terraform-provider.md.
package provider

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/r33drichards/browserjs-sessions/terraform-provider-browserjs/internal/client"
)

const (
	defaultEndpoint = "https://api.computeruse.site"
	envEndpoint     = "BROWSERJS_ENDPOINT"
	envToken        = "BROWSERJS_TOKEN"
)

// New returns the provider's constructor.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &browserjsProvider{version: version, pollEvery: defaultPollEvery}
	}
}

type browserjsProvider struct {
	version string
	// pollEvery is how long to wait before the nth poll (from 0).
	pollEvery func(n int) time.Duration
}

// providerData is what resources and data sources get from Configure.
type providerData struct {
	client    *client.Client
	pollEvery func(n int) time.Duration
}

type providerModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	Token    types.String `tfsdk:"token"`
}

func (p *browserjsProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "browserjs"
	resp.Version = p.version
}

func (p *browserjsProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages browserjs sessions (persistent browsers that agents drive over MCP) and the policies that say which tool calls an agent may make in a session: the browser, desktop control and the shell. " +
			"It signs in with an API token created on the Tokens page of the browserjs UI.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Base URL of the API host, without `/v1`. May be set with the environment variable `BROWSERJS_ENDPOINT`. Defaults to `" + defaultEndpoint + "`.",
			},
			"token": schema.StringAttribute{
				Optional:            true,
				Sensitive:           true,
				MarkdownDescription: "An API token (`bjs_…`). May be set with the environment variable `BROWSERJS_TOKEN`, which keeps it out of the configuration. Sessions need the scopes `sessions:read` and `sessions:write`; policies need `policies:read` and `policies:write`.",
			},
		},
	}
}

func (p *browserjsProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if cfg.Endpoint.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"), "Unknown browserjs endpoint",
			"The endpoint depends on a value that is not known until apply. Set it to a known value, or use the environment variable "+envEndpoint+".")
	}
	if cfg.Token.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("token"), "Unknown browserjs token",
			"The token depends on a value that is not known until apply. Set it to a known value, or use the environment variable "+envToken+".")
	}
	if resp.Diagnostics.HasError() {
		return
	}

	endpoint := firstNonEmpty(cfg.Endpoint.ValueString(), os.Getenv(envEndpoint), defaultEndpoint)
	token := firstNonEmpty(cfg.Token.ValueString(), os.Getenv(envToken))
	if token == "" {
		resp.Diagnostics.AddAttributeError(path.Root("token"), "Missing browserjs token",
			"Set the provider's token attribute or the environment variable "+envToken+". Tokens are created on the Tokens page of the browserjs UI.")
		return
	}
	c, err := client.New(endpoint, token, p.version)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"), "Invalid browserjs endpoint", err.Error())
		return
	}
	if u, err := url.Parse(endpoint); err == nil && u.Scheme == "http" && !isLoopback(u.Hostname()) {
		resp.Diagnostics.AddAttributeWarning(path.Root("endpoint"), "The browserjs endpoint is not https",
			"The API token is sent to "+u.Host+" without encryption.")
	}
	data := &providerData{client: c, pollEvery: p.pollEvery}
	resp.ResourceData = data
	resp.DataSourceData = data
}

func (p *browserjsProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{newSessionResource, newSessionPolicyResource}
}

func (p *browserjsProvider) DataSources(context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{newSessionDataSource, newSessionsDataSource}
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

// defaultPollEvery starts at half a second and doubles to four.
func defaultPollEvery(n int) time.Duration {
	return min(500*time.Millisecond<<min(n, 3), 4*time.Second)
}

// poll calls check until it says done, fails, or ctx ends. check runs once
// before any wait.
func poll(ctx context.Context, every func(int) time.Duration, check func() (bool, error)) error {
	for n := 0; ; n++ {
		done, err := check()
		if err != nil || done {
			return err
		}
		t := time.NewTimer(every(n))
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// apiError adds err as a diagnostic, naming what was being done.
func apiError(diags *diag.Diagnostics, doing string, err error) {
	detail := err.Error()
	switch client.StatusOf(err) {
	case 401:
		detail += "\n\nThe API token is missing, malformed, expired or revoked. Create one on the Tokens page of the browserjs UI and set " + envToken + "."
	case 403:
		detail += "\n\nThe API token lacks a scope this needs."
	}
	diags.AddError("Could not "+doing, detail)
}
