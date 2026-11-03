// terraform-provider-hubdeck is a Terraform/OpenTofu provider for Hubdeck
// apps. Today it only implements KVault's (https://github.com/hsuk-io/kvault)
// resources, scoped deliberately narrow: it only ever authenticates as a
// canMintEnrollments Service Token, and so can only mint/revoke machine
// enrollment codes and read secrets — never manage projects, Service
// Tokens, or IP restrictions, all of which stay behind KVault's
// human-SSO-only /admin/* surface. See internal/provider/provider.go and
// KVault's own CLAUDE.md for the full design.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/hubdeck/terraform-provider-hubdeck/internal/provider"
)

// version and commit are overridden at build time via -ldflags, same
// convention as HashiCorp's own provider scaffolding (goreleaser sets
// these from the git tag/commit — see .goreleaser.yml). Left at their
// zero values for local builds; commit isn't surfaced anywhere yet, kept
// only so goreleaser's -X main.commit=... doesn't fail to link against a
// nonexistent symbol.
var (
	version = "dev"
	commit  = "none" //nolint:unused
)

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "set to true to run the provider with support for debuggers like delve")
	flag.Parse()

	opts := providerserver.ServeOpts{
		Address: "registry.terraform.io/hubdeck/hubdeck",
		Debug:   debug,
	}

	if err := providerserver.Serve(context.Background(), provider.New(version), opts); err != nil {
		log.Fatal(err.Error())
	}
}
