# terraform-provider-kvault

A Terraform/OpenTofu provider for [KVault](https://github.com/hsuk-io/kvault),
scoped deliberately narrow: it authenticates as a single `canMintEnrollments`
Service Token and can only mint/revoke machine enrollment codes and read
secrets. It has no concept of an "admin" credential — KVault's `/admin/*`
surface (projects, Service Tokens, IP restrictions) is entirely gated by
Cloudflare Access human SSO and stays that way; this provider is not, and
will never be, a way around that.

Requires **Terraform 1.10+ or OpenTofu 1.10+** (ephemeral resource
support).

## Provider configuration

```hcl
terraform {
  required_providers {
    kvault = {
      source = "hubdeck/kvault"
    }
  }
}

provider "kvault" {
  endpoint         = "https://vault.example.com"
  ci_client_id     = var.kvault_ci_client_id     # or set KVAULT_CI_CLIENT_ID
  ci_client_secret = var.kvault_ci_client_secret # or set KVAULT_CI_CLIENT_SECRET
}
```

`ci_client_id`/`ci_client_secret` are a Cloudflare Access Service Token's
credentials. An admin must create the token in KVault and set
`canMintEnrollments` on it first — see KVault's `CLAUDE.md`, "CI
self-enrollment" / "Fleet rebuilds (rekey)" sections.

## Resource: `kvault_machine_enrollment`

Tracks a KVault machine identity's lifecycle: resolves its `key_id` once
a host actually enrolls under this name, and revokes that `key_id` on
destroy. **Mints no codes itself** — see the ephemeral resource below
for that. (An earlier version of this provider tried to mint a code
here via a write-only attribute; that's structurally impossible —
`terraform-plugin-framework` nulls every write-only attribute's value
right after `Create`/`Update` return, before the response even reaches
Terraform Core, specifically so a provider can never use one as an
output channel. Write-only attributes are for values flowing *into* a
resource from the practitioner's own config, not values a provider
generates and hands back.)

```hcl
resource "kvault_machine_enrollment" "host" {
  name = "prod-worker-03"
}
```

- **`name`** (required) — the machine identity to track. Changing it is
  NOT a rename: it forces a destroy (revoking the old identity) and
  create (starts tracking a fresh one under the new name).
- **`key_id`** (computed) — the KVault machine key this identity resolved
  to, once a host has actually redeemed a code minted for this name.
  Empty until then — that's expected, not an error. Also reverts to
  empty (rather than staying stale) if the key is revoked out of band,
  e.g. by an admin via the KVault GUI.

Destroying this resource revokes `key_id` via
`DELETE /api/machine-keys/:id` — if no host ever actually enrolled under
this name, there's nothing to revoke and this is a no-op.

## Ephemeral resource: `kvault_machine_enrollment_code`

Mints a single-use KVault machine enrollment code — the Terraform-native
replacement for baking a `canMintEnrollments` Service Token directly
into a host. **Every `Open` mints a fresh code** — unlike the resource
above, an ephemeral resource has no state to diff against, so there is
no "generation" counter to bump: referencing it at all means "mint one
right now." Pair it with `kvault_machine_enrollment.<name>.key_id` to
mint a **rekey**-mode code against that exact, already-tracked identity
(e.g. after rebuilding a host); leave `key_id` unset for a fresh
enrollment.

```hcl
resource "kvault_machine_enrollment" "host" {
  name = "prod-worker-03"
}

ephemeral "kvault_machine_enrollment_code" "host" {
  name   = kvault_machine_enrollment.host.name
  key_id = kvault_machine_enrollment.host.key_id # omit/empty for a fresh enrollment
}

resource "some_compute_instance" "host" {
  # ...
  provisioner "remote-exec" {
    inline = [
      "kvault-cli enroll --url https://vault.example.com --code ${ephemeral.kvault_machine_enrollment_code.host.code} --name prod-worker-03",
    ]
  }
}
```

An ephemeral value like `code` above can ONLY flow into another
ephemeral resource's config, a provider configuration block, a
write-only managed-resource attribute, or (as here) a provisioner block
— never into a plain resource attribute or an output; Terraform's own
consistency checks reject that. A `provisioner` is the right fit for
handing a one-time enrollment code to a freshly-created host.

- **`name`** (required) — the machine identity this code is bound to.
- **`key_id`** (optional) — mint a rekey-mode code against this exact
  identity. Leave unset for a fresh enrollment, or to let the server
  resolve by name itself (same 0/1/ambiguous logic as the underlying
  `enroll-code` API).
- **`ttl`** (optional) — a Go duration string (e.g. `"10m"`, `"1h"`) for
  how long the code stays redeemable. Server-enforced bounds apply and
  differ for a fresh enrollment vs. a rekey; omit to use the server's own
  default for whichever mode applies.
- **`bind_source_ip`** (optional) — restrict redemption to one known IP.
- **`code`** (computed, sensitive) — the freshly minted one-time code.
  Ephemeral by definition — never persisted anywhere by Terraform.
- **`mode`** (computed) — `"create"` or `"rekey"`, for logging/debugging.
- **`expires_in_seconds`** (computed) — how long the code is actually
  valid for, as resolved by the server.

## Ephemeral resource: `kvault_secret`

Reads a secret via KVault's ordinary Service-Token-authenticated
`GET /api/secret/:key` route — the value exists only for the duration of
the apply that opens it and is never written to plan or state.

```hcl
ephemeral "kvault_secret" "docker_hub" {
  name = "docker-hub-token"
}

# Feed it to a provisioner, a write-only argument elsewhere, or
# an external program via `terraform_data`'s ephemeral-aware equivalent —
# never to a plain resource attribute or an output.
```

- **`name`** (required) — the secret's key.
- **`namespace`** (optional) — if the secret's ACL requires one.
- **`value`** (computed, sensitive) — the decrypted value.

## What this provider deliberately does not do

- Manage KVault projects, Service Tokens, or IP restrictions — those
  require an admin (human SSO) credential this provider never holds.
- Write secrets — only the ephemeral *read* path exists.
- Generate or hold a host's own Ed25519 keypair — that still happens on
  the host itself (`kvault-cli enroll`), using the `code` this provider
  mints as the one-time credential.

## Development

```sh
go build ./...
go vet ./...
go test ./...
```

Acceptance tests (`internal/provider/machine_enrollment_test.go`) run
real plan/apply/destroy cycles through a Terraform or OpenTofu binary
against an in-memory stub server — no live KVault instance or network
access needed. They run unconditionally (`IsUnitTest: true`, no `TF_ACC`
required), but do need two environment variables when the only binary
available is `tofu`, not `terraform`, since OpenTofu enforces a stricter
registry-host check for the test harness's locally-reattached provider
than Terraform does:

```sh
TF_ACC_TERRAFORM_PATH=$(which tofu) TF_ACC_PROVIDER_HOST=registry.opentofu.org go test ./...
```

With a real `terraform` binary on `PATH`, plain `go test ./...` is
enough — neither variable is needed.

See KVault's own `CLAUDE.md` (in the `kvault` repo) for the server-side
design this provider's `kvault_machine_enrollment` resource and
`GET /api/machine-keys/resolve` endpoint implement.