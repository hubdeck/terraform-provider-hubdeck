# terraform-provider-kvault

A Terraform/OpenTofu provider for [KVault](https://github.com/hsuk-io/kvault),
scoped deliberately narrow: it authenticates as a single `canMintEnrollments`
Service Token and can only mint/revoke machine enrollment codes and read
secrets. It has no concept of an "admin" credential — KVault's `/admin/*`
surface (projects, Service Tokens, IP restrictions) is entirely gated by
Cloudflare Access human SSO and stays that way; this provider is not, and
will never be, a way around that.

Requires **Terraform 1.11+ or OpenTofu 1.11+** (write-only resource
attribute support).

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

Mints a single-use, name-bound KVault machine enrollment code — the
Terraform-native replacement for baking a `canMintEnrollments` Service
Token directly into a host's `user_data`. Pairs a persistent identity
(`name`, resolved `key_id`) with a one-time `code` that only exists for
the duration of one apply.

```hcl
resource "kvault_machine_enrollment" "host" {
  name       = "prod-worker-03"
  generation = 1 # bump to rekey the SAME identity, e.g. after a rebuild
}

resource "some_compute_instance" "host" {
  # ...
  user_data = templatefile("cloud-init.tpl.yaml", {
    kvault_code = kvault_machine_enrollment.host.code
  })
}
```

- **`name`** (required) — the machine identity this code is bound to.
  Changing it is NOT a rename: it forces a destroy (revoking the old
  identity) and create (minting a fresh one).
- **`generation`** (required) — bump this to mint a fresh **rekey**-mode
  code for the *same* identity (`key_id` is preserved). This is always an
  in-place update, never a destroy/recreate — that distinction is the
  entire point: replacing would revoke-then-recreate, destroying the
  continuity rekey-mode exists to preserve.
- **`ttl`** (optional) — a Go duration string (e.g. `"10m"`, `"1h"`) for
  how long the code stays redeemable. Server-enforced bounds apply and
  differ for a fresh enrollment vs. a rekey.
- **`bind_source_ip`** (optional) — restrict redemption to one known IP.
- **`code`** (write-only) — the freshly minted one-time code. **Never
  stored in state.** Reference it only from another write-only/ephemeral
  context within the same apply (as in the example above, or a
  provisioner) — not from an output, and not from a resource attribute
  that isn't itself write-only.
- **`key_id`** (computed) — the KVault machine key this identity resolved
  to, once the host has actually redeemed its code. Empty until then —
  that's expected, not an error; it fills in on a later refresh.

Destroying this resource revokes `key_id` via
`DELETE /api/machine-keys/:id` — if the host never actually enrolled
(code expired unused), there's nothing to revoke and this is a no-op.

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
```

See KVault's own `CLAUDE.md` (in the `kvault` repo) for the server-side
design this provider's `kvault_machine_enrollment` resource and
`GET /api/machine-keys/resolve` endpoint implement.