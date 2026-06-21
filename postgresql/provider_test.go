package postgresql

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/provider"
)

// testAccProviderShim is a minimal stand-in used by acceptance-test check
// functions to reach the database: testAccProvider.Meta().(*Client) returns a
// *Client configured from the standard PG* environment variables, the same way
// Terraform configures the provider during the test run.
type testAccProviderShim struct{ meta any }

func (p *testAccProviderShim) Meta() any { return p.meta }

var testAccProvider = &testAccProviderShim{}

// TestProviderSchema validates the provider configuration schema without needing
// Terraform or a live database.
func TestProviderSchema(t *testing.T) {
	var resp provider.SchemaResponse
	NewFrameworkProvider("test")().Schema(context.Background(), provider.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("provider schema has errors: %+v", resp.Diagnostics)
	}
}

func testAccPreCheck(t *testing.T) {
	if os.Getenv("PGHOST") == "" {
		t.Fatal("PGHOST must be set for acceptance tests")
	}
	if os.Getenv("PGUSER") == "" {
		t.Fatal("PGUSER must be set for acceptance tests")
	}

	if testAccProvider.meta == nil {
		client, err := NewClientFromParams(testAccProviderParamsFromEnv())
		if err != nil {
			t.Fatal(err)
		}
		testAccProvider.meta = client
	}
}

// testAccProviderParamsFromEnv mirrors the provider's environment-variable
// fallbacks so check functions connect the same way the configured provider does.
func testAccProviderParamsFromEnv() ProviderConfigParams {
	envInt := func(key string, def int) int {
		if v := os.Getenv(key); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
		return def
	}
	envStr := func(key, def string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return def
	}
	superuser := true
	if v := os.Getenv("PGSUPERUSER"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			superuser = b
		}
	}
	return ProviderConfigParams{
		Scheme:          "postgres",
		Host:            os.Getenv("PGHOST"),
		Port:            envInt("PGPORT", 5432),
		Database:        envStr("PGDATABASE", "postgres"),
		Username:        envStr("PGUSER", "postgres"),
		Password:        os.Getenv("PGPASSWORD"),
		Superuser:       superuser,
		SSLMode:         os.Getenv("PGSSLMODE"),
		ConnectTimeout:  180,
		MaxConnections:  20,
		ExpectedVersion: defaultExpectedPostgreSQLVersion,
	}
}
