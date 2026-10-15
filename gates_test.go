package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/i9wa4/tmux-mac-voice/internal/voice"
)

type gateASR func(context.Context, []int16) (string, error)

func (f gateASR) Transcribe(ctx context.Context, samples []int16) (string, error) {
	return f(ctx, samples)
}

type gateAdapter struct {
	mu       sync.Mutex
	calls    []voice.TurnRequest
	exchange func(context.Context, voice.TurnRequest) (voice.TurnResult, error)
}

func (a *gateAdapter) Exchange(ctx context.Context, request voice.TurnRequest) (voice.TurnResult, error) {
	a.mu.Lock()
	a.calls = append(a.calls, request)
	exchange := a.exchange
	a.mu.Unlock()
	return exchange(ctx, request)
}

func (a *gateAdapter) requests() []voice.TurnRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]voice.TurnRequest(nil), a.calls...)
}

func terminal(request voice.TurnRequest, status string) voice.TurnResult {
	return voice.TurnResult{SessionID: request.SessionID, TurnID: request.TurnID,
		RequestID: request.RequestID, ResultID: "result-" + request.RequestID,
		Status: status, Text: "返事"}
}

type gateTTS struct{}

func (gateTTS) Speak(context.Context, string) ([]byte, error) { return validTestWAV(), nil }

type gateTTSFunc func(context.Context, string) ([]byte, error)

func (f gateTTSFunc) Speak(ctx context.Context, text string) ([]byte, error) { return f(ctx, text) }

func TestStage1FailuresMakeZeroAdapterCalls(t *testing.T) {
	for _, tc := range []struct {
		name string
		asr  gateASR
	}{
		{"whitespace", func(context.Context, []int16) (string, error) { return " \t\n", nil }},
		{"empty", func(context.Context, []int16) (string, error) { return "", nil }},
		{"error", func(context.Context, []int16) (string, error) { return "こんにちは", errors.New("ASR failed") }},
		{"timeout", func(ctx context.Context, _ []int16) (string, error) { <-ctx.Done(); return "", ctx.Err() }},
		{"long text", func(context.Context, []int16) (string, error) { return strings.Repeat("あ", 1400), nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &gateAdapter{exchange: func(_ context.Context, r voice.TurnRequest) (voice.TurnResult, error) {
				return terminal(r, "done"), nil
			}}
			s := &server{asr: tc.asr, adapter: a, target: "agent", tts: gateTTS{}}
			ctx := context.Background()
			if tc.name == "timeout" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Millisecond)
				defer cancel()
			}
			_, err := s.processTurn(ctx, "session", "turn", "request", make([]int16, voice.SamplesPerFrame), func(string) error { return nil })
			if err == nil || len(a.requests()) != 0 {
				t.Fatalf("error=%v adapter calls=%d", err, len(a.requests()))
			}
		})
	}
}

func TestStage1RejectsInvalidAudioBeforeASR(t *testing.T) {
	called := false
	s := &server{asr: gateASR(func(context.Context, []int16) (string, error) { called = true; return "こんにちは", nil })}
	for _, samples := range [][]int16{nil, make([]int16, voice.MaxUtteranceSecs*voice.SampleRate+1)} {
		if _, err := s.finalTranscript(context.Background(), samples); err == nil {
			t.Fatal("invalid audio accepted")
		}
	}
	if called {
		t.Fatal("ASR received invalid audio")
	}
}

func TestStage2SubmitsFixedTextOnceAndRequiresTerminalResult(t *testing.T) {
	a := &gateAdapter{exchange: func(_ context.Context, r voice.TurnRequest) (voice.TurnResult, error) {
		return terminal(r, "done"), nil
	}}
	s := &server{adapter: a}
	r := voice.TurnRequest{SessionID: "s", TurnID: "t", RequestID: "r", Target: "agent", Text: "こんにちは", Deadline: time.Now().Add(time.Second)}
	got, err := s.submitFinalText(context.Background(), r)
	if err != nil || got.Status != "done" || len(a.requests()) != 1 || a.requests()[0] != r {
		t.Fatalf("result=%+v error=%v calls=%+v", got, err, a.requests())
	}
	r.Text = " こんにちは"
	if _, err := s.submitFinalText(context.Background(), r); err == nil || len(a.requests()) != 1 {
		t.Fatal("unfixed text reached adapter")
	}
	r.Text = "こんにちは"
	r.Deadline = time.Now().Add(-time.Second)
	if _, err := s.submitFinalText(context.Background(), r); err == nil || len(a.requests()) != 1 {
		t.Fatal("expired request reached adapter")
	}
	a.exchange = func(_ context.Context, r voice.TurnRequest) (voice.TurnResult, error) {
		return terminal(r, "pending"), nil
	}
	r.Deadline = time.Now().Add(time.Second)
	if _, err := s.submitFinalText(context.Background(), r); err == nil {
		t.Fatal("pending receipt treated as terminal")
	}
	a.exchange = func(_ context.Context, r voice.TurnRequest) (voice.TurnResult, error) {
		return terminal(r, "blocked"), nil
	}
	if got, err := s.submitFinalText(context.Background(), r); err != nil || got.Status != "blocked" {
		t.Fatalf("blocked result=%+v error=%v", got, err)
	}
}

func TestStage2DeadlineStopsWaitingFake(t *testing.T) {
	entered := make(chan struct{}, 1)
	a := &gateAdapter{exchange: func(ctx context.Context, _ voice.TurnRequest) (voice.TurnResult, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return voice.TurnResult{}, ctx.Err()
	}}
	s := &server{adapter: a}
	r := voice.TurnRequest{SessionID: "s", TurnID: "t", RequestID: "r", Target: "agent", Text: "こんにちは", Deadline: time.Now().Add(50 * time.Millisecond)}
	if _, err := s.submitFinalText(context.Background(), r); !errors.Is(err, context.DeadlineExceeded) || len(a.requests()) != 1 {
		t.Fatalf("timeout error=%v calls=%d", err, len(a.requests()))
	}
	select {
	case <-entered:
	default:
		t.Fatal("adapter was never called")
	}
}

func TestJoinedFakeMakesOneRequestPerFinalTurn(t *testing.T) {
	texts := []string{" こんにちは ", "天気を教えて"}
	a := &gateAdapter{exchange: func(_ context.Context, r voice.TurnRequest) (voice.TurnResult, error) {
		return terminal(r, "done"), nil
	}}
	s := &server{target: "agent", asr: gateASR(func(context.Context, []int16) (string, error) {
		text := texts[0]
		texts = texts[1:]
		return text, nil
	}), adapter: a, tts: gateTTS{}}
	for i, want := range []string{"こんにちは", "天気を教えて"} {
		requestID := "request-" + string(rune('1'+i))
		var transcript string
		result, err := s.processTurn(context.Background(), "session", "turn-"+requestID, requestID,
			make([]int16, voice.SamplesPerFrame), func(text string) error { transcript = text; return nil })
		if err != nil || transcript != want || result.Status != "done" || result.RequestID != requestID {
			t.Fatalf("turn %d: result=%+v transcript=%q error=%v", i, result, transcript, err)
		}
	}
	if len(a.requests()) != 2 || a.requests()[0].Text != "こんにちは" || a.requests()[1].Text != "天気を教えて" || a.requests()[0].RequestID == a.requests()[1].RequestID {
		t.Fatalf("unexpected joined requests: %+v", a.requests())
	}
}

func TestStage2RejectsMismatchedCorrelationAndMalformedTerminalStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*voice.TurnResult)
	}{
		{"session", func(r *voice.TurnResult) { r.SessionID = "other" }},
		{"turn", func(r *voice.TurnResult) { r.TurnID = "other" }},
		{"request", func(r *voice.TurnResult) { r.RequestID = "other" }},
		{"result", func(r *voice.TurnResult) { r.ResultID = "" }},
		{"status", func(r *voice.TurnResult) { r.Status = "pending" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var spoken bool
			a := &gateAdapter{exchange: func(_ context.Context, req voice.TurnRequest) (voice.TurnResult, error) {
				r := terminal(req, "done")
				tc.mutate(&r)
				return r, nil
			}}
			s := &server{asr: gateASR(func(context.Context, []int16) (string, error) { return "text", nil }), adapter: a, target: "agent", tts: gateTTSFunc(func(context.Context, string) ([]byte, error) { spoken = true; return validTestWAV(), nil })}
			_, err := s.processTurn(context.Background(), "session", "turn", "request", make([]int16, voice.SamplesPerFrame), func(string) error { return nil })
			if err == nil || spoken || len(a.requests()) != 1 {
				t.Fatalf("bad adapter result advanced pipeline: err=%v calls=%d", err, len(a.requests()))
			}
		})
	}
}

func TestNoncooperativeAdapterDeadlineBlocksOverlapAndDropsLateResult(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	a := &gateAdapter{exchange: func(_ context.Context, req voice.TurnRequest) (voice.TurnResult, error) {
		entered <- struct{}{}
		<-release
		return terminal(req, "done"), nil
	}}
	s := &server{adapter: a}
	r := voice.TurnRequest{SessionID: "s", TurnID: "t", RequestID: "r", Target: "agent", Text: "text", Deadline: time.Now().Add(50 * time.Millisecond)}
	started := time.Now()
	_, err := s.submitFinalText(context.Background(), r)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("unbounded adapter wait: %v", err)
	}
	select {
	case <-entered:
	default:
		t.Fatal("adapter was never called")
	}
	r.Deadline = time.Now().Add(time.Second)
	if _, err := s.submitFinalText(context.Background(), r); !errors.Is(err, errAdapterBusy) {
		t.Fatalf("overlap was admitted: %v", err)
	}
	close(release)
	select {
	case <-s.adapterWork:
	case <-time.After(time.Second):
		t.Fatal("backend did not exit")
	}
	if len(a.requests()) != 1 {
		t.Fatalf("late result caused another call: %d", len(a.requests()))
	}
}
