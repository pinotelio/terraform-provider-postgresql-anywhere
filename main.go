package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/pinotelio/terraform-provider-postgresql-anywhere/postgresql"
)

// version is set by the release build (goreleaser ldflags).
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "set to true to run the provider with support for debuggers like delve")
	flag.Parse()

	opts := providerserver.ServeOpts{
		Address: "registry.terraform.io/pinotelio/postgresql-anywhere",
		Debug:   debug,
	}

	if err := providerserver.Serve(context.Background(), postgresql.NewFrameworkProvider(version), opts); err != nil {
		log.Fatal(err)
	}
}
