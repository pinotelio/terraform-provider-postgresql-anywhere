package postgresql

import (
	"context"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestSSHAuthMethodsRequiresCredential(t *testing.T) {
	if _, err := (SSHTunnelConfig{}).authMethods(); err == nil {
		t.Fatal("expected an error when neither password nor private_key is set")
	}
	if _, err := (SSHTunnelConfig{Password: "x"}).authMethods(); err != nil {
		t.Fatalf("password auth should be accepted: %v", err)
	}
}

func TestSSHHostKeyVerification(t *testing.T) {
	if _, err := (SSHTunnelConfig{}).hostKeyCallback(); err == nil {
		t.Fatal("expected an error when no host-key verification is configured")
	}
	if _, err := (SSHTunnelConfig{InsecureIgnoreHostKey: true}).hostKeyCallback(); err != nil {
		t.Fatalf("insecure_ignore_host_key should be accepted: %v", err)
	}
	if _, err := (SSHTunnelConfig{HostKey: "not-a-valid-key"}).hostKeyCallback(); err == nil {
		t.Fatal("expected an error for an invalid host_key")
	}
}

func TestCloudTunnelsRequireFields(t *testing.T) {
	if _, _, err := (&AzureBastionTunnelConfig{}).EnsureUp(); err == nil {
		t.Fatal("expected an error for missing azure_bastion fields")
	}
	if _, _, err := (&GCPIAPTunnelConfig{}).EnsureUp(); err == nil {
		t.Fatal("expected an error for missing gcp_iap fields")
	}
}

func TestBuildTunnelSelectsTransport(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		model providerModel
		want  string
	}{
		{"direct", providerModel{}, "<nil>"},
		{"aws_ssm", providerModel{AWSSSM: &awsSSMBlockModel{Region: types.StringValue("eu-west-1")}}, "*postgresql.SSMTunnelConfig"},
		{"ssh_bastion", providerModel{SSHBastion: &sshBastionBlockModel{Host: types.StringValue("h")}}, "*postgresql.SSHTunnelConfig"},
		{"azure_bastion", providerModel{AzureBastion: &azureBastionBlockModel{BastionName: types.StringValue("b")}}, "*postgresql.AzureBastionTunnelConfig"},
		{"gcp_iap", providerModel{GCPIAP: &gcpIAPBlockModel{Instance: types.StringValue("i")}}, "*postgresql.GCPIAPTunnelConfig"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var diags diag.Diagnostics
			got := fmt.Sprintf("%T", c.model.buildTunnel(ctx, "db", 5432, &diags))
			if got != c.want {
				t.Fatalf("buildTunnel = %s, want %s", got, c.want)
			}
		})
	}
}
