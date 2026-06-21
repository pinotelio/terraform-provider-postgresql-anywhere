package postgresql

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"

	"github.com/gorilla/websocket"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// GCP Identity-Aware Proxy TCP forwarding, implemented in-process (no gcloud
// CLI). The provider opens a WebSocket to the IAP relay for each client
// connection and bridges bytes using IAP's documented binary framing. The OAuth
// token comes from Google Application Default Credentials, so workload-identity
// federation / OIDC works with no static keys.
//
// IAP terminates at the instance's port, so reaching a managed database means
// the instance must relay onward to it (point instance_port at the database, or
// at a relay on the VM).

const (
	iapTunnelHost      = "tunnel.cloudproxy.app"
	iapConnectPath     = "/v4/connect"
	iapSubprotocol     = "relay.tunnel.cloudproxy.app"
	iapScope           = "https://www.googleapis.com/auth/cloud-platform"
	iapMaxDataFrame    = 16384
	iapTagConnectSID   = 0x0001
	iapTagReconnectACK = 0x0002
	iapTagData         = 0x0004
	iapTagACK          = 0x0007
)

// GCPIAPTunnelConfig reaches a private GCE instance's port through IAP.
type GCPIAPTunnelConfig struct {
	Instance     string // GCE instance name (required)
	Zone         string // instance zone (required)
	Project      string // project id (required for IAP)
	NetInterface string // network interface (default "nic0")
	InstancePort int    // port on the instance to reach (required)
	LocalPort    int    // local listener port (0 => OS-chosen)
}

var (
	gcpIAPRegistryLock sync.Mutex
	gcpIAPRegistry     = map[string]*loopbackTunnel{}
)

func (cfg *GCPIAPTunnelConfig) registryKey() string {
	return fmt.Sprintf("gcp|%s|%s|%s|%d", cfg.Project, cfg.Zone, cfg.Instance, cfg.InstancePort)
}

// EnsureUp starts or reuses the IAP tunnel and returns its local loopback address.
func (cfg *GCPIAPTunnelConfig) EnsureUp() (string, int, error) {
	if cfg.Instance == "" || cfg.Zone == "" || cfg.InstancePort == 0 {
		return "", 0, fmt.Errorf("gcp_iap: instance, zone and instance_port are required")
	}
	if cfg.Project == "" {
		return "", 0, fmt.Errorf("gcp_iap: project is required")
	}

	gcpIAPRegistryLock.Lock()
	defer gcpIAPRegistryLock.Unlock()
	key := cfg.registryKey()
	if t, ok := gcpIAPRegistry[key]; ok {
		return t.localHost, t.localPort, nil
	}

	ctx := context.Background()
	tokenSource, err := iapTokenSource(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("gcp_iap: could not obtain Google credentials (configure Application Default Credentials / workload identity): %w", err)
	}

	iface := cfg.NetInterface
	if iface == "" {
		iface = "nic0"
	}

	t, err := startLoopbackTunnel(cfg.LocalPort, func(local net.Conn, closed <-chan struct{}) {
		cfg.handleConn(ctx, tokenSource, iface, local, closed)
	})
	if err != nil {
		return "", 0, fmt.Errorf("gcp_iap: could not open local listener: %w", err)
	}
	gcpIAPRegistry[key] = t
	log.Printf("[INFO] gcp_iap tunnel: listening on %s:%d, forwarding to %s/%s/%s:%d",
		t.localHost, t.localPort, cfg.Project, cfg.Zone, cfg.Instance, cfg.InstancePort)
	return t.localHost, t.localPort, nil
}

// Close tears down the registered IAP tunnel for this config, if any.
func (cfg *GCPIAPTunnelConfig) Close() error {
	gcpIAPRegistryLock.Lock()
	defer gcpIAPRegistryLock.Unlock()
	key := cfg.registryKey()
	t, ok := gcpIAPRegistry[key]
	if !ok {
		return nil
	}
	delete(gcpIAPRegistry, key)
	return t.close()
}

// handleConn opens an IAP WebSocket for one client connection and relays bytes.
func (cfg *GCPIAPTunnelConfig) handleConn(ctx context.Context, ts oauth2.TokenSource, iface string, local net.Conn, closed <-chan struct{}) {
	defer local.Close()

	tok, err := ts.Token()
	if err != nil {
		log.Printf("[WARN] gcp_iap tunnel: could not get token: %v", err)
		return
	}

	u := url.URL{Scheme: "wss", Host: iapTunnelHost, Path: iapConnectPath}
	q := u.Query()
	q.Set("project", cfg.Project)
	q.Set("zone", cfg.Zone)
	q.Set("instance", cfg.Instance)
	q.Set("interface", iface)
	q.Set("port", strconv.Itoa(cfg.InstancePort))
	u.RawQuery = q.Encode()

	header := http.Header{}
	header.Set("Authorization", "Bearer "+tok.AccessToken)
	header.Set("User-Agent", "terraform-provider-postgresql-anywhere")

	ws, err := iapDialWebsocket(ctx, u.String(), header)
	if err != nil {
		log.Printf("[WARN] gcp_iap tunnel: could not open IAP websocket: %v", err)
		return
	}
	defer ws.Close()

	iapRelay(local, ws, closed)
}

// iapDialWebsocket opens the IAP relay WebSocket. It is a variable so tests can
// point it at a fake relay.
var iapDialWebsocket = func(ctx context.Context, urlStr string, header http.Header) (*websocket.Conn, error) {
	dialer := websocket.Dialer{Subprotocols: []string{iapSubprotocol}}
	ws, _, err := dialer.DialContext(ctx, urlStr, header)
	return ws, err
}

// iapTokenSource returns the Google token source used for IAP. It is a variable
// so tests can inject a static token.
var iapTokenSource = func(ctx context.Context) (oauth2.TokenSource, error) {
	return google.DefaultTokenSource(ctx, iapScope)
}

// iapRelay bridges a client connection and the IAP WebSocket using IAP's binary
// framing: DATA frames carry payload in both directions and ACK frames report
// bytes received for flow control.
func iapRelay(local net.Conn, ws *websocket.Conn, closed <-chan struct{}) {
	var writeMu sync.Mutex
	writeFrame := func(b []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return ws.WriteMessage(websocket.BinaryMessage, b)
	}

	// ws -> local
	go func() {
		var received uint64
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				local.Close()
				return
			}
			for len(msg) >= 2 {
				tag := binary.BigEndian.Uint16(msg[0:2])
				switch tag {
				case iapTagData:
					if len(msg) < 6 {
						return
					}
					n := binary.BigEndian.Uint32(msg[2:6])
					if uint32(len(msg)) < 6+n {
						return
					}
					if _, err := local.Write(msg[6 : 6+n]); err != nil {
						return
					}
					received += uint64(n)
					_ = writeFrame(iapAckFrame(received))
					msg = msg[6+n:]
				case iapTagConnectSID:
					if len(msg) < 6 {
						return
					}
					n := binary.BigEndian.Uint32(msg[2:6])
					if uint32(len(msg)) < 6+n {
						return
					}
					msg = msg[6+n:]
				case iapTagACK, iapTagReconnectACK:
					if len(msg) < 10 {
						return
					}
					msg = msg[10:]
				default:
					return
				}
			}
		}
	}()

	// local -> ws
	buf := make([]byte, iapMaxDataFrame)
	for {
		select {
		case <-closed:
			return
		default:
		}
		n, err := local.Read(buf)
		if n > 0 {
			if werr := writeFrame(iapDataFrame(buf[:n])); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func iapDataFrame(data []byte) []byte {
	out := make([]byte, 6+len(data))
	binary.BigEndian.PutUint16(out[0:2], iapTagData)
	binary.BigEndian.PutUint32(out[2:6], uint32(len(data)))
	copy(out[6:], data)
	return out
}

func iapAckFrame(total uint64) []byte {
	out := make([]byte, 10)
	binary.BigEndian.PutUint16(out[0:2], iapTagACK)
	binary.BigEndian.PutUint64(out[2:10], total)
	return out
}
