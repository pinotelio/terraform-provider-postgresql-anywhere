package postgresql

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/providervalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ provider.Provider                     = (*frameworkProvider)(nil)
	_ provider.ProviderWithConfigValidators = (*frameworkProvider)(nil)
)

// ConfigValidators enforces that at most one network transport is configured.
func (p *frameworkProvider) ConfigValidators(_ context.Context) []provider.ConfigValidator {
	return []provider.ConfigValidator{
		providervalidator.Conflicting(
			path.MatchRoot("aws_ssm"),
			path.MatchRoot("ssh_bastion"),
			path.MatchRoot("azure_bastion"),
			path.MatchRoot("gcp_iap"),
		),
	}
}

type frameworkProvider struct {
	version string
}

// NewFrameworkProvider returns a framework provider factory for the given version.
func NewFrameworkProvider(version string) func() provider.Provider {
	return func() provider.Provider {
		return &frameworkProvider{version: version}
	}
}

func (p *frameworkProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "postgresql"
	resp.Version = p.version
}

// Schema defines the provider-level configuration schema.
func (p *frameworkProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Attributes: map[string]schema.Attribute{
			"scheme": schema.StringAttribute{
				Optional: true,
			},
			"host": schema.StringAttribute{
				Optional:    true,
				Description: "Name of PostgreSQL server address to connect to",
			},
			"port": schema.Int64Attribute{
				Optional:    true,
				Description: "The PostgreSQL port number to connect to at the server host, or socket file name extension for Unix-domain connections",
			},
			"database": schema.StringAttribute{
				Optional:    true,
				Description: "The name of the database to connect to (defaults to `postgres`).",
			},
			"username": schema.StringAttribute{
				Optional:    true,
				Description: "PostgreSQL user name to connect as",
			},
			"password": schema.StringAttribute{
				Optional:    true,
				Description: "Password to be used if the PostgreSQL server demands password authentication",
				Sensitive:   true,
			},
			"aws_rds_iam_auth": schema.BoolAttribute{
				Optional: true,
				Description: "Use rds_iam instead of password authentication " +
					"(see: https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/UsingWithRDS.IAMDBAuth.html)",
			},
			"aws_rds_iam_profile": schema.StringAttribute{
				Optional:    true,
				Description: "AWS profile to use for IAM auth",
			},
			"aws_rds_iam_region": schema.StringAttribute{
				Optional:    true,
				Description: "AWS region to use for IAM auth",
			},
			"aws_rds_iam_provider_role_arn": schema.StringAttribute{
				Optional:    true,
				Description: "AWS IAM role to assume for IAM auth",
			},
			"azure_identity_auth": schema.BoolAttribute{
				Optional: true,
				Description: "Use MS Azure identity OAuth token " +
					"(see: https://learn.microsoft.com/en-us/azure/postgresql/flexible-server/how-to-configure-sign-in-azure-ad-authentication)",
			},
			"azure_tenant_id": schema.StringAttribute{
				Optional:    true,
				Description: "MS Azure tenant ID (see: https://registry.terraform.io/providers/hashicorp/azurerm/latest/docs/data-sources/client_config.html)",
			},
			"gcp_iam_impersonate_service_account": schema.StringAttribute{
				Optional:    true,
				Description: "Service account to impersonate when using GCP IAM authentication.",
			},
			"database_username": schema.StringAttribute{
				Optional:    true,
				Description: "Database username associated to the connected user (for user name maps)",
			},
			"superuser": schema.BoolAttribute{
				Optional: true,
				Description: "Specify if the user to connect as is a Postgres superuser or not." +
					"If not, some feature might be disabled (e.g.: Refreshing state password from Postgres)",
			},
			"sslmode": schema.StringAttribute{
				Optional:    true,
				Description: "This option determines whether or with what priority a secure SSL TCP/IP connection will be negotiated with the PostgreSQL server",
			},
			"ssl_mode": schema.StringAttribute{
				Optional:           true,
				DeprecationMessage: "Rename PostgreSQL provider `ssl_mode` attribute to `sslmode`",
			},
			"sslrootcert": schema.StringAttribute{
				Optional:    true,
				Description: "The SSL server root certificate file path. The file must contain PEM encoded data.",
			},
			"connect_timeout": schema.Int64Attribute{
				Optional:    true,
				Description: "Maximum wait for connection, in seconds. Zero or not specified means wait indefinitely.",
			},
			"max_connections": schema.Int64Attribute{
				Optional:    true,
				Description: "Maximum number of connections to establish to the database. Zero means unlimited.",
			},
			"expected_version": schema.StringAttribute{
				Optional:    true,
				Description: "Specify the expected version of PostgreSQL.",
			},
		},
		Blocks: map[string]schema.Block{
			"clientcert": schema.ListNestedBlock{
				Description: "SSL client certificate if required by the database.",
				Validators: []validator.List{
					listvalidator.SizeAtMost(1),
				},
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"cert": schema.StringAttribute{
							Required:    true,
							Description: "The SSL client certificate file path. The file must contain PEM encoded data.",
						},
						"key": schema.StringAttribute{
							Required:    true,
							Description: "The SSL client certificate private key file path. The file must contain PEM encoded data.",
						},
						"sslinline": schema.BoolAttribute{
							Optional:    true,
							Description: "Must be set to true if you are inlining the cert/key instead of using a file path.",
						},
					},
				},
			},
			"aws_ssm": schema.SingleNestedBlock{
				Description: "Reach a private PostgreSQL endpoint by opening an AWS SSM port-forwarding session to `host`:`port` through a bastion EC2 instance. Only with scheme `postgres`.",
				Attributes: map[string]schema.Attribute{
					"region":        schema.StringAttribute{Optional: true, Description: "AWS region of the bastion and the RDS endpoint."},
					"profile":       schema.StringAttribute{Optional: true, Description: "AWS shared-config profile. Empty uses the default credential chain (incl. OIDC web identity)."},
					"access_key":    schema.StringAttribute{Optional: true, Description: "Optional static AWS access key id. Prefer the default credential chain / OIDC."},
					"secret_key":    schema.StringAttribute{Optional: true, Sensitive: true, Description: "Optional static AWS secret access key (required with access_key)."},
					"session_token": schema.StringAttribute{Optional: true, Sensitive: true, Description: "Optional static AWS session token for temporary credentials."},
					"role_arn":      schema.StringAttribute{Optional: true, Description: "Optional IAM role ARN to assume before opening the session."},
					"instance_id":   schema.StringAttribute{Optional: true, Description: "Explicit bastion EC2 instance id. Takes precedence over name/tag discovery."},
					"instance_name": schema.StringAttribute{Optional: true, Description: "Discover the bastion by its Name tag (running instances only)."},
					"instance_tags": schema.MapAttribute{Optional: true, ElementType: types.StringType, Description: "Discover the bastion by tag=value filters (running instances only)."},
					"local_port":    schema.Int64Attribute{Optional: true, Description: "Local loopback port. 0 lets the OS choose a free port."},
				},
			},
			"ssh_bastion": schema.SingleNestedBlock{
				Description: "Reach a private PostgreSQL endpoint by tunnelling `host`:`port` through an SSH bastion. Works for any cloud or on-prem. Only with scheme `postgres`.",
				Attributes: map[string]schema.Attribute{
					"host":                     schema.StringAttribute{Optional: true, Description: "Bastion hostname or IP."},
					"port":                     schema.Int64Attribute{Optional: true, Description: "Bastion SSH port (default 22)."},
					"user":                     schema.StringAttribute{Optional: true, Description: "SSH user."},
					"password":                 schema.StringAttribute{Optional: true, Sensitive: true, Description: "SSH password (use this or private_key)."},
					"private_key":              schema.StringAttribute{Optional: true, Sensitive: true, Description: "PEM-encoded SSH private key."},
					"private_key_passphrase":   schema.StringAttribute{Optional: true, Sensitive: true, Description: "Passphrase for an encrypted private_key."},
					"host_key":                 schema.StringAttribute{Optional: true, Description: "Bastion public host key in authorized_keys format, for host-key verification."},
					"known_hosts_file":         schema.StringAttribute{Optional: true, Description: "Path to a known_hosts file for host-key verification."},
					"insecure_ignore_host_key": schema.BoolAttribute{Optional: true, Description: "Disable host-key verification (not recommended)."},
					"local_port":               schema.Int64Attribute{Optional: true, Description: "Local loopback port. 0 lets the OS choose a free port."},
				},
			},
			"azure_bastion": schema.SingleNestedBlock{
				Description: "Reach a private PostgreSQL endpoint through Azure Bastion. Drives the `az` CLI (must be installed and authenticated). The tunnel terminates at the target VM's `resource_port`. Only with scheme `postgres`.",
				Attributes: map[string]schema.Attribute{
					"bastion_name":       schema.StringAttribute{Optional: true, Description: "Azure Bastion resource name."},
					"resource_group":     schema.StringAttribute{Optional: true, Description: "Resource group of the bastion."},
					"target_resource_id": schema.StringAttribute{Optional: true, Description: "Resource id of the target VM."},
					"subscription":       schema.StringAttribute{Optional: true, Description: "Optional subscription id."},
					"resource_port":      schema.Int64Attribute{Optional: true, Description: "Port on the target VM to reach (the database, or a relay to it)."},
					"local_port":         schema.Int64Attribute{Optional: true, Description: "Local loopback port. 0 lets the OS choose a free port."},
				},
			},
			"gcp_iap": schema.SingleNestedBlock{
				Description: "Reach a private PostgreSQL endpoint through GCP Identity-Aware Proxy. Drives the `gcloud` CLI (must be installed and authenticated). The tunnel terminates at the instance's `instance_port`. Only with scheme `postgres`.",
				Attributes: map[string]schema.Attribute{
					"instance":      schema.StringAttribute{Optional: true, Description: "GCE instance name."},
					"zone":          schema.StringAttribute{Optional: true, Description: "Instance zone."},
					"project":       schema.StringAttribute{Optional: true, Description: "Optional project id."},
					"instance_port": schema.Int64Attribute{Optional: true, Description: "Port on the instance to reach (the database, or a relay to it)."},
					"local_port":    schema.Int64Attribute{Optional: true, Description: "Local loopback port. 0 lets the OS choose a free port."},
				},
			},
		},
	}
}

func (p *frameworkProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var data providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	params := data.toParams(ctx, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := NewClientFromParams(params)
	if err != nil {
		resp.Diagnostics.AddError("Unable to configure PostgreSQL client", err.Error())
		return
	}

	resp.ResourceData = client
	resp.DataSourceData = client
}

// Resources returns the provider's resources.
func (p *frameworkProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewIndexResource,
		NewReplicationSlotResource,
		NewPhysicalReplicationSlotResource,
		NewSecurityLabelResource,
		NewUserMappingResource,
		NewGrantRoleResource,
		NewSubscriptionResource,
		NewExtensionResource,
		NewServerResource,
		NewDefaultPrivilegesResource,
		NewFunctionResource,
		NewPublicationResource,
		NewSchemaResource,
		NewDatabaseResource,
		NewGrantResource,
		NewRoleResource,
	}
}

func (p *frameworkProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewSchemasDataSource,
		NewSequencesDataSource,
		NewTablesDataSource,
	}
}
