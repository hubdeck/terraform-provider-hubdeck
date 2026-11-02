package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/hubdeck/terraform-provider-kvault/internal/kvaultclient"
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

// machineEnrollmentModel mirrors KVault's own enroll-code design (see
// CLAUDE.md's "Fleet rebuilds (rekey)" section) one-to-one:
//   - Name is the bound machine identity — changing it means a genuinely
//     different machine, so it forces Replace (which calls Delete, i.e.
//     revokes the OLD identity, then Create, which mints a fresh
//     create-mode code for the new name).
//   - Generation is the trigger attribute: bumping it alone must cause an
//     in-place Update (remint a REKEY-mode code against the SAME key_id),
//     never a Replace — Replace would Delete-then-Create, destroying the
//     exact continuity rekey-mode exists to preserve. No RequiresReplace
//     plan modifier is attached to it for exactly that reason.
//   - Code is the one-time code itself: WriteOnly (Terraform 1.11+/
//     OpenTofu 1.11+ required — practitioners on older versions get a
//     clear error from Terraform core itself, not from this provider),
//     paired with Generation as its "trigger" attribute, following
//     HashiCorp's own documented pattern for a provider-generated
//     write-only value. Never persisted to state; flows only to other
//     ephemeral/write-only sinks or a provisioner within the same apply —
//     e.g. into a cloud-init/user_data template passed to the compute
//     resource that boots this host.
//   - KeyID is resolved asynchronously, via Read/Refresh calling
//     GET /api/machine-keys/resolve — it does not exist yet immediately
//     after Create (the host hasn't booted and redeemed the code yet),
//     and that is not an error; it simply stays empty until a later
//     refresh after the host has enrolled.
type machineEnrollmentModel struct {
	Name         types.String `tfsdk:"name"`
	Generation   types.Int64  `tfsdk:"generation"`
	TTL          types.String `tfsdk:"ttl"`
	BindSourceIP types.String `tfsdk:"bind_source_ip"`
	Code         types.String `tfsdk:"code"`
	KeyID        types.String `tfsdk:"key_id"`
}

func (r *machineEnrollmentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_machine_enrollment"
}

func (r *machineEnrollmentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Mints a single-use, name-bound KVault machine enrollment code — the Terraform-native replacement for baking a canMintEnrollments Service Token directly into a host's user_data. See KVault's CLAUDE.md, \"Fleet rebuilds (rekey)\" section, for the full design this resource implements.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required:    true,
				Description: "The machine identity this code is bound to (matches [a-zA-Z0-9_.-]+). Changing this destroys the old identity (revoking its key) and mints a fresh one — it is NOT a rename.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"generation": schema.Int64Attribute{
				Required:    true,
				Description: "Bump this to mint a fresh rekey-mode code for the SAME machine identity (key_id is preserved) — e.g. after rebuilding the host from its base image. This is an in-place update, never a destroy/recreate.",
			},
			"ttl": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "How long the minted code remains redeemable, as a Go duration string (e.g. \"10m\", \"1h\"). Server-enforced bounds apply and differ for a fresh enrollment vs. a rekey — see KVault's CLAUDE.md. Defaults to the server's own default for whichever mode applies.",
			},
			"bind_source_ip": schema.StringAttribute{
				Optional:    true,
				Description: "Optional bare IP this code's redemption is restricted to, if you already know the host's address ahead of time.",
			},
			"code": schema.StringAttribute{
				Optional:    true,
				WriteOnly:   true,
				Sensitive:   true,
				Description: "The freshly minted one-time enrollment code, recomputed whenever \"generation\" changes. Never stored in state — reference it only from an ephemeral/write-only context within the same apply, e.g. a provisioner or a cloud-init template passed to the resource that boots this host. Requires Terraform 1.11+ or OpenTofu 1.11+.",
			},
			"key_id": schema.StringAttribute{
				Computed:    true,
				Description: "The KVault machine key ID this identity resolved to, once the host has actually redeemed its code. Empty until then — not an error, just not-yet-enrolled.",
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

func (r *machineEnrollmentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var model machineEnrollmentModel
	// Config (not just plan/state) is required here: Code is write-only,
	// so its value is never present in req.Plan — only in req.Config, and
	// even there the practitioner normally leaves it null since this
	// provider computes it. We don't actually read Config.Code (nothing
	// to read), but every other field comes from Plan as usual.
	resp.Diagnostics.Append(req.Plan.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}

	seconds, err := ttlSeconds(model.TTL)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("ttl"), "Invalid ttl", err.Error())
		return
	}

	result, err := r.client.EnrollCode(model.Name.ValueString(), "", seconds, model.BindSourceIP.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error minting enrollment code", err.Error())
		return
	}

	model.Code = types.StringValue(result.Code)
	model.KeyID = types.StringValue(result.KeyID) // empty for a fresh ("create" mode) enrollment — set once the host enrolls, via Read
	model.TTL = types.StringValue(ttlString(seconds, result.ExpiresInSeconds, model.TTL))

	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

// ttlString preserves whatever the practitioner configured (so plans stay
// stable across refreshes); only fills in a concrete value from the
// server's actual ExpiresInSeconds when the practitioner left ttl unset,
// so Computed has something deterministic to settle on.
func ttlString(requestedSeconds, actualSeconds int, configured types.String) string {
	if requestedSeconds > 0 && !configured.IsNull() && !configured.IsUnknown() {
		return configured.ValueString()
	}
	return (time.Duration(actualSeconds) * time.Second).String()
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
			// Not yet enrolled by the host — not an error, just not
			// resolved yet. Leave key_id as whatever it already was
			// (almost always still empty).
			resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
			return
		}
		resp.Diagnostics.AddError("Error resolving machine enrollment", err.Error())
		return
	}

	model.KeyID = types.StringValue(keyID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

func (r *machineEnrollmentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state machineEnrollmentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	seconds, err := ttlSeconds(plan.TTL)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("ttl"), "Invalid ttl", err.Error())
		return
	}

	// Name can't have changed here — it forces Replace, so Update is only
	// ever reached for the same identity. Pass the known key_id (if any)
	// so the server mints a REKEY-mode code against that exact identity
	// rather than re-resolving by name (name-resolution is still a safe
	// fallback if key_id isn't known yet, e.g. the host never enrolled
	// after the original Create — then this is really still a fresh
	// enrollment under the hood).
	result, err := r.client.EnrollCode(plan.Name.ValueString(), state.KeyID.ValueString(), seconds, plan.BindSourceIP.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error minting enrollment code", err.Error())
		return
	}

	plan.Code = types.StringValue(result.Code)
	if result.KeyID != "" {
		plan.KeyID = types.StringValue(result.KeyID)
	} else {
		plan.KeyID = state.KeyID // preserve what Read last resolved, if the mint response didn't carry it (create-mode path)
	}
	plan.TTL = types.StringValue(ttlString(seconds, result.ExpiresInSeconds, plan.TTL))

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *machineEnrollmentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var model machineEnrollmentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if model.KeyID.ValueString() == "" {
		// The host never actually enrolled (code expired unused, or the
		// host was never booted) — nothing exists server-side to revoke.
		return
	}

	if err := r.client.RevokeMachineKey(model.KeyID.ValueString()); err != nil {
		resp.Diagnostics.AddError("Error revoking machine enrollment", err.Error())
	}
}

func (r *machineEnrollmentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Import by name — key_id and generation are then resolved/left at
	// their zero values on the next Read/plan; the practitioner's config
	// for generation must still be reconciled by hand (there is no
	// server-side "current generation" to import, since KVault itself
	// has no such concept — generation is a purely Terraform-side trigger).
	resource.ImportStatePassthroughID(ctx, path.Root("name"), req, resp)
}
