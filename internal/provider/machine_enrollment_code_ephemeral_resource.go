package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/hubdeck/terraform-provider-kvault/internal/kvaultclient"
)

var (
	_ ephemeral.EphemeralResource              = &machineEnrollmentCodeEphemeralResource{}
	_ ephemeral.EphemeralResourceWithConfigure = &machineEnrollmentCodeEphemeralResource{}
)

func NewMachineEnrollmentCodeEphemeralResource() ephemeral.EphemeralResource {
	return &machineEnrollmentCodeEphemeralResource{}
}

type machineEnrollmentCodeEphemeralResource struct {
	client *kvaultclient.Client
}

// machineEnrollmentCodeEphemeralModel — see machine_enrollment_resource.go's
// package comment for WHY minting lives here rather than on that
// resource: an ephemeral resource's Open() result is never persisted to
// state at all, so there is no write-only-nullification problem to work
// around — Result.Set's value really does flow to whatever referenced it
// (another ephemeral/write-only sink, or a provisioner) within that one
// apply.
//
// Every Open() mints a FRESH code — there is no "generation" counter to
// bump here, because unlike a managed resource's Update, an ephemeral
// resource has no state to diff against in the first place: referencing
// it at all means "I want a code right now," full stop. Pass key_id
// (from the matching kvault_machine_enrollment resource's own key_id,
// once known) to mint a REKEY-mode code against that exact identity;
// leave it unset for a fresh (create-mode) enrollment, or to let the
// server self-resolve by name (same 0/1/ambiguous logic as the
// underlying enroll-code API).
type machineEnrollmentCodeEphemeralModel struct {
	Name             types.String `tfsdk:"name"`
	KeyID            types.String `tfsdk:"key_id"`
	TTL              types.String `tfsdk:"ttl"`
	BindSourceIP     types.String `tfsdk:"bind_source_ip"`
	Code             types.String `tfsdk:"code"`
	Mode             types.String `tfsdk:"mode"`
	ExpiresInSeconds types.Int64  `tfsdk:"expires_in_seconds"`
}

func (e *machineEnrollmentCodeEphemeralResource) Metadata(_ context.Context, req ephemeral.MetadataRequest, resp *ephemeral.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_machine_enrollment_code"
}

func (e *machineEnrollmentCodeEphemeralResource) Schema(_ context.Context, _ ephemeral.SchemaRequest, resp *ephemeral.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Mints a single-use KVault machine enrollment code on every open — never written to plan or state. Pair with kvault_machine_enrollment for identity tracking and revoke-on-destroy; reference THIS resource wherever a host or operator script actually needs a code (initial enrollment, or a later rekey), e.g. from a provisioner block.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required:    true,
				Description: "The machine identity this code is bound to (matches [a-zA-Z0-9_.-]+).",
			},
			"key_id": schema.StringAttribute{
				Optional:    true,
				Description: "Mint a rekey-mode code against this exact identity (from kvault_machine_enrollment.<name>.key_id). Leave unset for a fresh enrollment, or to let the server resolve by name itself.",
			},
			"ttl": schema.StringAttribute{
				Optional:    true,
				Description: "How long the minted code remains redeemable, as a Go duration string (e.g. \"10m\", \"1h\"). Server-enforced bounds apply and differ for a fresh enrollment vs. a rekey — see KVault's CLAUDE.md. Omit to use the server's own mode-appropriate default.",
			},
			"bind_source_ip": schema.StringAttribute{
				Optional:    true,
				Description: "Optional bare IP this code's redemption is restricted to, if you already know the host's address ahead of time.",
			},
			"code": schema.StringAttribute{
				Computed:    true,
				Sensitive:   true,
				Description: "The freshly minted one-time code. Ephemeral by definition — never persisted anywhere by Terraform. Reference it only from a provisioner or another ephemeral/write-only context within this same apply.",
			},
			"mode": schema.StringAttribute{
				Computed:    true,
				Description: "\"create\" or \"rekey\" — which mode the server actually minted, for logging/debugging. Not sensitive.",
			},
			"expires_in_seconds": schema.Int64Attribute{
				Computed:    true,
				Description: "How long the minted code is actually valid for, as resolved by the server (may differ from a requested ttl if it fell outside the server's bounds for this mode).",
			},
		},
	}
}

func (e *machineEnrollmentCodeEphemeralResource) Configure(_ context.Context, req ephemeral.ConfigureRequest, resp *ephemeral.ConfigureResponse) {
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

func (e *machineEnrollmentCodeEphemeralResource) Open(ctx context.Context, req ephemeral.OpenRequest, resp *ephemeral.OpenResponse) {
	var model machineEnrollmentCodeEphemeralModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}

	seconds, err := ttlSeconds(model.TTL)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("ttl"), "Invalid ttl", err.Error())
		return
	}

	result, err := e.client.EnrollCode(model.Name.ValueString(), model.KeyID.ValueString(), seconds, model.BindSourceIP.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error minting enrollment code", err.Error())
		return
	}

	model.Code = types.StringValue(result.Code)
	model.Mode = types.StringValue(result.Mode)
	model.ExpiresInSeconds = types.Int64Value(int64(result.ExpiresInSeconds))
	if result.KeyID != "" {
		model.KeyID = types.StringValue(result.KeyID)
	}

	resp.Diagnostics.Append(resp.Result.Set(ctx, &model)...)
}

func ttlSeconds(ttl types.String) (int, error) {
	if ttl.IsNull() || ttl.IsUnknown() || ttl.ValueString() == "" {
		return 0, nil // let the server apply its own mode-appropriate default
	}
	d, err := time.ParseDuration(ttl.ValueString())
	if err != nil {
		return 0, fmt.Errorf("ttl must be a Go duration string (e.g. \"10m\", \"1h\"): %w", err)
	}
	return int(d.Seconds()), nil
}
