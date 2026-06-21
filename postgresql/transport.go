package postgresql

// Tunnel makes a PostgreSQL endpoint that is only reachable from inside a
// private network reachable from the machine running Terraform.
//
// EnsureUp starts (or reuses) the tunnel and returns the local loopback
// host:port that forwards to the configured private endpoint; the provider
// connects there instead of the real endpoint. Exactly one transport is active
// at a time: AWS SSM, SSH bastion, Azure Bastion, or GCP IAP.
type Tunnel interface {
	EnsureUp() (localHost string, localPort int, err error)
	Close() error
}

// EnsureUp starts or reuses the AWS SSM port-forwarding tunnel for this config.
func (cfg *SSMTunnelConfig) EnsureUp() (string, int, error) {
	t, err := GetOrStartSSMTunnel(*cfg)
	if err != nil {
		return "", 0, err
	}
	return t.LocalHost(), t.LocalPort(), nil
}

// Close tears down the registered SSM tunnel for this config, if any.
func (cfg *SSMTunnelConfig) Close() error {
	tunnelRegistryLock.Lock()
	defer tunnelRegistryLock.Unlock()
	key := cfg.registryKey()
	t, ok := tunnelRegistry[key]
	if !ok {
		return nil
	}
	delete(tunnelRegistry, key)
	return t.Close()
}
