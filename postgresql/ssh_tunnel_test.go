package postgresql

import (
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"testing"

	"golang.org/x/crypto/ssh"
)

// TestSSHTunnelEndToEnd proves the SSH bastion transport carries real PostgreSQL
// traffic: it stands up an in-process SSH server that forwards direct-tcpip
// channels, opens the tunnel to the test database through it, and runs a query.
func TestSSHTunnelEndToEnd(t *testing.T) {
	pgHost := os.Getenv("PGHOST")
	if pgHost == "" {
		t.Skip("PGHOST not set; this test needs the local test database")
	}
	pgPort := 5432
	if v := os.Getenv("PGPORT"); v != "" {
		pgPort, _ = strconv.Atoi(v)
	}

	bastionAddr, hostKey, stop := startTestSSHServer(t)
	defer stop()
	bastionHost, bastionPortStr, _ := net.SplitHostPort(bastionAddr)
	bastionPort, _ := strconv.Atoi(bastionPortStr)

	cfg := &SSHTunnelConfig{
		Host:       bastionHost,
		Port:       bastionPort,
		User:       "tester",
		Password:   "secret",
		HostKey:    hostKey,
		RemoteHost: pgHost,
		RemotePort: pgPort,
	}
	localHost, localPort, err := cfg.EnsureUp()
	if err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	defer cfg.Close()

	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		localHost, localPort, os.Getenv("PGUSER"), os.Getenv("PGPASSWORD"), envOrPostgres("PGDATABASE"))
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	var one int
	if err := db.QueryRow("SELECT 1").Scan(&one); err != nil {
		t.Fatalf("query through SSH tunnel: %v", err)
	}
	if one != 1 {
		t.Fatalf("SELECT 1 returned %d", one)
	}
}

func envOrPostgres(key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return "postgres"
}

// startTestSSHServer starts an in-process SSH server that forwards direct-tcpip
// channels to their requested destination. It returns the listen address, the
// server's host public key in authorized_keys format, and a stop func.
func startTestSSHServer(t *testing.T) (addr, hostKeyAuthorized string, stop func()) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == "tester" && string(pass) == "secret" {
				return nil, nil
			}
			return nil, fmt.Errorf("authentication denied")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveTestSSHConn(conn, cfg)
		}
	}()

	return ln.Addr().String(), string(ssh.MarshalAuthorizedKey(signer.PublicKey())), func() { _ = ln.Close() }
}

func serveTestSSHConn(c net.Conn, cfg *ssh.ServerConfig) {
	sconn, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)

	for nc := range chans {
		if nc.ChannelType() != "direct-tcpip" {
			_ = nc.Reject(ssh.UnknownChannelType, "only direct-tcpip is supported")
			continue
		}
		go serveDirectTCPIP(nc)
	}
}

func serveDirectTCPIP(nc ssh.NewChannel) {
	var payload struct {
		DestHost string
		DestPort uint32
		SrcHost  string
		SrcPort  uint32
	}
	if err := ssh.Unmarshal(nc.ExtraData(), &payload); err != nil {
		_ = nc.Reject(ssh.Prohibited, "bad direct-tcpip payload")
		return
	}
	remote, err := net.Dial("tcp", net.JoinHostPort(payload.DestHost, strconv.Itoa(int(payload.DestPort))))
	if err != nil {
		_ = nc.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		_ = remote.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	go func() { _, _ = io.Copy(ch, remote); _ = ch.Close() }()
	go func() { _, _ = io.Copy(remote, ch); _ = remote.Close() }()
}
