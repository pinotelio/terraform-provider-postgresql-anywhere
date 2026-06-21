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
	"github.com/xtaci/smux"
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

// SSMTunnel is a running tunnel: a local TCP listener that multiplexes every
// connection over a single shared SSM port-forwarding session using smux. The
// session is established on first use and re-established only when it breaks, so
// rapid open/close churn and concurrent connections no longer each spin up (and
// corrupt) their own SSM session.
type SSMTunnel struct {
	cfg        SSMTunnelConfig
	instanceID string
	ssmClient  *ssm.Client
	listener   net.Listener
	localHost  string
	localPort  int

	sessMu    sync.Mutex
	session   *smux.Session   // shared; nil until first use / after a failure
	dc        *ssmDataChannel // data channel backing session
	sessionID string

	closeOnce sync.Once
	closed    chan struct{}
}

// LocalHost returns the loopback host the provider should connect to.
func (t *SSMTunnel) LocalHost() string { return t.localHost }

// LocalPort returns the local port the tunnel listens on.
func (t *SSMTunnel) LocalPort() int { return t.localPort }

// Close shuts the listener down and tears down the shared SSM session.
func (t *SSMTunnel) Close() error {
	var err error
	t.closeOnce.Do(func() {
		close(t.closed)
		t.sessMu.Lock()
		t.teardownLocked()
		t.sessMu.Unlock()
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

// handleConn bridges one local connection to a fresh smux stream on the shared
// SSM session, opening (or re-opening) that session as needed.
func (t *SSMTunnel) handleConn(local net.Conn) {
	defer func() { _ = local.Close() }()

	stream, err := t.openStream()
	if err != nil {
		log.Printf("[WARN] ssm tunnel: could not open stream: %v", err)
		return
	}
	defer func() { _ = stream.Close() }()

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(stream, local); done <- struct{}{} }()
	go func() { _, _ = io.Copy(local, stream); done <- struct{}{} }()
	select {
	case <-done:
	case <-t.closed:
	}
}

// openStream returns a new multiplexed stream on the shared SSM session. It
// establishes the session on first use and re-establishes it if it has died, so
// callers always get a working stream or an error. This is the "reuse one
// session, reconnect only when broken" model.
func (t *SSMTunnel) openStream() (*smux.Stream, error) {
	t.sessMu.Lock()
	defer t.sessMu.Unlock()

	if t.session == nil || t.session.IsClosed() {
		if err := t.establishLocked(); err != nil {
			return nil, err
		}
	}
	stream, err := t.session.OpenStream()
	if err != nil {
		// The session broke between the health check and OpenStream; rebuild once.
		log.Printf("[INFO] ssm tunnel: session unusable (%v), re-establishing", err)
		if err := t.establishLocked(); err != nil {
			return nil, err
		}
		return t.session.OpenStream()
	}
	return stream, nil
}

// establishLocked starts a fresh SSM session, performs the data-channel
// handshake, and layers an smux client over it. The caller holds sessMu.
func (t *SSMTunnel) establishLocked() error {
	t.teardownLocked()

	ctx := context.Background()
	out, err := t.ssmClient.StartSession(ctx, &ssm.StartSessionInput{
		Target:       aws.String(t.instanceID),
		DocumentName: aws.String("AWS-StartPortForwardingSessionToRemoteHost"),
		Parameters: map[string][]string{
			"host":       {t.cfg.RemoteHost},
			"portNumber": {strconv.Itoa(t.cfg.RemotePort)},
		},
	})
	if err != nil {
		return fmt.Errorf("StartSession: %w", err)
	}
	sessionID := aws.ToString(out.SessionId)

	ws, _, err := websocket.DefaultDialer.DialContext(ctx, aws.ToString(out.StreamUrl), nil)
	if err != nil {
		t.terminate(sessionID)
		return fmt.Errorf("websocket dial: %w", err)
	}

	pr, pw := io.Pipe()
	dc := &ssmDataChannel{
		ws:            ws,
		token:         aws.ToString(out.TokenValue),
		handshakeDone: make(chan struct{}),
		pr:            pr,
		pw:            pw,
		dead:          make(chan struct{}),
	}
	if err := dc.open(); err != nil {
		_ = dc.close()
		t.terminate(sessionID)
		return fmt.Errorf("open data channel: %w", err)
	}
	if err := dc.start(); err != nil {
		_ = dc.close()
		t.terminate(sessionID)
		return fmt.Errorf("handshake: %w", err)
	}

	session, err := smux.Client(&dataChannelRWC{dc: dc}, smux.DefaultConfig())
	if err != nil {
		_ = dc.close()
		t.terminate(sessionID)
		return fmt.Errorf("smux client: %w", err)
	}

	t.session = session
	t.dc = dc
	t.sessionID = sessionID
	log.Printf("[INFO] ssm tunnel: established session %s (multiplexing connections)", sessionID)
	return nil
}

// teardownLocked closes the current smux session, data channel, and SSM session.
// The caller holds sessMu.
func (t *SSMTunnel) teardownLocked() {
	if t.session != nil {
		_ = t.session.Close()
		t.session = nil
	}
	if t.dc != nil {
		_ = t.dc.close()
		t.dc = nil
	}
	if t.sessionID != "" {
		t.terminate(t.sessionID)
		t.sessionID = ""
	}
}

func (t *SSMTunnel) terminate(sessionID string) {
	tctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, e := t.ssmClient.TerminateSession(tctx, &ssm.TerminateSessionInput{
		SessionId: aws.String(sessionID),
	}); e != nil {
		log.Printf("[DEBUG] ssm tunnel: TerminateSession %s: %v", sessionID, e)
	}
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
	// On the wire the MessageId is stored as [least-significant 8 bytes][most-
	// significant 8 bytes], swapped relative to the standard UUID byte order we
	// keep in memory. The agent matches ACKs by this id, so the halves must be
	// swapped or it never sees our acknowledgements and retransmits forever.
	copy(buf[smMessageIDOffset:smMessageIDOffset+8], m.MessageID[8:16])
	copy(buf[smMessageIDOffset+8:smMessageIDOffset+16], m.MessageID[0:8])

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
	// Undo the on-wire MessageId half-swap (see serialize) so MessageID holds
	// the standard UUID byte order.
	copy(m.MessageID[0:8], buf[smMessageIDOffset+8:smMessageIDOffset+16])
	copy(m.MessageID[8:16], buf[smMessageIDOffset:smMessageIDOffset+8])

	payloadLen := int(binary.BigEndian.Uint32(buf[smPayloadLengthOffset:]))
	end := smPayloadOffset + payloadLen
	if payloadLen < 0 || end > len(buf) {
		return nil, fmt.Errorf("invalid payload length %d (buffer %d)", payloadLen, len(buf))
	}
	m.Payload = append([]byte(nil), buf[smPayloadOffset:end]...)
	return m, nil
}

// ssmDataChannel drives one WebSocket session: open -> handshake, then carries
// smux frames for the shared session that multiplexes every connection.
type ssmDataChannel struct {
	ws    *websocket.Conn
	token string

	writeMu sync.Mutex
	seq     int64 // next outbound (client->agent) input_stream_data sequence

	expectedSeq int64 // next expected inbound (agent->client) output_stream_data sequence

	handshakeDone chan struct{}
	handshakeOnce sync.Once

	// Agent output payloads (smux frames) are written to pw by readLoop and read
	// back by the smux session through pr.
	pr *io.PipeReader
	pw *io.PipeWriter

	dead      chan struct{} // closed when readLoop exits
	deadErr   error         // why readLoop exited (read after dead is closed)
	closeOnce sync.Once
}

// dataChannelRWC adapts the SSM data channel to an io.ReadWriteCloser so an smux
// client session can run over it. The agent serves the port-forwarding data
// plane as an smux stream multiplexer, so reads return agent output payloads and
// writes are sent as input_stream_data.
type dataChannelRWC struct{ dc *ssmDataChannel }

func (c *dataChannelRWC) Read(p []byte) (int, error) { return c.dc.pr.Read(p) }

func (c *dataChannelRWC) Write(p []byte) (int, error) {
	if err := c.dc.sendData(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *dataChannelRWC) Close() error { return c.dc.close() }

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

// start launches the read loop and blocks until the SSM handshake completes,
// the channel dies, or the handshake times out. After it returns, the read loop
// keeps running and feeds smux frames to the shared session.
func (dc *ssmDataChannel) start() error {
	go dc.readLoop()
	select {
	case <-dc.handshakeDone:
		return nil
	case <-dc.dead:
		if dc.deadErr != nil {
			return dc.deadErr
		}
		return fmt.Errorf("data channel closed before handshake")
	case <-time.After(30 * time.Second):
		return fmt.Errorf("handshake timed out")
	}
}

// close shuts the data channel down, unblocking both the smux reader and the
// read loop.
func (dc *ssmDataChannel) close() error {
	dc.closeOnce.Do(func() {
		_ = dc.pw.Close()
		_ = dc.ws.Close()
	})
	return nil
}

func (dc *ssmDataChannel) readLoop() {
	var exitErr error
	defer func() {
		dc.deadErr = exitErr
		_ = dc.pw.CloseWithError(exitErr) // unblock the smux reader
		close(dc.dead)
	}()

	for {
		_, data, err := dc.ws.ReadMessage()
		if err != nil {
			exitErr = err
			return
		}
		msg, err := deserializeAgentMessage(data)
		if err != nil {
			log.Printf("[WARN] ssm tunnel: dropping malformed message: %v", err)
			continue
		}

		switch msg.MessageType {
		case smOutputStreamData:
			// Always acknowledge so the agent stops retransmitting this message.
			dc.sendAcknowledge(msg)
			// Deduplicate by sequence number. Under load the agent retransmits
			// messages whose acks were delayed; processing a retransmit twice would
			// feed duplicate bytes into smux and corrupt the multiplexed streams
			// (manifesting as TLS errors on the database connections). Process each
			// sequence exactly once, in order.
			if msg.SequenceNumber != dc.expectedSeq {
				continue
			}
			dc.expectedSeq++
			switch msg.PayloadType {
			case smPayloadTypeHandshakeRequest:
				dc.sendHandshakeResponse(msg.Payload)
			case smPayloadTypeHandshakeComplete:
				dc.handshakeOnce.Do(func() { close(dc.handshakeDone) })
			case smPayloadTypeOutput:
				if _, werr := dc.pw.Write(msg.Payload); werr != nil {
					exitErr = werr
					return
				}
			case smPayloadTypeError:
				log.Printf("[ERROR] ssm tunnel: agent reported error: %s", string(msg.Payload))
			}
		case smAcknowledge:
			// Best-effort: we do not retransmit, so acks are informational.
		case smChannelClosed:
			log.Printf("[INFO] ssm tunnel: channel closed by agent")
			exitErr = io.EOF
			return
		case smStartPublication, smPausePublication:
			// Flow-control hints; ignored in this best-effort implementation.
		}
	}
}

// nextInput builds the next input_stream_data message on the shared client
// sequence, with the SYN flag on the first message (sequence 0). The handshake
// response and all subsequent smux data go through this one sequence: the agent
// advances its expected sequence as it processes the handshake response, so the
// smux frames that follow must be contiguous (sequence 1, 2, ...). Reusing
// sequence 0 makes the agent treat the first frame as a duplicate and drop it.
func (dc *ssmDataChannel) nextInput(payloadType uint32, p []byte) *agentMessage {
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
		PayloadType:    payloadType,
		Payload:        append([]byte(nil), p...),
	}
	dc.seq++
	return m
}

// sendInput serializes and sends one input_stream_data message under writeMu so
// the sequence counter and the websocket write stay consistent.
func (dc *ssmDataChannel) sendInput(payloadType uint32, p []byte) error {
	dc.writeMu.Lock()
	defer dc.writeMu.Unlock()
	return dc.ws.WriteMessage(websocket.BinaryMessage, dc.nextInput(payloadType, p).serialize())
}

func (dc *ssmDataChannel) sendData(p []byte) error {
	return dc.sendInput(smPayloadTypeOutput, p)
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
	// The handshake response is the first client message: sequence 0 with the SYN
	// flag. It shares the data stream sequence so the smux frames that follow are
	// contiguous (sequence 1, 2, ...).
	if err := dc.sendInput(smPayloadTypeHandshakeResponse, b); err != nil {
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
