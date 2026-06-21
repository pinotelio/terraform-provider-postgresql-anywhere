package postgresql

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/gorilla/websocket"
)

// Azure Bastion native-client tunnel, implemented in-process (no az CLI). The
// AAD token comes from azidentity.DefaultAzureCredential, so workload-identity
// federation / OIDC works with no static keys.
//
// EXPERIMENTAL: Azure does not publicly document the Bastion tunnel data-plane
// protocol; this follows the flow used by `az network bastion tunnel` and must
// be validated against a real Azure Bastion (a Standard/Premium SKU with native
// client support enabled) before you rely on it. The tunnel terminates at the
// target VM's resource_port, so a managed database needs that VM to relay onward.

const (
	azureARMScope          = "https://management.azure.com/.default"
	azureBastionAPIVersion = "2023-11-01"
)

// AzureBastionTunnelConfig reaches a private VM's port through Azure Bastion.
type AzureBastionTunnelConfig struct {
	BastionName      string // Azure Bastion resource name (required)
	ResourceGroup    string // resource group of the bastion (required)
	TargetResourceID string // resource id of the target VM (required)
	Subscription     string // subscription id (or parsed from TargetResourceID)
	ResourcePort     int    // port on the target VM to reach (required)
	LocalPort        int    // local listener port (0 => OS-chosen)
}

var (
	azureBastionRegistryLock sync.Mutex
	azureBastionRegistry     = map[string]*loopbackTunnel{}
)

func (cfg *AzureBastionTunnelConfig) registryKey() string {
	return fmt.Sprintf("azure|%s|%s|%s|%d", cfg.ResourceGroup, cfg.BastionName, cfg.TargetResourceID, cfg.ResourcePort)
}

func (cfg *AzureBastionTunnelConfig) subscriptionID() string {
	if cfg.Subscription != "" {
		return cfg.Subscription
	}
	parts := strings.Split(strings.TrimPrefix(cfg.TargetResourceID, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if strings.EqualFold(parts[i], "subscriptions") {
			return parts[i+1]
		}
	}
	return ""
}

// EnsureUp starts or reuses the Azure Bastion tunnel and returns its local address.
func (cfg *AzureBastionTunnelConfig) EnsureUp() (string, int, error) {
	if cfg.BastionName == "" || cfg.ResourceGroup == "" || cfg.TargetResourceID == "" || cfg.ResourcePort == 0 {
		return "", 0, fmt.Errorf("azure_bastion: bastion_name, resource_group, target_resource_id and resource_port are required")
	}
	sub := cfg.subscriptionID()
	if sub == "" {
		return "", 0, fmt.Errorf("azure_bastion: subscription is required (set subscription or use a full target_resource_id)")
	}

	azureBastionRegistryLock.Lock()
	defer azureBastionRegistryLock.Unlock()
	key := cfg.registryKey()
	if t, ok := azureBastionRegistry[key]; ok {
		return t.localHost, t.localPort, nil
	}

	ctx := context.Background()
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return "", 0, fmt.Errorf("azure_bastion: could not obtain Azure credentials (configure workload identity / managed identity): %w", err)
	}

	dnsName, err := cfg.bastionDNSName(ctx, cred, sub)
	if err != nil {
		return "", 0, err
	}

	log.Printf("[WARN] azure_bastion tunnel is experimental and unverified against live Azure; validate it before relying on it")

	t, err := startLoopbackTunnel(cfg.LocalPort, func(local net.Conn, closed <-chan struct{}) {
		cfg.handleConn(ctx, cred, dnsName, local, closed)
	})
	if err != nil {
		return "", 0, fmt.Errorf("azure_bastion: could not open local listener: %w", err)
	}
	azureBastionRegistry[key] = t
	log.Printf("[INFO] azure_bastion tunnel: listening on %s:%d, forwarding to %s:%d via bastion %s",
		t.localHost, t.localPort, cfg.TargetResourceID, cfg.ResourcePort, cfg.BastionName)
	return t.localHost, t.localPort, nil
}

// Close tears down the registered Azure Bastion tunnel for this config, if any.
func (cfg *AzureBastionTunnelConfig) Close() error {
	azureBastionRegistryLock.Lock()
	defer azureBastionRegistryLock.Unlock()
	key := cfg.registryKey()
	t, ok := azureBastionRegistry[key]
	if !ok {
		return nil
	}
	delete(azureBastionRegistry, key)
	return t.close()
}

func (cfg *AzureBastionTunnelConfig) armToken(ctx context.Context, cred *azidentity.DefaultAzureCredential) (string, error) {
	tok, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{azureARMScope}})
	if err != nil {
		return "", fmt.Errorf("azure_bastion: could not get AAD token: %w", err)
	}
	return tok.Token, nil
}

// bastionDNSName resolves the bastion's data-plane DNS name from ARM.
func (cfg *AzureBastionTunnelConfig) bastionDNSName(ctx context.Context, cred *azidentity.DefaultAzureCredential, sub string) (string, error) {
	tok, err := cfg.armToken(ctx, cred)
	if err != nil {
		return "", err
	}
	endpoint := fmt.Sprintf("https://management.azure.com/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/bastionHosts/%s?api-version=%s",
		url.PathEscape(sub), url.PathEscape(cfg.ResourceGroup), url.PathEscape(cfg.BastionName), azureBastionAPIVersion)

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("azure_bastion: could not read bastion resource: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("azure_bastion: bastion GET returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var body struct {
		Properties struct {
			DNSName string `json:"dnsName"`
		} `json:"properties"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("azure_bastion: could not parse bastion resource: %w", err)
	}
	if body.Properties.DNSName == "" {
		return "", fmt.Errorf("azure_bastion: bastion has no dnsName (a Standard/Premium SKU with native client support is required)")
	}
	return body.Properties.DNSName, nil
}

// sessionToken obtains a per-session token from the bastion data plane.
func (cfg *AzureBastionTunnelConfig) sessionToken(ctx context.Context, dnsName, aadToken string) (string, error) {
	form := url.Values{}
	form.Set("resourceId", cfg.TargetResourceID)
	form.Set("protocol", "tcptunnel")
	form.Set("workloadHostPort", strconv.Itoa(cfg.ResourcePort))
	form.Set("aztoken", aadToken)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("https://%s/api/tokens", dnsName), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("azure_bastion: could not get bastion session token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("azure_bastion: token endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var body struct {
		AuthToken string `json:"authToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("azure_bastion: could not parse bastion token: %w", err)
	}
	if body.AuthToken == "" {
		return "", fmt.Errorf("azure_bastion: bastion returned an empty auth token")
	}
	return body.AuthToken, nil
}

// handleConn opens a bastion websocket for one client connection and relays raw bytes.
func (cfg *AzureBastionTunnelConfig) handleConn(ctx context.Context, cred *azidentity.DefaultAzureCredential, dnsName string, local net.Conn, closed <-chan struct{}) {
	defer local.Close()

	aadToken, err := cfg.armToken(ctx, cred)
	if err != nil {
		log.Printf("[WARN] azure_bastion tunnel: %v", err)
		return
	}
	authToken, err := cfg.sessionToken(ctx, dnsName, aadToken)
	if err != nil {
		log.Printf("[WARN] azure_bastion tunnel: %v", err)
		return
	}

	u := url.URL{Scheme: "wss", Host: dnsName, Path: "/webtunnelv2/" + authToken}
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		log.Printf("[WARN] azure_bastion tunnel: could not open websocket: %v", err)
		return
	}
	defer ws.Close()

	var writeMu sync.Mutex
	go func() {
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				local.Close()
				return
			}
			if _, err := local.Write(msg); err != nil {
				return
			}
		}
	}()

	buf := make([]byte, 16384)
	for {
		select {
		case <-closed:
			return
		default:
		}
		n, err := local.Read(buf)
		if n > 0 {
			writeMu.Lock()
			werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n])
			writeMu.Unlock()
			if werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
