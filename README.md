# terraform-provider-hubdeck

A Terraform/OpenTofu provider for Hubdeck apps. Today it implements only
[KVault](https://github.com/hsuk-io/kvault)'s resources (the `hubdeck_kvault_*`
types below), scoped deliberately narrow: it authenticates as a single
`canMintEnrollments` Service Token and can only mint/revoke machine
enrollment codes and read secrets. It has no concept of an "admin"
credential — KVault's `/admin/*` surface (projects, Service Tokens, IP
restrictions) is entirely gated by Cloudflare Access human SSO and stays
that way; this provider is not, and will never be, a way around that.

Requires **Terraform 1.10+ or OpenTofu 1.10+** (ephemeral resource
support).

## Provider configuration

```hcl
terraform {
  required_providers {
    hubdeck = {
      source = "hubdeck/hubdeck"
    }
  }
}

provider "hubdeck" {
  endpoint         = "https://vault.example.com"
  ci_client_id     = var.kvault_ci_client_id     # or set KVAULT_CI_CLIENT_ID
  ci_client_secret = var.kvault_ci_client_secret # or set KVAULT_CI_CLIENT_SECRET
}
```

`ci_client_id`/`ci_client_secret` are a Cloudflare Access Service Token's
credentials. An admin must create the token in KVault and set
`canMintEnrollments` on it first — see KVault's `CLAUDE.md`, "CI
self-enrollment" / "Fleet rebuilds (rekey)" sections.

## Resource: `hubdeck_kvault_machine_enrollment`

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
resource "hubdeck_kvault_machine_enrollment" "host" {
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

## Ephemeral resource: `hubdeck_kvault_machine_enrollment_code`

Mints a single-use KVault machine enrollment code — the Terraform-native
replacement for baking a `canMintEnrollments` Service Token directly
into a host. **Every `Open` mints a fresh code** — unlike the resource
above, an ephemeral resource has no state to diff against, so there is
no "generation" counter to bump: referencing it at all means "mint one
right now." Pair it with `hubdeck_kvault_machine_enrollment.<name>.key_id` to
mint a **rekey**-mode code against that exact, already-tracked identity
(e.g. after rebuilding a host); leave `key_id` unset for a fresh
enrollment.

```hcl
resource "hubdeck_kvault_machine_enrollment" "host" {
  name = "prod-worker-03"
}

ephemeral "hubdeck_kvault_machine_enrollment_code" "host" {
  name   = hubdeck_kvault_machine_enrollment.host.name
  key_id = hubdeck_kvault_machine_enrollment.host.key_id # omit/empty for a fresh enrollment
}

resource "some_compute_instance" "host" {
  # ...
  provisioner "remote-exec" {
    inline = [
      "kvault-cli enroll --url https://vault.example.com --code ${ephemeral.hubdeck_kvault_machine_enrollment_code.host.code} --name prod-worker-03",
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

**"Never written to state" holds for THIS ephemeral resource's own
result — it does not hold for whatever you pass it into next.** A
`provisioner` block (as above) never touches state at all, so that path
is safe. But if a *different* provider's resource accepts `code` as an
ordinary (non-write-only) argument — e.g. a compute resource's plain
`user_data` field, as used for the Contabo instances this provider was
built against — **that provider writes it to state as plaintext**,
because an ordinary argument's value is always persisted; only
write-only arguments and ephemeral resources are exempt. In that case
the mitigation is that the code is single-use and expires within
minutes, not that it's somehow kept out of state. **Prefer a write-only
argument for this if the target provider offers one** (check its docs —
support is still uneven across providers); fall back to a provisioner
(never touches state either) before falling back to a plain `user_data`
argument.

**Every `Open` mints a fresh code — including on `terraform plan`, not
only `terraform apply`.** Ephemeral resources are opened by Terraform
Core during planning as well as during apply (confirmed empirically
while writing this provider's own acceptance tests — see
`machine_enrollment_test.go`'s stub server, which had to be written so
that minting a code does NOT register a MachineKey, matching the real
server: identity is only ever created by actual *redemption*
(`POST /enroll`), never by minting). Two real effects worth knowing
about, not bugs needing a guard:
- **A `terraform plan` that merely references this ephemeral resource
  mints a real code and writes a real `mint_enroll_code` audit entry on
  the KVault server — even though nothing was applied.** The code is
  simply never surfaced anywhere and expires unused. If this shows up as
  audit-log noise ("why did a mint happen with no apply?"), that's
  expected, not a sign of a problem.
- **Minting a second code does NOT invalidate an earlier one that's
  still valid and in flight** (confirmed against the server's actual
  redemption logic, `admin.ts`'s `handleEnrollRequest`): a code's
  validity is tracked only by its own `used`/expiry state, never by its
  relationship to other codes minted for the same name. So a plan-time
  mint never invalidates the apply-time code that actually gets pasted
  into a host that's still booting. The one real edge case: for a
  **fresh (create-mode) enrollment**, redemption never checks whether a
  `MachineKey` under that name already exists — it always creates a new
  one. If BOTH a plan-time and an apply-time create-mode code somehow
  got redeemed (not the normal case; the plan-time one is never exposed
  to anything), you'd end up with two separate machine identities under
  the same name, which a later rekey mint for that name would then
  correctly refuse as ambiguous (`409`) until resolved by an explicit
  `key_id`. In the ordinary case — only the code that actually reaches
  the host ever gets redeemed — this never comes up.

## Ephemeral resource: `hubdeck_kvault_secret`

Reads a secret via KVault's ordinary Service-Token-authenticated
`GET /api/secret/:key` route — the value exists only for the duration of
the apply that opens it and is never written to plan or state.

```hcl
ephemeral "hubdeck_kvault_secret" "docker_hub" {
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

CI (`.github/workflows/ci.yml`) runs `go build`/`go vet`/`gofmt` plus the
full test suite against BOTH Terraform and OpenTofu on every push/PR —
this provider is meant to work under either. Every action in both
workflows is pinned by commit SHA (with the version as a trailing
comment), not by tag — same convention as `reacks-deploy`.

## Releasing

`.goreleaser.yml` builds linux/darwin/windows/freebsd on amd64+arm64
(darwin/386 and freebsd/arm64 excluded — not realistic targets), zips
each, and produces a `_SHA256SUMS` file plus a detached GPG signature
over it, alongside `terraform-registry-manifest.json`
(`protocol_versions: ["6.0"]`) — the shape both the Terraform Registry
and the OpenTofu Registry expect. `.github/workflows/release.yml` runs
this via `goreleaser/goreleaser-action` whenever a `v*` tag is pushed.

**Dry run** (no tag, no signing key needed to try the rest of the
pipeline):

```sh
go install github.com/goreleaser/goreleaser/v2@latest
goreleaser release --snapshot --clean
```

This was run once during initial setup against a throwaway local GPG key
to confirm the full pipeline (7 platform builds, zip archives, checksum
file, valid detached signature) works end to end — it did. `dist/` is
gitignored.

**One-time setup still needed before the first real tag** (not done as
part of this change — these are account-level decisions, not something
to automate):

1. Generate a real signing key: `gpg --full-generate-key` (RSA 4096 or
   larger). This key is a long-term credential — treat it with the same
   care as KVault's own master-key escrow (CLAUDE.md's "Master key
   escrow" section): back up the private key and passphrase somewhere
   durable and offline, not only in GitHub.
2. Register the **public** key with the Terraform Registry (publishing
   settings on your registry.terraform.io account). The OpenTofu
   Registry does not require separate key registration — it picks up
   signed releases directly from this repo's GitHub releases.
3. Create a GitHub Environment named `release` on this repo (Settings ->
   Environments), add `GPG_PRIVATE_KEY` (`gpg --armor --export-secret-keys
   <key-id>`) and `PASSPHRASE` as **environment** secrets (not repository
   secrets), and set required reviewers on that environment so pushing a
   tag alone can't trigger a real publish without an explicit approval.
4. Push a `vX.Y.Z` tag. `release.yml` picks it up from there.

See KVault's own `CLAUDE.md` (in the `kvault` repo) for the server-side
design this provider's `hubdeck_kvault_machine_enrollment` resource and
`GET /api/machine-keys/resolve` endpoint implement.