package postgresql

// SSM tunnel support.
//
// This lets the provider reach a PostgreSQL endpoint (typically a private AWS
// RDS instance) that is only reachable from inside a VPC, by opening an AWS
// Systems Manager (SSM) "port forwarding to remote host" session through a
// bastion EC2 instance. The tunnel runs inside the provider process and is tied
// to the connection lifecycle, so it does not require the external
// session-manager-plugin binary on the Terraform runner.
//
// High level flow:
//
//   1. Resolve AWS credentials (profile, assumed role, or the default chain,
//      which covers OIDC web-identity).
//   2. Discover the bastion EC2 instance id (explicit id, Name tag, or an
//      arbitrary tag map) via the EC2 API.
//   3. Open a local TCP listener; the provider connects to 127.0.0.1:<localPort>
//      instead of the real RDS endpoint.
//   4. For every accepted local connection, call ssm:StartSession with the
//      AWS-StartPortForwardingSessionToRemoteHost document, open the returned
//      WebSocket data channel, perform the SSM agent handshake, and bridge bytes
//      in both directions until the local connection closes.
//
// The binary AgentMessage framing follows the AWS Session Manager message
// format. Set TF_LOG=DEBUG to see the tunnel's debug logging.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/gorilla/websocket"
)

// SSMTunnelConfig holds the user-supplied inputs for the SSM tunnel.
type SSMTunnelConfig struct {
	Region       string            // AWS region the bastion / RDS live in (required)
	Profile      string            // optional shared-config profile
	AccessKey    string            // optional static access key id
	SecretKey    string            // optional static secret access key
	SessionToken string            // optional static session token
	RoleARN      string            // optional role to assume before opening the session
	InstanceID   string            // explicit bastion instance id (takes precedence)
	InstanceName string            // shortcut for tag:Name discovery
	InstanceTags map[string]string // arbitrary tag=value discovery filters
	RemoteHost   string            // the real RDS endpoint to forward to
	RemotePort   int               // the real RDS port to forward to
	LocalPort    int               // local port to listen on (0 => OS-chosen)
}

// SSMTunnel is a running tunnel: a local TCP listener that proxies each
// connection over its own SSM port-forwarding session.
type SSMTunnel struct {
	cfg        SSMTunnelConfig
	instanceID string
	ssmClient  *ssm.Client
	listener   net.Listener
	localHost  string
	localPort  int

	closeOnce sync.Once
	closed    chan struct{}
}

// LocalHost returns the loopback host the provider should connect to.
func (t *SSMTunnel) LocalHost() string { return t.localHost }

// LocalPort returns the local port the tunnel listens on.
func (t *SSMTunnel) LocalPort() int { return t.localPort }

// Close shuts the listener down. In-flight sessions terminate on their own.
func (t *SSMTunnel) Close() error {
	var err error
	t.closeOnce.Do(func() {
		close(t.closed)
		if t.listener != nil {
			err = t.listener.Close()
		}
	})
	return err
}

// tunnelRegistry caches tunnels so that repeated Connect() calls for different
// databases reuse a single listener / bastion discovery per remote endpoint.
var (
	tunnelRegistryLock sync.Mutex
	tunnelRegistry     = map[string]*SSMTunnel{}
)

func (c *SSMTunnelConfig) registryKey() string {
	return fmt.Sprintf("%s|%s|%s|%s|%d", c.Region, c.InstanceID, c.InstanceName, c.RemoteHost, c.RemotePort)
}

// GetOrStartSSMTunnel returns an existing tunnel for the given config or starts
// a new one. The returned tunnel exposes a stable local address.
func GetOrStartSSMTunnel(cfg SSMTunnelConfig) (*SSMTunnel, error) {
	tunnelRegistryLock.Lock()
	defer tunnelRegistryLock.Unlock()

	key := cfg.registryKey()
	if t, ok := tunnelRegistry[key]; ok {
		return t, nil
	}

	t, err := startSSMTunnel(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	tunnelRegistry[key] = t
	return t, nil
}

func startSSMTunnel(ctx context.Context, cfg SSMTunnelConfig) (*SSMTunnel, error) {
	if cfg.Region == "" {
		return nil, fmt.Errorf("ssm tunnel: aws_ssm_region must be set")
	}
	if cfg.RemoteHost == "" {
		return nil, fmt.Errorf("ssm tunnel: remote host (RDS endpoint / host) must be set")
	}
	if cfg.RemotePort == 0 {
		cfg.RemotePort = 5432
	}

	awscfg, err := loadSSMAWSConfig(ctx, cfg.Region, cfg.Profile, cfg.RoleARN, cfg.AccessKey, cfg.SecretKey, cfg.SessionToken)
	if err != nil {
		return nil, fmt.Errorf("ssm tunnel: loading AWS config: %w", err)
	}

	instanceID := cfg.InstanceID
	if instanceID == "" {
		instanceID, err = discoverBastion(ctx, ec2.NewFromConfig(awscfg), cfg)
		if err != nil {
			return nil, fmt.Errorf("ssm tunnel: discovering bastion: %w", err)
		}
		log.Printf("[INFO] ssm tunnel: discovered bastion instance %s", instanceID)
	}

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.LocalPort))
	if err != nil {
		return nil, fmt.Errorf("ssm tunnel: opening local listener: %w", err)
	}
	addr := listener.Addr().(*net.TCPAddr)

	t := &SSMTunnel{
		cfg:        cfg,
		instanceID: instanceID,
		ssmClient:  ssm.NewFromConfig(awscfg),
		listener:   listener,
		localHost:  "127.0.0.1",
		localPort:  addr.Port,
		closed:     make(chan struct{}),
	}

	log.Printf("[INFO] ssm tunnel: listening on 127.0.0.1:%d -> %s:%d via %s (region %s)",
		t.localPort, cfg.RemoteHost, cfg.RemotePort, instanceID, cfg.Region)

	go t.acceptLoop()
	return t, nil
}

func (t *SSMTunnel) acceptLoop() {
	for {
		conn, err := t.listener.Accept()
		if err != nil {
			select {
			case <-t.closed:
				return
			default:
				log.Printf("[WARN] ssm tunnel: accept error: %v", err)
				return
			}
		}
		go t.handleConn(conn)
	}
}

// handleConn opens a dedicated SSM port-forwarding session for one local
// connection and bridges the two until either side closes.
func (t *SSMTunnel) handleConn(local net.Conn) {
	defer local.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, err := t.ssmClient.StartSession(ctx, &ssm.StartSessionInput{
		Target:       aws.String(t.instanceID),
		DocumentName: aws.String("AWS-StartPortForwardingSessionToRemoteHost"),
		Parameters: map[string][]string{
			"host":       {t.cfg.RemoteHost},
			"portNumber": {strconv.Itoa(t.cfg.RemotePort)},
		},
	})
	if err != nil {
		log.Printf("[ERROR] ssm tunnel: StartSession failed: %v", err)
		return
	}
	sessionID := aws.ToString(out.SessionId)
	log.Printf("[DEBUG] ssm tunnel: started session %s", sessionID)

	defer func() {
		tctx, tcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer tcancel()
		if _, e := t.ssmClient.TerminateSession(tctx, &ssm.TerminateSessionInput{
			SessionId: aws.String(sessionID),
		}); e != nil {
			log.Printf("[WARN] ssm tunnel: TerminateSession %s failed: %v", sessionID, e)
		} else {
			log.Printf("[DEBUG] ssm tunnel: terminated session %s", sessionID)
		}
	}()

	ws, _, err := websocket.DefaultDialer.DialContext(ctx, aws.ToString(out.StreamUrl), nil)
	if err != nil {
		log.Printf("[ERROR] ssm tunnel: websocket dial failed: %v", err)
		return
	}
	defer ws.Close()

	dc := &ssmDataChannel{
		ws:            ws,
		token:         aws.ToString(out.TokenValue),
		local:         local,
		handshakeDone: make(chan struct{}),
	}
	if err := dc.open(); err != nil {
		log.Printf("[ERROR] ssm tunnel: open data channel: %v", err)
		return
	}
	dc.run(ctx)
	log.Printf("[DEBUG] ssm tunnel: session %s closed", sessionID)
}

func loadSSMAWSConfig(ctx context.Context, region, profile, roleARN, accessKey, secretKey, sessionToken string) (aws.Config, error) {
	opts := []func(*awsConfig.LoadOptions) error{awsConfig.WithRegion(region)}
	if profile != "" {
		opts = append(opts, awsConfig.WithSharedConfigProfile(profile))
	}
	if accessKey != "" && secretKey != "" {
		opts = append(opts, awsConfig.WithCredentialsProvider(
			aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(accessKey, secretKey, sessionToken)),
		))
	}
	awscfg, err := awsConfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, err
	}

	if roleARN != "" {
		stsClient := sts.NewFromConfig(awscfg)
		creds, err := stsClient.AssumeRole(ctx, &sts.AssumeRoleInput{
			RoleArn:         aws.String(roleARN),
			RoleSessionName: aws.String("TerraformPostgresqlProviderSSM"),
		})
		if err != nil {
			return aws.Config{}, fmt.Errorf("could not assume role %s: %w", roleARN, err)
		}
		awscfg.Credentials = aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(
			aws.ToString(creds.Credentials.AccessKeyId),
			aws.ToString(creds.Credentials.SecretAccessKey),
			aws.ToString(creds.Credentials.SessionToken),
		))
	}
	return awscfg, nil
}

// discoverBastion finds a single running EC2 instance matching the configured
// Name / tag filters.
func discoverBastion(ctx context.Context, client *ec2.Client, cfg SSMTunnelConfig) (string, error) {
	filters := []ec2types.Filter{
		{Name: aws.String("instance-state-name"), Values: []string{"running"}},
	}
	if cfg.InstanceName != "" {
		filters = append(filters, ec2types.Filter{
			Name:   aws.String("tag:Name"),
			Values: []string{cfg.InstanceName},
		})
	}
	for k, v := range cfg.InstanceTags {
		filters = append(filters, ec2types.Filter{
			Name:   aws.String("tag:" + k),
			Values: []string{v},
		})
	}
	if len(filters) == 1 {
		return "", fmt.Errorf("no discovery filter set: provide aws_ssm_instance_id, aws_ssm_instance_name, or aws_ssm_instance_tags")
	}

	out, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: filters})
	if err != nil {
		return "", err
	}
	var ids []string
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			ids = append(ids, aws.ToString(inst.InstanceId))
		}
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("no running instance matched the discovery filters")
	case 1:
		return ids[0], nil
	default:
		log.Printf("[WARN] ssm tunnel: %d instances matched discovery filters, using %s", len(ids), ids[0])
		return ids[0], nil
	}
}

// ---------------------------------------------------------------------------
// SSM agent message (data channel) protocol
// ---------------------------------------------------------------------------

// Field offsets in the serialized AgentMessage (big-endian).
const (
	smHeaderLength         = 4
	smMessageTypeLength    = 32
	smSchemaVersionLength  = 4
	smCreatedDateLength    = 8
	smSequenceNumberLength = 8
	smFlagsLength          = 8
	smMessageIDLength      = 16
	smPayloadDigestLength  = 32
	smPayloadTypeLength    = 4
	smPayloadLengthLength  = 4

	smHLOffset             = 0
	smMessageTypeOffset    = smHLOffset + smHeaderLength                     // 4
	smSchemaVersionOffset  = smMessageTypeOffset + smMessageTypeLength       // 36
	smCreatedDateOffset    = smSchemaVersionOffset + smSchemaVersionLength   // 40
	smSequenceNumberOffset = smCreatedDateOffset + smCreatedDateLength       // 48
	smFlagsOffset          = smSequenceNumberOffset + smSequenceNumberLength // 56
	smMessageIDOffset      = smFlagsOffset + smFlagsLength                   // 64
	smPayloadDigestOffset  = smMessageIDOffset + smMessageIDLength           // 80
	smPayloadTypeOffset    = smPayloadDigestOffset + smPayloadDigestLength   // 112
	smPayloadLengthOffset  = smPayloadTypeOffset + smPayloadTypeLength       // 116
	smPayloadOffset        = smPayloadLengthOffset + smPayloadLengthLength   // 120
)

// AgentMessage message types.
const (
	smInputStreamData  = "input_stream_data"
	smOutputStreamData = "output_stream_data"
	smAcknowledge      = "acknowledge"
	smChannelClosed    = "channel_closed"
	smStartPublication = "start_publication"
	smPausePublication = "pause_publication"
)

// AgentMessage payload types.
const (
	smPayloadTypeOutput            uint32 = 1
	smPayloadTypeError             uint32 = 2
	smPayloadTypeHandshakeRequest  uint32 = 5
	smPayloadTypeHandshakeResponse uint32 = 6
	smPayloadTypeHandshakeComplete uint32 = 7
)

// flagSyn marks the first data message of a stream.
const flagSyn uint64 = 1

type agentMessage struct {
	MessageType    string
	SchemaVersion  uint32
	CreatedDate    uint64
	SequenceNumber int64
	Flags          uint64
	MessageID      [16]byte
	PayloadType    uint32
	Payload        []byte
}

func (m *agentMessage) serialize() []byte {
	payloadLen := uint32(len(m.Payload))
	buf := make([]byte, smPayloadOffset+int(payloadLen))

	binary.BigEndian.PutUint32(buf[smHLOffset:], uint32(smPayloadLengthOffset))

	mt := m.MessageType
	if len(mt) > smMessageTypeLength {
		mt = mt[:smMessageTypeLength]
	}
	// Message type is a fixed 32-byte, space-padded field.
	field := buf[smMessageTypeOffset : smMessageTypeOffset+smMessageTypeLength]
	copy(field, mt)
	for i := len(mt); i < smMessageTypeLength; i++ {
		field[i] = ' '
	}

	binary.BigEndian.PutUint32(buf[smSchemaVersionOffset:], m.SchemaVersion)
	binary.BigEndian.PutUint64(buf[smCreatedDateOffset:], m.CreatedDate)
	binary.BigEndian.PutUint64(buf[smSequenceNumberOffset:], uint64(m.SequenceNumber))
	binary.BigEndian.PutUint64(buf[smFlagsOffset:], m.Flags)
	copy(buf[smMessageIDOffset:smMessageIDOffset+smMessageIDLength], m.MessageID[:])

	digest := sha256.Sum256(m.Payload)
	copy(buf[smPayloadDigestOffset:smPayloadDigestOffset+smPayloadDigestLength], digest[:])

	binary.BigEndian.PutUint32(buf[smPayloadTypeOffset:], m.PayloadType)
	binary.BigEndian.PutUint32(buf[smPayloadLengthOffset:], payloadLen)
	copy(buf[smPayloadOffset:], m.Payload)
	return buf
}

func deserializeAgentMessage(buf []byte) (*agentMessage, error) {
	if len(buf) < smPayloadOffset {
		return nil, fmt.Errorf("message too short: %d bytes", len(buf))
	}
	m := &agentMessage{
		MessageType:    strings.TrimSpace(string(buf[smMessageTypeOffset : smMessageTypeOffset+smMessageTypeLength])),
		SchemaVersion:  binary.BigEndian.Uint32(buf[smSchemaVersionOffset:]),
		CreatedDate:    binary.BigEndian.Uint64(buf[smCreatedDateOffset:]),
		SequenceNumber: int64(binary.BigEndian.Uint64(buf[smSequenceNumberOffset:])),
		Flags:          binary.BigEndian.Uint64(buf[smFlagsOffset:]),
		PayloadType:    binary.BigEndian.Uint32(buf[smPayloadTypeOffset:]),
	}
	copy(m.MessageID[:], buf[smMessageIDOffset:smMessageIDOffset+smMessageIDLength])

	payloadLen := int(binary.BigEndian.Uint32(buf[smPayloadLengthOffset:]))
	end := smPayloadOffset + payloadLen
	if payloadLen < 0 || end > len(buf) {
		return nil, fmt.Errorf("invalid payload length %d (buffer %d)", payloadLen, len(buf))
	}
	m.Payload = append([]byte(nil), buf[smPayloadOffset:end]...)
	return m, nil
}

// ssmDataChannel drives one WebSocket session: open -> handshake -> bridge.
type ssmDataChannel struct {
	ws    *websocket.Conn
	token string
	local net.Conn

	writeMu sync.Mutex
	seq     int64

	handshakeDone chan struct{}
	handshakeOnce sync.Once
}

type openDataChannelInput struct {
	MessageSchemaVersion string `json:"MessageSchemaVersion"`
	RequestId            string `json:"RequestId"`
	TokenValue           string `json:"TokenValue"`
	ClientId             string `json:"ClientId"`
	ClientVersion        string `json:"ClientVersion"`
}

func (dc *ssmDataChannel) open() error {
	input := openDataChannelInput{
		MessageSchemaVersion: "1.0",
		RequestId:            uuidString(newUUID()),
		TokenValue:           dc.token,
		ClientId:             uuidString(newUUID()),
		ClientVersion:        "1.2.0.0",
	}
	b, err := json.Marshal(input)
	if err != nil {
		return err
	}
	dc.writeMu.Lock()
	defer dc.writeMu.Unlock()
	return dc.ws.WriteMessage(websocket.TextMessage, b)
}

func (dc *ssmDataChannel) run(ctx context.Context) {
	errc := make(chan error, 2)
	go dc.readLoop(errc)

	// Wait for the agent handshake before pumping local data. If the handshake
	// never arrives we proceed anyway after a grace period (some document
	// versions begin streaming immediately).
	select {
	case <-dc.handshakeDone:
		log.Printf("[DEBUG] ssm tunnel: handshake complete")
	case <-time.After(30 * time.Second):
		log.Printf("[WARN] ssm tunnel: handshake timed out, proceeding to stream")
	case err := <-errc:
		log.Printf("[DEBUG] ssm tunnel: channel ended before handshake: %v", err)
		return
	case <-ctx.Done():
		return
	}

	go dc.writeLoop(errc)
	<-errc
}

func (dc *ssmDataChannel) readLoop(errc chan error) {
	for {
		_, data, err := dc.ws.ReadMessage()
		if err != nil {
			errc <- err
			return
		}
		msg, err := deserializeAgentMessage(data)
		if err != nil {
			log.Printf("[WARN] ssm tunnel: dropping malformed message: %v", err)
			continue
		}

		switch msg.MessageType {
		case smOutputStreamData:
			dc.sendAcknowledge(msg)
			switch msg.PayloadType {
			case smPayloadTypeHandshakeRequest:
				dc.sendHandshakeResponse(msg.Payload)
			case smPayloadTypeHandshakeComplete:
				dc.handshakeOnce.Do(func() { close(dc.handshakeDone) })
			case smPayloadTypeOutput:
				if _, werr := dc.local.Write(msg.Payload); werr != nil {
					errc <- werr
					return
				}
			case smPayloadTypeError:
				log.Printf("[ERROR] ssm tunnel: agent reported error: %s", string(msg.Payload))
			}
		case smAcknowledge:
			// Best-effort: we do not retransmit, so acks are informational.
		case smChannelClosed:
			log.Printf("[INFO] ssm tunnel: channel closed by agent")
			errc <- io.EOF
			return
		case smStartPublication, smPausePublication:
			// Flow-control hints; ignored in this best-effort implementation.
		}
	}
}

func (dc *ssmDataChannel) writeLoop(errc chan error) {
	buf := make([]byte, 1024)
	for {
		n, err := dc.local.Read(buf)
		if n > 0 {
			if werr := dc.sendData(buf[:n]); werr != nil {
				errc <- werr
				return
			}
		}
		if err != nil {
			errc <- err
			return
		}
	}
}

func (dc *ssmDataChannel) sendData(p []byte) error {
	flags := uint64(0)
	if dc.seq == 0 {
		flags = flagSyn
	}
	m := &agentMessage{
		MessageType:    smInputStreamData,
		SchemaVersion:  1,
		CreatedDate:    nowMillis(),
		SequenceNumber: dc.seq,
		Flags:          flags,
		MessageID:      newUUID(),
		PayloadType:    smPayloadTypeOutput,
		Payload:        append([]byte(nil), p...),
	}
	dc.seq++
	return dc.writeAgentMessage(m)
}

type acknowledgeContent struct {
	AcknowledgedMessageType           string `json:"AcknowledgedMessageType"`
	AcknowledgedMessageId             string `json:"AcknowledgedMessageId"`
	AcknowledgedMessageSequenceNumber int64  `json:"AcknowledgedMessageSequenceNumber"`
	IsSequentialMessage               bool   `json:"IsSequentialMessage"`
}

func (dc *ssmDataChannel) sendAcknowledge(in *agentMessage) {
	content := acknowledgeContent{
		AcknowledgedMessageType:           in.MessageType,
		AcknowledgedMessageId:             uuidString(in.MessageID),
		AcknowledgedMessageSequenceNumber: in.SequenceNumber,
		IsSequentialMessage:               true,
	}
	b, err := json.Marshal(content)
	if err != nil {
		log.Printf("[WARN] ssm tunnel: marshaling ack: %v", err)
		return
	}
	m := &agentMessage{
		MessageType:    smAcknowledge,
		SchemaVersion:  1,
		CreatedDate:    nowMillis(),
		SequenceNumber: in.SequenceNumber,
		MessageID:      newUUID(),
		Payload:        b,
	}
	if err := dc.writeAgentMessage(m); err != nil {
		log.Printf("[WARN] ssm tunnel: sending ack: %v", err)
	}
}

type handshakeRequestPayload struct {
	AgentVersion           string `json:"AgentVersion"`
	RequestedClientActions []struct {
		ActionType       string          `json:"ActionType"`
		ActionParameters json.RawMessage `json:"ActionParameters"`
	} `json:"RequestedClientActions"`
}

type processedClientAction struct {
	ActionType   string `json:"ActionType"`
	ActionStatus int    `json:"ActionStatus"` // 1 = Success
	ActionResult any    `json:"ActionResult,omitempty"`
	Error        string `json:"Error,omitempty"`
}

type handshakeResponsePayload struct {
	ClientVersion          string                  `json:"ClientVersion"`
	ProcessedClientActions []processedClientAction `json:"ProcessedClientActions"`
	Errors                 []string                `json:"Errors"`
}

func (dc *ssmDataChannel) sendHandshakeResponse(reqPayload []byte) {
	var req handshakeRequestPayload
	if err := json.Unmarshal(reqPayload, &req); err != nil {
		log.Printf("[WARN] ssm tunnel: parsing handshake request: %v", err)
	}

	resp := handshakeResponsePayload{
		ClientVersion:          "1.2.0.0",
		ProcessedClientActions: []processedClientAction{},
		Errors:                 []string{},
	}
	for _, a := range req.RequestedClientActions {
		resp.ProcessedClientActions = append(resp.ProcessedClientActions, processedClientAction{
			ActionType:   a.ActionType,
			ActionStatus: 1, // Success
		})
	}

	b, err := json.Marshal(resp)
	if err != nil {
		log.Printf("[WARN] ssm tunnel: marshaling handshake response: %v", err)
		return
	}
	m := &agentMessage{
		MessageType:    smInputStreamData,
		SchemaVersion:  1,
		CreatedDate:    nowMillis(),
		SequenceNumber: 0,
		MessageID:      newUUID(),
		PayloadType:    smPayloadTypeHandshakeResponse,
		Payload:        b,
	}
	if err := dc.writeAgentMessage(m); err != nil {
		log.Printf("[WARN] ssm tunnel: sending handshake response: %v", err)
	}
}

func (dc *ssmDataChannel) writeAgentMessage(m *agentMessage) error {
	dc.writeMu.Lock()
	defer dc.writeMu.Unlock()
	return dc.ws.WriteMessage(websocket.BinaryMessage, m.serialize())
}

// ---- small helpers -------------------------------------------------------

func nowMillis() uint64 {
	return uint64(time.Now().UnixMilli())
}

func newUUID() [16]byte {
	var u [16]byte
	_, _ = rand.Read(u[:])
	u[6] = (u[6] & 0x0f) | 0x40 // version 4
	u[8] = (u[8] & 0x3f) | 0x80 // variant 10
	return u
}

func uuidString(u [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}
