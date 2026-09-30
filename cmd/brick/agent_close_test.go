package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// wsConn.closeGracefully must send an actual WebSocket close control frame
// (not just drop the TCP connection) — that's what turns the server's log
// from an alarming "close 1006 (abnormal closure): unexpected EOF" into a
// clean, expected disconnect on an ordinary Ctrl-C.
func TestWSConnCloseGracefullySendsCloseFrame(t *testing.T) {
	upgrader := websocket.Upgrader{}
	closeCodeCh := make(chan int, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("server upgrade: %v", err)
			return
		}
		defer conn.Close()
		conn.SetCloseHandler(func(code int, text string) error {
			closeCodeCh <- code
			return nil
		})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	client, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer client.Close()

	conn := newWSConn(client)
	conn.closeGracefully()

	select {
	case code := <-closeCodeCh:
		if code != websocket.CloseNormalClosure {
			t.Errorf("server received close code %d, want %d (CloseNormalClosure)", code, websocket.CloseNormalClosure)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server never received a close frame — closeGracefully must not just drop the connection")
	}
}
