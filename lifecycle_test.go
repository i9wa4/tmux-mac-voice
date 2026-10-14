package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/i9wa4/tmux-mac-voice/internal/voice"
)

const testToken = "0123456789abcdef0123456789abcdef"

type testPipeline struct {
	block        string
	entered      chan struct{}
	release      chan struct{}
	asrCalls     atomic.Int32
	adapterCalls atomic.Int32
	ttsCalls     atomic.Int32
}

func (p *testPipeline) wait(stage string) {
	if p.block == stage {
		p.entered <- struct{}{}
		<-p.release // Deliberately ignore cancellation to test session admission.
	}
}

func (p *testPipeline) Transcribe(context.Context, []int16) (string, error) {
	p.asrCalls.Add(1)
	p.wait("asr")
	return "こんにちは", nil
}

func (p *testPipeline) Exchange(_ context.Context, request voice.TurnRequest) (voice.TurnResult, error) {
	p.adapterCalls.Add(1)
	p.wait("adapter")
	return voice.TurnResult{SessionID: request.SessionID, TurnID: request.TurnID,
		RequestID: request.RequestID, ResultID: "result", Status: "done", Text: "返事"}, nil
}

func (p *testPipeline) Speak(context.Context, string) ([]byte, error) {
	p.ttsCalls.Add(1)
	p.wait("tts")
	return validTestWAV(), nil
}

func TestAdapterDeadlineClosesSessionAndKeepsAdmissionBusy(t *testing.T) {
	p := &testPipeline{block: "adapter", entered: make(chan struct{}, 1), release: make(chan struct{})}
	s, h, dial := testSession(t, p, 0)
	s.adapterTimeout = 30 * time.Millisecond
	defer h.Close()
	conn, _, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	ready := nextEvent(t, conn)
	sendTestUtterance(t, conn, ready.Generation)
	select {
	case <-p.entered:
	case <-time.After(time.Second):
		t.Fatal("adapter did not start")
	}
	started := time.Now()
	for {
		e := nextEvent(t, conn)
		if e.Type == "error" {
			if !strings.Contains(e.Error, "deadline") {
				t.Fatalf("unexpected error: %#v", e)
			}
			break
		}
	}
	if time.Since(started) > time.Second {
		t.Fatal("noncooperative adapter held the turn")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("timed-out session remained open")
	}
	second, response, err := dial()
	if second != nil {
		second.CloseNow()
		t.Fatal("overlapping session admitted")
	}
	if err == nil || response == nil || response.StatusCode != http.StatusConflict {
		t.Fatalf("want busy 409, got %v %#v", err, response)
	}
	close(p.release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		second, _, err = dial()
		if err == nil {
			second.CloseNow()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("admission did not recover after adapter exit: %v", err)
	}
	if p.adapterCalls.Load() != 1 || p.ttsCalls.Load() != 0 {
		t.Fatalf("late result advanced pipeline: adapter=%d tts=%d", p.adapterCalls.Load(), p.ttsCalls.Load())
	}
}

func TestSilenceAndStaleGenerationNeverReachBackends(t *testing.T) {
	p := &testPipeline{}
	_, h, dial := testSession(t, p, 0)
	defer h.Close()
	conn, _, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	ready := nextEvent(t, conn)
	packet := make([]byte, 4+voice.SamplesPerFrame*2)
	binary.LittleEndian.PutUint32(packet, ready.Generation)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := 0; i < 100; i++ {
		if err := conn.Write(ctx, websocket.MessageBinary, packet); err != nil {
			t.Fatal(err)
		}
	}
	sendTestUtterance(t, conn, ready.Generation-1)
	time.Sleep(50 * time.Millisecond)
	if p.asrCalls.Load() != 0 || p.adapterCalls.Load() != 0 || p.ttsCalls.Load() != 0 {
		t.Fatalf("silent or stale frames reached backends: %d %d %d", p.asrCalls.Load(), p.adapterCalls.Load(), p.ttsCalls.Load())
	}
	sendTestUtterance(t, conn, ready.Generation)
	for {
		e := nextEvent(t, conn)
		if e.Type == "reply" {
			break
		}
		if e.Type == "error" {
			t.Fatalf("valid turn failed: %#v", e)
		}
	}
	if p.asrCalls.Load() != 1 || p.adapterCalls.Load() != 1 {
		t.Fatalf("valid turn did not run once: %d %d", p.asrCalls.Load(), p.adapterCalls.Load())
	}
}

func testSession(t *testing.T, p *testPipeline, playbackTimeout time.Duration) (*server, *httptest.Server, func() (*websocket.Conn, *http.Response, error)) {
	t.Helper()
	s := &server{token: testToken, target: "agent", asr: p, adapter: p, tts: p,
		playbackTimeout: playbackTimeout, shutdownJoinTimeout: 20 * time.Millisecond}
	h := httptest.NewServer(http.HandlerFunc(s.stream))
	s.origin = h.URL
	dial := func() (*websocket.Conn, *http.Response, error) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.URL, "http"), &websocket.DialOptions{
			HTTPHeader:   http.Header{"Origin": []string{s.origin}},
			Subprotocols: []string{"voice-v1", "auth." + testToken},
		})
	}
	return s, h, dial
}

func nextEvent(t *testing.T, conn *websocket.Conn) event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var e event
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func sendTestUtterance(t *testing.T, conn *websocket.Conn, generation uint32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	packet := make([]byte, 4+voice.SamplesPerFrame*2)
	binary.LittleEndian.PutUint32(packet, generation)
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
}

func sendControl(t *testing.T, conn *websocket.Conn, control any) {
	t.Helper()
	b, err := json.Marshal(control)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func waitForReply(t *testing.T, conn *websocket.Conn) event {
	t.Helper()
	ready := nextEvent(t, conn)
	if ready.Type != "ready" {
		t.Fatalf("first event: %#v", ready)
	}
	sendTestUtterance(t, conn, ready.Generation)
	for {
		e := nextEvent(t, conn)
		if e.Type == "reply" {
			return e
		}
		if e.Type == "error" {
			t.Fatalf("processing error: %#v", e)
		}
	}
}

func TestStopKeepsSessionBusyUntilEachBackendExits(t *testing.T) {
	for _, stage := range []string{"asr", "adapter", "tts"} {
		t.Run(stage, func(t *testing.T) {
			p := &testPipeline{block: stage, entered: make(chan struct{}, 1), release: make(chan struct{})}
			_, h, dial := testSession(t, p, 0)
			defer h.Close()
			conn, _, err := dial()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			ready := nextEvent(t, conn)
			sendTestUtterance(t, conn, ready.Generation)
			select {
			case <-p.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("backend did not start")
			}
			sendControl(t, conn, map[string]string{"type": "stop"})
			time.Sleep(80 * time.Millisecond) // Past the bounded join, while backend is still blocked.
			second, response, err := dial()
			if second != nil {
				second.CloseNow()
				t.Fatal("new session admitted before old backend exited")
			}
			if err == nil || response == nil || response.StatusCode != http.StatusConflict {
				t.Fatalf("expected 409 while backend runs: %v, %#v", err, response)
			}
			close(p.release)
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				second, _, err = dial()
				if err == nil {
					second.CloseNow()
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatal("session remained busy after backend exit")
		})
	}
}

func TestPlaybackDeadlineRejectsStaleAckAndAllowsReconnect(t *testing.T) {
	p := &testPipeline{}
	_, h, dial := testSession(t, p, 80*time.Millisecond)
	defer h.Close()
	conn, _, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	reply := waitForReply(t, conn)
	sendControl(t, conn, map[string]any{"type": "played", "session_id": reply.SessionID,
		"turn_id": reply.TurnID, "request_id": reply.RequestID, "result_id": "stale", "generation": reply.Generation})
	e := nextEvent(t, conn)
	if e.Type != "error" || !strings.Contains(e.Error, "timed out") {
		t.Fatalf("want playback timeout, got %#v", e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("timed-out connection stayed open")
	}
	second, _, err := dial()
	if err != nil {
		t.Fatalf("reconnect after timeout: %v", err)
	}
	second.CloseNow()
}

func TestPlaybackFailureAndAbortReleaseSession(t *testing.T) {
	for _, kind := range []string{"playback_failed", "abort"} {
		t.Run(kind, func(t *testing.T) {
			p := &testPipeline{}
			_, h, dial := testSession(t, p, time.Second)
			defer h.Close()
			conn, _, err := dial()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			reply := waitForReply(t, conn)
			sendControl(t, conn, map[string]any{"type": kind, "session_id": reply.SessionID,
				"turn_id": reply.TurnID, "request_id": reply.RequestID,
				"result_id": reply.ResultID, "generation": reply.Generation})
			if e := nextEvent(t, conn); e.Type != "error" {
				t.Fatalf("want error, got %#v", e)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, _, err := conn.Read(ctx); err == nil {
				t.Fatal("failed playback did not close session")
			}
			second, _, err := dial()
			if err != nil {
				t.Fatalf("reconnect after failure: %v", err)
			}
			second.CloseNow()
		})
	}
}
