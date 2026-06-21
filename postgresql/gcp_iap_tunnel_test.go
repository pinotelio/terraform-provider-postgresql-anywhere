package postgresql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"golang.org/x/oauth2"
)

func TestIAPFrameBuilders(t *testing.T) {
	if got, want := iapDataFrame([]byte("hi")), []byte{0x00, 0x04, 0x00, 0x00, 0x00, 0x02, 'h', 'i'}; !bytes.Equal(got, want) {
		t.Fatalf("iapDataFrame = % x, want % x", got, want)
	}
	if got, want := iapAckFrame(258), []byte{0x00, 0x07, 0, 0, 0, 0, 0, 0, 0x01, 0x02}; !bytes.Equal(got, want) {
		t.Fatalf("iapAckFrame = % x, want % x", got, want)
	}
}

// TestGCPIAPTunnelEndToEnd proves the IAP relay and binary framing carry real
// PostgreSQL traffic: a fake IAP relay (speaking IAP framing) forwards to the
// test database, and the tunnel runs a query through it.
func TestGCPIAPTunnelEndToEnd(t *testing.T) {
	pgHost := os.Getenv("PGHOST")
	if pgHost == "" {
		t.Skip("PGHOST not set; this test needs the local test database")
	}
	pgPort := 5432
	if v := os.Getenv("PGPORT"); v != "" {
		pgPort, _ = strconv.Atoi(v)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fakeIAPRelay(w, r, pgHost, pgPort)
	}))
	defer srv.Close()

	origDial := iapDialWebsocket
	origToken := iapTokenSource
	iapDialWebsocket = func(ctx context.Context, _ string, _ http.Header) (*websocket.Conn, error) {
		wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
		ws, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
		return ws, err
	}
	iapTokenSource = func(_ context.Context) (oauth2.TokenSource, error) {
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test"}), nil
	}
	defer func() { iapDialWebsocket = origDial; iapTokenSource = origToken }()

	cfg := &GCPIAPTunnelConfig{Instance: "i", Zone: "z", Project: "p", InstancePort: pgPort}
	localHost, localPort, err := cfg.EnsureUp()
	if err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}
	defer func() { _ = cfg.Close() }()

	dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		localHost, localPort, os.Getenv("PGUSER"), os.Getenv("PGPASSWORD"), envOrPostgres("PGDATABASE"))
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	var one int
	if err := db.QueryRow("SELECT 1").Scan(&one); err != nil {
		t.Fatalf("query through IAP tunnel: %v", err)
	}
	if one != 1 {
		t.Fatalf("SELECT 1 returned %d", one)
	}
}

var iapTestUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// fakeIAPRelay speaks the server side of IAP framing and forwards to dest.
func fakeIAPRelay(w http.ResponseWriter, r *http.Request, destHost string, destPort int) {
	ws, err := iapTestUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = ws.Close() }()

	// CONNECT_SUCCESS_SID
	sid := []byte("sid")
	sidFrame := make([]byte, 6+len(sid))
	binary.BigEndian.PutUint16(sidFrame[0:2], iapTagConnectSID)
	binary.BigEndian.PutUint32(sidFrame[2:6], uint32(len(sid)))
	copy(sidFrame[6:], sid)
	if err := ws.WriteMessage(websocket.BinaryMessage, sidFrame); err != nil {
		return
	}

	dest, err := net.Dial("tcp", net.JoinHostPort(destHost, strconv.Itoa(destPort)))
	if err != nil {
		return
	}
	defer func() { _ = dest.Close() }()

	// dest -> client (DATA frames)
	go func() {
		buf := make([]byte, iapMaxDataFrame)
		for {
			n, err := dest.Read(buf)
			if n > 0 {
				if werr := ws.WriteMessage(websocket.BinaryMessage, iapDataFrame(buf[:n])); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// client -> dest (parse DATA frames)
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			return
		}
		for len(msg) >= 2 {
			switch binary.BigEndian.Uint16(msg[0:2]) {
			case iapTagData:
				if len(msg) < 6 {
					return
				}
				n := binary.BigEndian.Uint32(msg[2:6])
				if uint32(len(msg)) < 6+n {
					return
				}
				if _, err := dest.Write(msg[6 : 6+n]); err != nil {
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
}
