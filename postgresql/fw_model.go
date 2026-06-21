package postgresql

import (
	"context"
	"os"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// providerModel maps the provider config block to Go values.
type providerModel struct {
	Scheme                          types.String      `tfsdk:"scheme"`
	Host                            types.String      `tfsdk:"host"`
	Port                            types.Int64       `tfsdk:"port"`
	Database                        types.String      `tfsdk:"database"`
	Username                        types.String      `tfsdk:"username"`
	Password                        types.String      `tfsdk:"password"`
	AWSRDSIAMAuth                   types.Bool        `tfsdk:"aws_rds_iam_auth"`
	AWSRDSIAMProfile                types.String      `tfsdk:"aws_rds_iam_profile"`
	AWSRDSIAMRegion                 types.String      `tfsdk:"aws_rds_iam_region"`
	AWSRDSIAMProviderRoleARN        types.String      `tfsdk:"aws_rds_iam_provider_role_arn"`
	AzureIdentityAuth               types.Bool        `tfsdk:"azure_identity_auth"`
	AzureTenantID                   types.String      `tfsdk:"azure_tenant_id"`
	GCPIAMImpersonateServiceAccount types.String      `tfsdk:"gcp_iam_impersonate_service_account"`
	DatabaseUsername                types.String      `tfsdk:"database_username"`
	Superuser                       types.Bool        `tfsdk:"superuser"`
	SSLMode                         types.String      `tfsdk:"sslmode"`
	SSLModeDeprecated               types.String      `tfsdk:"ssl_mode"`
	SSLRootCert                     types.String      `tfsdk:"sslrootcert"`
	ConnectTimeout                  types.Int64       `tfsdk:"connect_timeout"`
	MaxConnections                  types.Int64       `tfsdk:"max_connections"`
	ExpectedVersion                 types.String      `tfsdk:"expected_version"`
	Clientcert                      []clientCertModel `tfsdk:"clientcert"`

	// Network transports. At most one is set (enforced by ConfigValidators).
	AWSSSM       *awsSSMBlockModel       `tfsdk:"aws_ssm"`
	SSHBastion   *sshBastionBlockModel   `tfsdk:"ssh_bastion"`
	AzureBastion *azureBastionBlockModel `tfsdk:"azure_bastion"`
	GCPIAP       *gcpIAPBlockModel       `tfsdk:"gcp_iap"`
}

type clientCertModel struct {
	Cert      types.String `tfsdk:"cert"`
	Key       types.String `tfsdk:"key"`
	SSLInline types.Bool   `tfsdk:"sslinline"`
}

type awsSSMBlockModel struct {
	Region       types.String `tfsdk:"region"`
	Profile      types.String `tfsdk:"profile"`
	AccessKey    types.String `tfsdk:"access_key"`
	SecretKey    types.String `tfsdk:"secret_key"`
	SessionToken types.String `tfsdk:"session_token"`
	RoleARN      types.String `tfsdk:"role_arn"`
	InstanceID   types.String `tfsdk:"instance_id"`
	InstanceName types.String `tfsdk:"instance_name"`
	InstanceTags types.Map    `tfsdk:"instance_tags"`
	LocalPort    types.Int64  `tfsdk:"local_port"`
}

type sshBastionBlockModel struct {
	Host                  types.String `tfsdk:"host"`
	Port                  types.Int64  `tfsdk:"port"`
	User                  types.String `tfsdk:"user"`
	Password              types.String `tfsdk:"password"`
	PrivateKey            types.String `tfsdk:"private_key"`
	PrivateKeyPassphrase  types.String `tfsdk:"private_key_passphrase"`
	HostKey               types.String `tfsdk:"host_key"`
	KnownHostsFile        types.String `tfsdk:"known_hosts_file"`
	InsecureIgnoreHostKey types.Bool   `tfsdk:"insecure_ignore_host_key"`
	LocalPort             types.Int64  `tfsdk:"local_port"`
}

type azureBastionBlockModel struct {
	BastionName      types.String `tfsdk:"bastion_name"`
	ResourceGroup    types.String `tfsdk:"resource_group"`
	TargetResourceID types.String `tfsdk:"target_resource_id"`
	Subscription     types.String `tfsdk:"subscription"`
	ResourcePort     types.Int64  `tfsdk:"resource_port"`
	LocalPort        types.Int64  `tfsdk:"local_port"`
}

type gcpIAPBlockModel struct {
	Instance     types.String `tfsdk:"instance"`
	Zone         types.String `tfsdk:"zone"`
	Project      types.String `tfsdk:"project"`
	InstancePort types.Int64  `tfsdk:"instance_port"`
	LocalPort    types.Int64  `tfsdk:"local_port"`
}

// toParams converts the model into ProviderConfigParams, applying default and
// environment-variable fallbacks (the framework does not apply schema-level
// defaults).
func (m providerModel) toParams(ctx context.Context, diags *diag.Diagnostics) ProviderConfigParams {
	p := ProviderConfigParams{
		Scheme:                          strDefault(m.Scheme, "postgres"),
		Host:                            strEnv(m.Host, "PGHOST", ""),
		Port:                            int(intEnv(m.Port, "PGPORT", 5432)),
		Database:                        strEnv(m.Database, "PGDATABASE", "postgres"),
		Username:                        strEnv(m.Username, "PGUSER", "postgres"),
		DatabaseUsername:                m.DatabaseUsername.ValueString(),
		Password:                        strEnv(m.Password, "PGPASSWORD", ""),
		Superuser:                       boolEnv(m.Superuser, "PGSUPERUSER", true),
		SSLRootCertPath:                 m.SSLRootCert.ValueString(),
		ConnectTimeout:                  int(intEnv(m.ConnectTimeout, "PGCONNECT_TIMEOUT", 180)),
		MaxConnections:                  int(intDefault(m.MaxConnections, 20)),
		ExpectedVersion:                 strDefault(m.ExpectedVersion, "9.0.0"),
		AWSRDSIAMAuth:                   m.AWSRDSIAMAuth.ValueBool(),
		AWSRDSIAMProfile:                m.AWSRDSIAMProfile.ValueString(),
		AWSRDSIAMRegion:                 m.AWSRDSIAMRegion.ValueString(),
		AWSRDSIAMProviderRoleARN:        m.AWSRDSIAMProviderRoleARN.ValueString(),
		AzureIdentityAuth:               m.AzureIdentityAuth.ValueBool(),
		AzureTenantID:                   m.AzureTenantID.ValueString(),
		GCPIAMImpersonateServiceAccount: m.GCPIAMImpersonateServiceAccount.ValueString(),
	}

	switch {
	case !m.SSLMode.IsNull() && !m.SSLMode.IsUnknown():
		p.SSLMode = m.SSLMode.ValueString()
	case !m.SSLModeDeprecated.IsNull() && !m.SSLModeDeprecated.IsUnknown():
		p.SSLModeDeprecated = m.SSLModeDeprecated.ValueString()
	default:
		p.SSLMode = os.Getenv("PGSSLMODE")
	}

	if len(m.Clientcert) > 0 {
		cc := m.Clientcert[0]
		p.ClientCert = &ClientCertificateConfig{
			CertificatePath: cc.Cert.ValueString(),
			KeyPath:         cc.Key.ValueString(),
			SSLInline:       cc.SSLInline.ValueBool(),
		}
	}

	p.Tunnel = m.buildTunnel(ctx, p.Host, p.Port, diags)

	return p
}

// buildTunnel constructs the configured network transport (or nil for a direct
// connection). dbHost/dbPort are the private database endpoint reached through
// the bastion (used by the AWS SSM and SSH transports).
func (m providerModel) buildTunnel(ctx context.Context, dbHost string, dbPort int, diags *diag.Diagnostics) Tunnel {
	switch {
	case m.AWSSSM != nil:
		b := m.AWSSSM
		tags := map[string]string{}
		if !b.InstanceTags.IsNull() && !b.InstanceTags.IsUnknown() {
			diags.Append(b.InstanceTags.ElementsAs(ctx, &tags, false)...)
		}
		return &SSMTunnelConfig{
			Region:       b.Region.ValueString(),
			Profile:      b.Profile.ValueString(),
			AccessKey:    b.AccessKey.ValueString(),
			SecretKey:    b.SecretKey.ValueString(),
			SessionToken: b.SessionToken.ValueString(),
			RoleARN:      b.RoleARN.ValueString(),
			InstanceID:   b.InstanceID.ValueString(),
			InstanceName: b.InstanceName.ValueString(),
			InstanceTags: tags,
			RemoteHost:   dbHost,
			RemotePort:   dbPort,
			LocalPort:    int(b.LocalPort.ValueInt64()),
		}
	case m.SSHBastion != nil:
		b := m.SSHBastion
		return &SSHTunnelConfig{
			Host:                  b.Host.ValueString(),
			Port:                  int(b.Port.ValueInt64()),
			User:                  b.User.ValueString(),
			Password:              b.Password.ValueString(),
			PrivateKey:            b.PrivateKey.ValueString(),
			PrivateKeyPassphrase:  b.PrivateKeyPassphrase.ValueString(),
			HostKey:               b.HostKey.ValueString(),
			KnownHostsFile:        b.KnownHostsFile.ValueString(),
			InsecureIgnoreHostKey: b.InsecureIgnoreHostKey.ValueBool(),
			RemoteHost:            dbHost,
			RemotePort:            dbPort,
			LocalPort:             int(b.LocalPort.ValueInt64()),
		}
	case m.AzureBastion != nil:
		b := m.AzureBastion
		return &AzureBastionTunnelConfig{
			BastionName:      b.BastionName.ValueString(),
			ResourceGroup:    b.ResourceGroup.ValueString(),
			TargetResourceID: b.TargetResourceID.ValueString(),
			Subscription:     b.Subscription.ValueString(),
			ResourcePort:     int(b.ResourcePort.ValueInt64()),
			LocalPort:        int(b.LocalPort.ValueInt64()),
		}
	case m.GCPIAP != nil:
		b := m.GCPIAP
		return &GCPIAPTunnelConfig{
			Instance:     b.Instance.ValueString(),
			Zone:         b.Zone.ValueString(),
			Project:      b.Project.ValueString(),
			InstancePort: int(b.InstancePort.ValueInt64()),
			LocalPort:    int(b.LocalPort.ValueInt64()),
		}
	}
	return nil
}

func strDefault(v types.String, def string) string {
	if v.IsNull() || v.IsUnknown() {
		return def
	}
	return v.ValueString()
}

func strEnv(v types.String, env, def string) string {
	if !v.IsNull() && !v.IsUnknown() {
		return v.ValueString()
	}
	if e := os.Getenv(env); e != "" {
		return e
	}
	return def
}

func intDefault(v types.Int64, def int64) int64 {
	if v.IsNull() || v.IsUnknown() {
		return def
	}
	return v.ValueInt64()
}

func intEnv(v types.Int64, env string, def int64) int64 {
	if !v.IsNull() && !v.IsUnknown() {
		return v.ValueInt64()
	}
	if e := os.Getenv(env); e != "" {
		if n, err := strconv.Atoi(e); err == nil {
			return int64(n)
		}
	}
	return def
}

func boolEnv(v types.Bool, env string, def bool) bool {
	if !v.IsNull() && !v.IsUnknown() {
		return v.ValueBool()
	}
	if e := os.Getenv(env); e != "" {
		if b, err := strconv.ParseBool(e); err == nil {
			return b
		}
	}
	return def
}
