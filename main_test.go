package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/i9wa4/tmux-mac-voice/internal/voice"
)

type waitingASR struct{ started chan struct{} }

func (a waitingASR) Transcribe(ctx context.Context, _ []int16) (string, error) {
	select {
	case a.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return "", ctx.Err()
}

func TestStopCancelsProcessingAndReleasesSession(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	a := waitingASR{started: make(chan struct{}, 1)}
	s := &server{token: token, target: "agent", asr: a, adapter: voice.UnavailableAdapter{}}
	httpServer := httptest.NewServer(http.HandlerFunc(s.stream))
	defer httpServer.Close()
	s.origin = httpServer.URL
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http")
	dial := func() (*websocket.Conn, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
			HTTPHeader:   http.Header{"Origin": []string{s.origin}},
			Subprotocols: []string{"voice-v1", "auth." + token},
		})
		return conn, err
	}
	conn, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, first, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ready event
	if err := json.Unmarshal(first, &ready); err != nil || ready.Type != "ready" {
		t.Fatalf("first event: %s, %v", first, err)
	}
	packet := make([]byte, 4+voice.SamplesPerFrame*2)
	binary.LittleEndian.PutUint32(packet, ready.Generation)
	for i := 0; i < 55; i++ {
		for j := 0; j < voice.SamplesPerFrame; j++ {
			value := uint16(0)
			if i < 20 {
				value = 4000
			}
			binary.LittleEndian.PutUint16(packet[4+j*2:], value)
		}
		if err := conn.Write(ctx, websocket.MessageBinary, packet); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-a.started:
	case <-ctx.Done():
		t.Fatal("ASR never started")
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"stop"}`)); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		second, err := dial()
		if err == nil {
			second.CloseNow()
			return
		}
	}
	t.Fatal("Stop did not release the active session")
}

func TestStreamRejectsWrongOrigin(t *testing.T) {
	s := &server{token: "0123456789abcdef0123456789abcdef", origin: "https://voice.example"}
	r := httptest.NewRequest(http.MethodGet, "/stream", nil)
	r.Header.Set("Origin", "https://other.example")
	w := httptest.NewRecorder()
	s.stream(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

func TestStreamRejectsMissingMalformedAndDuplicateAuthBeforeUpgrade(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct{ name, protocols string }{
		{"missing", "voice-v1"},
		{"bare auth", "voice-v1, auth."},
		{"short", "voice-v1, auth.short"},
		{"incorrect", "voice-v1, auth.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"duplicate", "voice-v1, auth." + token + ", auth." + token},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &server{token: token, origin: "https://voice.example"}
			r := httptest.NewRequest(http.MethodGet, "/stream", nil)
			r.Header.Set("Origin", s.origin)
			r.Header.Set("Sec-WebSocket-Protocol", tc.protocols)
			w := httptest.NewRecorder()
			s.stream(w, r)
			if w.Code != http.StatusUnauthorized || w.Header().Get("Sec-WebSocket-Protocol") != "" || s.busy {
				t.Fatalf("rejection=%d protocol=%q busy=%v", w.Code, w.Header().Get("Sec-WebSocket-Protocol"), s.busy)
			}
		})
	}
}

func TestAcceptedProtocolAndLogsDoNotEchoToken(t *testing.T) {
	p := &testPipeline{}
	_, h, dial := testSession(t, p, 0)
	defer h.Close()
	var logs bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldWriter)
	conn, response, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if conn.Subprotocol() != "voice-v1" || response.Header.Get("Sec-WebSocket-Protocol") != "voice-v1" {
		t.Fatalf("unexpected selected protocol: %q %q", conn.Subprotocol(), response.Header.Get("Sec-WebSocket-Protocol"))
	}
	if strings.Contains(logs.String(), testToken) || strings.Contains(response.Header.Get("Sec-WebSocket-Protocol"), testToken) {
		t.Fatal("token echoed in logs or selected protocol")
	}
}
