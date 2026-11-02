package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/hubdeck/terraform-provider-kvault/internal/kvaultclient"
)

var (
	_ ephemeral.EphemeralResource              = &secretEphemeralResource{}
	_ ephemeral.EphemeralResourceWithConfigure = &secretEphemeralResource{}
)

func NewSecretEphemeralResource() ephemeral.EphemeralResource {
	return &secretEphemeralResource{}
}

type secretEphemeralResource struct {
	client *kvaultclient.Client
}

type secretEphemeralModel struct {
	Name      types.String `tfsdk:"name"`
	Namespace types.String `tfsdk:"namespace"`
	Value     types.String `tfsdk:"value"`
}

func (e *secretEphemeralResource) Metadata(_ context.Context, req ephemeral.MetadataRequest, resp *ephemeral.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_secret"
}

func (e *secretEphemeralResource) Schema(_ context.Context, _ ephemeral.SchemaRequest, resp *ephemeral.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Reads a KVault secret via the ordinary Service-Token-authenticated GET /api/secret/:key route — the same access path a Kubernetes/ESO caller uses. The value exists only for the duration of the apply that opens it; it is never written to plan or state. This is the primary path for feeding a real credential to with-secrets.sh-style CI steps or a provisioner, without that value ever touching a .tfstate file.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required:    true,
				Description: "The secret's key, as registered in KVault.",
			},
			"namespace": schema.StringAttribute{
				Optional:    true,
				Description: "Namespace to request, if the secret's ACL requires one (SecretAcl.allowedNamespaces) — must be one this provider's Service Token was itself issued.",
			},
			"value": schema.StringAttribute{
				Computed:    true,
				Sensitive:   true,
				Description: "The decrypted secret value. Ephemeral by definition — never persisted anywhere by Terraform.",
			},
		},
	}
}

func (e *secretEphemeralResource) Configure(_ context.Context, req ephemeral.ConfigureRequest, resp *ephemeral.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data type", fmt.Sprintf("Expected *providerData, got: %T", req.ProviderData))
		return
	}
	e.client = data.Client
}

func (e *secretEphemeralResource) Open(ctx context.Context, req ephemeral.OpenRequest, resp *ephemeral.OpenResponse) {
	var model secretEphemeralModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}

	value, err := e.client.FetchSecret(model.Name.ValueString(), model.Namespace.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error fetching secret", err.Error())
		return
	}

	model.Value = types.StringValue(value)
	resp.Diagnostics.Append(resp.Result.Set(ctx, &model)...)
}
