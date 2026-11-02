// terraform-provider-kvault is a Terraform/OpenTofu provider for KVault
// (https://github.com/hsuk-io/kvault), scoped deliberately narrow: it
// only ever authenticates as a canMintEnrollments Service Token, and so
// can only mint/revoke machine enrollment codes and read secrets — never
// manage projects, Service Tokens, or IP restrictions, all of which stay
// behind KVault's human-SSO-only /admin/* surface. See
// internal/provider/provider.go and KVault's own CLAUDE.md for the full
// design.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/hubdeck/terraform-provider-kvault/internal/provider"
)

// version is overridden at build time via -ldflags, same convention as
// HashiCorp's own provider scaffolding (goreleaser sets this from the git
// tag). Left as "dev" for local builds.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "set to true to run the provider with support for debuggers like delve")
	flag.Parse()

	opts := providerserver.ServeOpts{
		Address: "registry.terraform.io/hubdeck/kvault",
		Debug:   debug,
	}

	if err := providerserver.Serve(context.Background(), provider.New(version), opts); err != nil {
		log.Fatal(err.Error())
	}
}
