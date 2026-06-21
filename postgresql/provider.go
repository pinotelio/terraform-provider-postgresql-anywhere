package postgresql

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/blang/semver"
	"golang.org/x/oauth2/google"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/rds/auth"
)

func getRDSAuthToken(region string, profile string, role string, username string, host string, port int) (string, error) {
	endpoint := fmt.Sprintf("%s:%d", host, port)

	ctx := context.Background()

	var awscfg aws.Config
	var err error

	if profile != "" {
		awscfg, err = awsConfig.LoadDefaultConfig(ctx, awsConfig.WithSharedConfigProfile(profile))
	} else if region != "" {
		awscfg, err = awsConfig.LoadDefaultConfig(ctx, awsConfig.WithRegion(region))
	} else {
		awscfg, err = awsConfig.LoadDefaultConfig(ctx)
	}
	if err != nil {
		return "", err
	}

	if role != "" {
		stsClient := sts.NewFromConfig(awscfg)
		roleInput := &sts.AssumeRoleInput{
			RoleArn:         aws.String(role),
			RoleSessionName: aws.String("TerraformPostgresqlProvider"),
		}

		roleOutput, err := stsClient.AssumeRole(ctx, roleInput)
		if err != nil {
			return "", fmt.Errorf("could not assume AWS role: %w", err)
		}

		awscfg, err = awsConfig.LoadDefaultConfig(ctx,
			awsConfig.WithCredentialsProvider(
				aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(
					*roleOutput.Credentials.AccessKeyId,
					*roleOutput.Credentials.SecretAccessKey,
					*roleOutput.Credentials.SessionToken,
				)),
			),
		)
		if err != nil {
			return "", fmt.Errorf("could not load AWS default config: %w", err)
		}
	}

	token, err := auth.BuildAuthToken(ctx, endpoint, awscfg.Region, username, awscfg.Credentials)

	return token, err
}

func createGoogleCredsFileIfNeeded() error {
	if _, err := google.FindDefaultCredentials(context.Background()); err == nil {
		return nil
	}

	rawGoogleCredentials := os.Getenv("GOOGLE_CREDENTIALS")
	if rawGoogleCredentials == "" {
		return nil
	}

	tmpFile, err := os.CreateTemp("", "")
	if err != nil {
		return fmt.Errorf("could not create temporary file: %w", err)
	}
	defer func() {
		if err := tmpFile.Close(); err != nil {
			fmt.Printf("could not close temporary file: %v", err)
		}
	}()

	_, err = tmpFile.WriteString(rawGoogleCredentials)
	if err != nil {
		return fmt.Errorf("could not write in temporary file: %w", err)
	}

	return os.Setenv("GOOGLE_APPLICATION_CREDENTIALS", tmpFile.Name())
}

func acquireAzureOauthToken(tenantId string) (string, error) {
	credential, err := azidentity.NewDefaultAzureCredential(
		&azidentity.DefaultAzureCredentialOptions{TenantID: tenantId})
	if err != nil {
		return "", err
	}
	token, err := credential.GetToken(context.Background(), policy.TokenRequestOptions{
		Scopes:   []string{"https://ossrdbms-aad.database.windows.net/.default"},
		TenantID: tenantId,
	})
	if err != nil {
		return "", err
	}
	return token.Token, nil
}

// ProviderConfigParams holds the raw provider-level configuration values.
// NewClientFromParams turns it into a *Client, centralizing the IAM / Azure /
// SSM connection logic.
type ProviderConfigParams struct {
	Scheme            string
	Host              string
	Port              int
	Database          string
	Username          string
	DatabaseUsername  string
	Password          string
	Superuser         bool
	SSLMode           string
	SSLModeDeprecated string
	SSLRootCertPath   string
	ConnectTimeout    int
	MaxConnections    int
	ExpectedVersion   string

	AWSRDSIAMAuth            bool
	AWSRDSIAMProfile         string
	AWSRDSIAMRegion          string
	AWSRDSIAMProviderRoleARN string

	AzureIdentityAuth bool
	AzureTenantID     string

	GCPIAMImpersonateServiceAccount string

	ClientCert *ClientCertificateConfig

	// Tunnel is the configured network transport (AWS SSM, SSH, Azure Bastion,
	// or GCP IAP), or nil for a direct connection.
	Tunnel Tunnel
}

// NewClientFromParams builds a configured *Client, acquiring auth tokens and
// setting up the SSM tunnel config as needed.
func NewClientFromParams(p ProviderConfigParams) (*Client, error) {
	sslMode := p.SSLMode
	if sslMode == "" && p.SSLModeDeprecated != "" {
		sslMode = p.SSLModeDeprecated
	}
	version, _ := semver.ParseTolerant(p.ExpectedVersion)

	var password string
	switch {
	case p.AWSRDSIAMAuth:
		var err error
		password, err = getRDSAuthToken(p.AWSRDSIAMRegion, p.AWSRDSIAMProfile, p.AWSRDSIAMProviderRoleARN, p.Username, p.Host, p.Port)
		if err != nil {
			return nil, err
		}
	case p.AzureIdentityAuth:
		if p.AzureTenantID == "" {
			return nil, fmt.Errorf("postgresql: azure_identity_auth is enabled, azure_tenant_id must be provided also")
		}
		var err error
		password, err = acquireAzureOauthToken(p.AzureTenantID)
		if err != nil {
			return nil, err
		}
	default:
		password = p.Password
	}

	config := Config{
		Scheme:                          p.Scheme,
		Host:                            p.Host,
		Port:                            p.Port,
		Username:                        p.Username,
		Password:                        password,
		DatabaseUsername:                p.DatabaseUsername,
		Superuser:                       p.Superuser,
		SSLMode:                         sslMode,
		ApplicationName:                 "Terraform provider",
		ConnectTimeoutSec:               p.ConnectTimeout,
		MaxConns:                        p.MaxConnections,
		ExpectedVersion:                 version,
		SSLRootCertPath:                 p.SSLRootCertPath,
		GCPIAMImpersonateServiceAccount: p.GCPIAMImpersonateServiceAccount,
		SSLClientCert:                   p.ClientCert,
	}

	if config.Scheme == "gcppostgres" {
		if err := createGoogleCredsFileIfNeeded(); err != nil {
			return nil, err
		}
	}

	if p.Tunnel != nil {
		if config.Scheme != "postgres" {
			return nil, fmt.Errorf("postgresql: a network transport (aws_ssm/ssh_bastion/azure_bastion/gcp_iap) is only supported with scheme \"postgres\"")
		}
		config.Tunnel = p.Tunnel
	}

	return config.NewClient(p.Database), nil
}
