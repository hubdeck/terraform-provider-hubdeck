// Package provider implements the KVault Terraform/OpenTofu provider.
//
// Scope, deliberately narrow (see KVault's own CLAUDE.md, "Terraform/
// OpenTofu provider" section, for the full reasoning): this provider only
// ever authenticates as a Service Token flagged canMintEnrollments — the
// same credential KVault's existing CI self-enrollment path already
// trusts. It has no concept of an "admin" credential and cannot manage
// projects, IP restrictions, or other Service Tokens; /admin/* on a
// KVault instance is entirely gated by Cloudflare Access human SSO and
// deliberately stays that way.
package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/hubdeck/terraform-provider-kvault/internal/kvaultclient"
)

// Ensure the implementation satisfies the expected interfaces.
var (
	_ provider.Provider                       = &kvaultProvider{}
	_ provider.ProviderWithEphemeralResources = &kvaultProvider{}
)

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &kvaultProvider{version: version}
	}
}

type kvaultProvider struct {
	version string
}

type kvaultProviderModel struct {
	Endpoint       types.String `tfsdk:"endpoint"`
	CIClientID     types.String `tfsdk:"ci_client_id"`
	CIClientSecret types.String `tfsdk:"ci_client_secret"`
}

// providerData is what Configure hands to every resource/ephemeral
// resource via resp.ResourceData / resp.EphemeralResourceData.
type providerData struct {
	Client *kvaultclient.Client
}

func (p *kvaultProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "kvault"
	resp.Version = p.version
}

func (p *kvaultProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages machine enrollment codes and reads secrets from a KVault instance (https://github.com/hsuk-io/kvault), authenticated as a canMintEnrollments Service Token. See that project's CLAUDE.md for the full security model this provider deliberately stays within.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional:    true,
				Description: "Base URL of the KVault instance, e.g. https://vault.example.com. Falls back to the KVAULT_ENDPOINT environment variable.",
			},
			"ci_client_id": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "The Cloudflare Access Service Token's Client ID — the token must have canMintEnrollments set by a KVault admin. Falls back to the KVAULT_CI_CLIENT_ID environment variable.",
			},
			"ci_client_secret": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "The Service Token's Client Secret. Falls back to the KVAULT_CI_CLIENT_SECRET environment variable.",
			},
		},
	}
}

func (p *kvaultProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var model kvaultProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}

	endpoint := firstNonEmpty(model.Endpoint.ValueString(), os.Getenv("KVAULT_ENDPOINT"))
	ciClientID := firstNonEmpty(model.CIClientID.ValueString(), os.Getenv("KVAULT_CI_CLIENT_ID"))
	ciClientSecret := firstNonEmpty(model.CIClientSecret.ValueString(), os.Getenv("KVAULT_CI_CLIENT_SECRET"))

	if endpoint == "" {
		resp.Diagnostics.AddError("Missing KVault endpoint", "Set the provider's \"endpoint\" attribute or the KVAULT_ENDPOINT environment variable.")
	}
	if ciClientID == "" {
		resp.Diagnostics.AddError("Missing KVault CI client ID", "Set the provider's \"ci_client_id\" attribute or the KVAULT_CI_CLIENT_ID environment variable.")
	}
	if ciClientSecret == "" {
		resp.Diagnostics.AddError("Missing KVault CI client secret", "Set the provider's \"ci_client_secret\" attribute or the KVAULT_CI_CLIENT_SECRET environment variable.")
	}
	if resp.Diagnostics.HasError() {
		return
	}

	data := &providerData{Client: kvaultclient.New(endpoint, ciClientID, ciClientSecret)}
	resp.ResourceData = data
	resp.EphemeralResourceData = data
}

func (p *kvaultProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewMachineEnrollmentResource,
	}
}

func (p *kvaultProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return nil
}

func (p *kvaultProvider) EphemeralResources(_ context.Context) []func() ephemeral.EphemeralResource {
	return []func() ephemeral.EphemeralResource{
		NewSecretEphemeralResource,
		NewMachineEnrollmentCodeEphemeralResource,
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
