package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/hubdeck/terraform-provider-hubdeck/internal/kvaultclient"
)

var (
	_ resource.Resource                = &machineEnrollmentResource{}
	_ resource.ResourceWithConfigure   = &machineEnrollmentResource{}
	_ resource.ResourceWithImportState = &machineEnrollmentResource{}
)

func NewMachineEnrollmentResource() resource.Resource {
	return &machineEnrollmentResource{}
}

type machineEnrollmentResource struct {
	client *kvaultclient.Client
}

// machineEnrollmentModel is deliberately minimal: this resource ONLY
// tracks a machine identity's lifecycle (resolve its key_id once the
// host enrolls; revoke that key_id on destroy) — it mints NO codes
// itself. An earlier version tried to mint a one-time code here via a
// write-only "code" attribute recomputed on every Update, but that is
// structurally impossible: terraform-plugin-framework's
// NullifyWriteOnlyAttributes nulls every write-only attribute value in
// resp.NewState right after Create/Update return — BEFORE the RPC
// response is even sent to Terraform Core — specifically so a provider
// can never use a write-only attribute as an output channel. (Write-only
// attributes exist for the opposite direction: a value the PRACTITIONER
// supplies in config, which the provider reads during that one apply and
// never persists — e.g. a password-rotation resource's "new password"
// input. A provider cannot use the same mechanism to hand a
// provider-generated secret back out.) Minting now lives entirely in the
// hubdeck_kvault_machine_enrollment_code EPHEMERAL resource (see that file),
// whose Open() result is never subject to this nullification at all —
// ephemeral resources have no persisted state in the first place. See
// KVault's CLAUDE.md, "Fleet rebuilds (rekey)" section, for the
// server-side design both resources here implement.
type machineEnrollmentModel struct {
	Name  types.String `tfsdk:"name"`
	KeyID types.String `tfsdk:"key_id"`
}

func (r *machineEnrollmentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_kvault_machine_enrollment"
}

func (r *machineEnrollmentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Tracks a KVault machine identity's lifecycle — resolving its key_id once a host enrolls under this name, and revoking that key_id on destroy. Mints no codes itself: use the hubdeck_kvault_machine_enrollment_code ephemeral resource (referencing this resource's key_id) whenever a host actually needs one, including a later rekey.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required:    true,
				Description: "The machine identity to track (matches [a-zA-Z0-9_.-]+). Changing this destroys the old identity (revoking its key) and starts tracking a fresh one under the new name — it is NOT a rename.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"key_id": schema.StringAttribute{
				Computed:    true,
				Description: "The KVault machine key ID this identity resolved to, once a host has actually redeemed a code minted for this name. Empty until then — not an error, just not-yet-enrolled. Also reverts to empty if the key is revoked out of band (e.g. by an admin), rather than staying stale.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *machineEnrollmentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data type", fmt.Sprintf("Expected *providerData, got: %T", req.ProviderData))
		return
	}
	r.client = data.Client
}

// Create makes no API call at all — there is nothing to provision until
// a code is actually minted (via the ephemeral resource) and a host
// redeems it, asynchronously, at some later point. This resource's job
// starts with tracking that identity's key_id (via Read) and ends with
// revoking it (via Delete); Create just records the name.
func (r *machineEnrollmentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var model machineEnrollmentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}

	model.KeyID = types.StringValue("")
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

func (r *machineEnrollmentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var model machineEnrollmentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}

	keyID, err := r.client.ResolveMachineKey(model.Name.ValueString())
	if err != nil {
		if kvaultclient.IsNotFound(err) {
			// Not enrolled (yet, or not anymore — see below), either way
			// not an error. Explicitly clear key_id rather than leaving a
			// stale value from a PRIOR successful resolve: if the key was
			// revoked out of band (e.g. by an admin via the GUI, or by a
			// DIFFERENT apply's Delete for the same name) since the last
			// Read, this resource's state must reflect that, not keep
			// reporting a key_id that no longer exists server-side.
			model.KeyID = types.StringValue("")
			resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
			return
		}
		resp.Diagnostics.AddError("Error resolving machine enrollment", err.Error())
		return
	}

	model.KeyID = types.StringValue(keyID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

// Update is only ever reached for in-place changes to attributes that
// aren't RequiresReplace — currently none exist on this schema besides
// name (which IS RequiresReplace) and the Computed key_id, so this
// should never actually be invoked by Terraform core in practice. It
// exists only to satisfy the resource.Resource interface; implemented as
// a Read-equivalent rather than a no-op, in case a future schema
// addition reaches it unexpectedly.
func (r *machineEnrollmentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan machineEnrollmentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	keyID, err := r.client.ResolveMachineKey(plan.Name.ValueString())
	if err != nil {
		if kvaultclient.IsNotFound(err) {
			plan.KeyID = types.StringValue("")
			resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
			return
		}
		resp.Diagnostics.AddError("Error resolving machine enrollment", err.Error())
		return
	}

	plan.KeyID = types.StringValue(keyID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *machineEnrollmentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var model machineEnrollmentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if model.KeyID.ValueString() == "" {
		// Never actually enrolled (no code was ever redeemed for this
		// name), or already revoked — nothing exists server-side to
		// revoke.
		return
	}

	if err := r.client.RevokeMachineKey(model.KeyID.ValueString()); err != nil {
		resp.Diagnostics.AddError("Error revoking machine enrollment", err.Error())
	}
}

func (r *machineEnrollmentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Import by name — key_id is then resolved on the next Read/plan.
	resource.ImportStatePassthroughID(ctx, path.Root("name"), req, resp)
}
