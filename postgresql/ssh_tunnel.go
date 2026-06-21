package postgresql

import (
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// SSHTunnelConfig describes an SSH bastion the provider tunnels through to reach
// a PostgreSQL endpoint inside a private network. The bastion must be reachable
// from where Terraform runs and must have network line-of-sight to
// RemoteHost:RemotePort.
type SSHTunnelConfig struct {
	Host                 string // bastion host (required)
	Port                 int    // bastion SSH port (default 22)
	User                 string // SSH user (required)
	Password             string // password auth (optional)
	PrivateKey           string // PEM-encoded private key (optional)
	PrivateKeyPassphrase string // passphrase for an encrypted PrivateKey (optional)

	// Host-key verification. Set one of HostKey or KnownHostsFile, or set
	// InsecureIgnoreHostKey to disable verification (not recommended).
	HostKey               string // bastion public key in authorized_keys format
	KnownHostsFile        string // path to a known_hosts file
	InsecureIgnoreHostKey bool

	RemoteHost string // database endpoint reachable from the bastion (required)
	RemotePort int    // database port reachable from the bastion (required)
	LocalPort  int    // local listener port (0 => OS-chosen)
}

type sshTunnel struct {
	cfg       SSHTunnelConfig
	client    *ssh.Client
	listener  net.Listener
	localHost string
	localPort int
	closeOnce sync.Once
	closed    chan struct{}
}

var (
	sshTunnelRegistryLock sync.Mutex
	sshTunnelRegistry     = map[string]*sshTunnel{}
)

func (cfg *SSHTunnelConfig) registryKey() string {
	return fmt.Sprintf("%s:%d|%s|%s:%d", cfg.Host, cfg.Port, cfg.User, cfg.RemoteHost, cfg.RemotePort)
}

// EnsureUp starts or reuses the SSH tunnel and returns its local loopback address.
func (cfg *SSHTunnelConfig) EnsureUp() (string, int, error) {
	sshTunnelRegistryLock.Lock()
	defer sshTunnelRegistryLock.Unlock()

	key := cfg.registryKey()
	if t, ok := sshTunnelRegistry[key]; ok {
		return t.localHost, t.localPort, nil
	}
	t, err := startSSHTunnel(*cfg)
	if err != nil {
		return "", 0, err
	}
	sshTunnelRegistry[key] = t
	return t.localHost, t.localPort, nil
}

// Close tears down the registered SSH tunnel for this config, if any.
func (cfg *SSHTunnelConfig) Close() error {
	sshTunnelRegistryLock.Lock()
	defer sshTunnelRegistryLock.Unlock()
	key := cfg.registryKey()
	t, ok := sshTunnelRegistry[key]
	if !ok {
		return nil
	}
	delete(sshTunnelRegistry, key)
	return t.close()
}

func (cfg SSHTunnelConfig) authMethods() ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if cfg.PrivateKey != "" {
		var signer ssh.Signer
		var err error
		if cfg.PrivateKeyPassphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(cfg.PrivateKey), []byte(cfg.PrivateKeyPassphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(cfg.PrivateKey))
		}
		if err != nil {
			return nil, fmt.Errorf("ssh_bastion: could not parse private_key: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if cfg.Password != "" {
		methods = append(methods, ssh.Password(cfg.Password))
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("ssh_bastion: set one of private_key or password")
	}
	return methods, nil
}

func (cfg SSHTunnelConfig) hostKeyCallback() (ssh.HostKeyCallback, error) {
	switch {
	case cfg.InsecureIgnoreHostKey:
		return ssh.InsecureIgnoreHostKey(), nil
	case cfg.HostKey != "":
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(cfg.HostKey))
		if err != nil {
			return nil, fmt.Errorf("ssh_bastion: could not parse host_key (expected authorized_keys format): %w", err)
		}
		return ssh.FixedHostKey(pub), nil
	case cfg.KnownHostsFile != "":
		cb, err := knownhosts.New(cfg.KnownHostsFile)
		if err != nil {
			return nil, fmt.Errorf("ssh_bastion: could not load known_hosts_file: %w", err)
		}
		return cb, nil
	default:
		return nil, fmt.Errorf("ssh_bastion: host-key verification required; set one of host_key, known_hosts_file, or insecure_ignore_host_key")
	}
}

func startSSHTunnel(cfg SSHTunnelConfig) (*sshTunnel, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("ssh_bastion: host must be set")
	}
	if cfg.User == "" {
		return nil, fmt.Errorf("ssh_bastion: user must be set")
	}
	if cfg.RemoteHost == "" || cfg.RemotePort == 0 {
		return nil, fmt.Errorf("ssh_bastion: the database endpoint (host/port) must be set")
	}

	port := cfg.Port
	if port == 0 {
		port = 22
	}

	auth, err := cfg.authMethods()
	if err != nil {
		return nil, err
	}
	hostKeyCallback, err := cfg.hostKeyCallback()
	if err != nil {
		return nil, err
	}

	sshConf := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            auth,
		HostKeyCallback: hostKeyCallback,
		Timeout:         30 * time.Second,
	}

	bastionAddr := net.JoinHostPort(cfg.Host, strconv.Itoa(port))
	client, err := ssh.Dial("tcp", bastionAddr, sshConf)
	if err != nil {
		return nil, fmt.Errorf("ssh_bastion: could not connect to %s: %w", bastionAddr, err)
	}

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.LocalPort))
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ssh_bastion: could not open local listener: %w", err)
	}

	t := &sshTunnel{
		cfg:       cfg,
		client:    client,
		listener:  listener,
		localHost: "127.0.0.1",
		localPort: listener.Addr().(*net.TCPAddr).Port,
		closed:    make(chan struct{}),
	}
	go t.acceptLoop()

	log.Printf("[INFO] ssh tunnel: listening on %s:%d, forwarding to %s:%d via %s",
		t.localHost, t.localPort, cfg.RemoteHost, cfg.RemotePort, bastionAddr)
	return t, nil
}

func (t *sshTunnel) acceptLoop() {
	for {
		local, err := t.listener.Accept()
		if err != nil {
			select {
			case <-t.closed:
			default:
				log.Printf("[WARN] ssh tunnel: accept error: %v", err)
			}
			return
		}
		go t.handleConn(local)
	}
}

func (t *sshTunnel) handleConn(local net.Conn) {
	defer func() { _ = local.Close() }()

	remoteAddr := net.JoinHostPort(t.cfg.RemoteHost, strconv.Itoa(t.cfg.RemotePort))
	remote, err := t.client.Dial("tcp", remoteAddr)
	if err != nil {
		log.Printf("[WARN] ssh tunnel: could not reach %s through bastion: %v", remoteAddr, err)
		return
	}
	defer func() { _ = remote.Close() }()

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(remote, local); done <- struct{}{} }()
	go func() { _, _ = io.Copy(local, remote); done <- struct{}{} }()
	select {
	case <-done:
	case <-t.closed:
	}
}

func (t *sshTunnel) close() error {
	var err error
	t.closeOnce.Do(func() {
		close(t.closed)
		if t.listener != nil {
			_ = t.listener.Close()
		}
		if t.client != nil {
			err = t.client.Close()
		}
	})
	return err
}
