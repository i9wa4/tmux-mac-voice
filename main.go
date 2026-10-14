package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/i9wa4/tmux-mac-voice/internal/voice"
)

//go:embed web/*
var assets embed.FS

type transcriber interface {
	Transcribe(context.Context, []int16) (string, error)
}

type synthesizer interface {
	Speak(context.Context, string) ([]byte, error)
}

type server struct {
	token               string
	origin              string
	target              string
	asr                 transcriber
	adapter             voice.TextAdapter
	tts                 synthesizer
	mu                  sync.Mutex
	busy                bool
	adapterWork         <-chan struct{}
	adapterTimeout      time.Duration
	playbackTimeout     time.Duration
	shutdownJoinTimeout time.Duration
}

const defaultPlaybackTimeout = 2 * time.Minute

var errAdapterUnfinished = errors.New("adapter deadline reached while backend work may continue")
var errAdapterBusy = errors.New("previous adapter work is still running")

type event struct {
	Type       string `json:"type"`
	SessionID  string `json:"session_id,omitempty"`
	TurnID     string `json:"turn_id,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
	ResultID   string `json:"result_id,omitempty"`
	Generation uint32 `json:"generation,omitempty"`
	Text       string `json:"text,omitempty"`
	Audio      []byte `json:"audio,omitempty"`
	Error      string `json:"error,omitempty"`
}

func main() {
	token := os.Getenv("VOICE_TOKEN")
	origin := os.Getenv("VOICE_ORIGIN")
	if len(token) < 32 || origin == "" {
		log.Fatal("set VOICE_TOKEN (at least 32 characters) and VOICE_ORIGIN")
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" {
		log.Fatal("VOICE_ORIGIN must be an exact http(s) origin without a path")
	}
	asr, err := voice.NewWhisper(os.Getenv("VOICE_WHISPER_URL"))
	if err != nil {
		log.Fatal(err)
	}
	s := &server{token: token, origin: origin, target: os.Getenv("VOICE_TARGET"), asr: asr, adapter: voice.UnavailableAdapter{}, tts: swiftTTS{path: os.Getenv("VOICE_TTS_HELPER")}}
	if os.Getenv("VOICE_DEMO") == "1" {
		s.adapter = demoAdapter{}
		log.Print("DEMO MODE: replies echo recognized text; no Postman messages are sent")
	}
	if s.target == "" {
		log.Fatal("set VOICE_TARGET to the authorized Postman agent")
	}
	web, err := fs.Sub(assets, "web")
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(web)))
	mux.HandleFunc("/stream", s.stream)
	httpServer := &http.Server{Addr: "127.0.0.1:8765", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("voice gateway listening on %s; Postman adapter unavailable until #767 is integrated", httpServer.Addr)
	log.Fatal(httpServer.ListenAndServe())
}

func (s *server) stream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.Header.Get("Origin") != s.origin {
		http.Error(w, "origin rejected", http.StatusForbidden)
		return
	}
	// Browser WebSocket APIs cannot set Authorization. The secret is carried in
	// a second subprotocol value and checked before the upgrade. Never log it.
	var gotToken string
	authCount := 0
	for _, protocol := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		protocol = strings.TrimSpace(protocol)
		if strings.HasPrefix(protocol, "auth.") {
			authCount++
			gotToken = strings.TrimPrefix(protocol, "auth.")
		}
	}
	if authCount != 1 || subtle.ConstantTimeCompare([]byte(gotToken), []byte(s.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		http.Error(w, "session already active", http.StatusConflict)
		return
	}
	s.busy = true
	s.mu.Unlock()
	var turnExited <-chan struct{}
	releaseBusy := func() {
		s.mu.Lock()
		s.busy = false
		s.mu.Unlock()
	}
	releaseWhenAdapterIdle := func() {
		s.mu.Lock()
		work := s.adapterWork
		s.mu.Unlock()
		if work == nil {
			releaseBusy()
			return
		}
		select {
		case <-work:
			releaseBusy()
		default:
			go func() {
				<-work
				releaseBusy()
			}()
		}
	}
	defer func() {
		if turnExited != nil {
			joinTimeout := s.shutdownJoinTimeout
			if joinTimeout <= 0 {
				joinTimeout = 2 * time.Second
			}
			select {
			case <-turnExited:
			case <-time.After(joinTimeout):
				// A non-cooperative backend must not let a new session overlap it.
				go func() { <-turnExited; releaseWhenAdapterIdle() }()
				return
			}
		}
		releaseWhenAdapterIdle()
	}()
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"voice-v1"}})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(4096)
	sessionID, err := randomID()
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	generation := uint32(1)
	if err := writeEvent(ctx, conn, event{Type: "ready", SessionID: sessionID, Generation: generation}); err != nil {
		return
	}
	endpoint := voice.NewEndpoint(0.018)
	playingID := ""
	playingTurnID := ""
	playingRequestID := ""
	playingGeneration := uint32(0)
	var playbackDeadline <-chan time.Time
	var playbackTimer *time.Timer
	var playbackExpiresAt time.Time
	defer func() {
		if playbackTimer != nil {
			playbackTimer.Stop()
		}
	}()
	stopPlaybackTimer := func() {
		if playbackTimer != nil {
			playbackTimer.Stop()
			playbackTimer = nil
			playbackDeadline = nil
		}
	}
	mode := "listening"
	type received struct {
		kind    websocket.MessageType
		message []byte
		err     error
	}
	type turnDone struct {
		turnID     string
		requestID  string
		generation uint32
		result     processed
		err        error
	}
	readCh := make(chan received, 1)
	doneCh := make(chan turnDone, 1)
	go func() {
		for {
			kind, message, err := conn.Read(ctx)
			select {
			case readCh <- received{kind: kind, message: message, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-playbackDeadline:
			_ = writeEvent(ctx, conn, event{Type: "error", SessionID: sessionID, TurnID: playingTurnID, RequestID: playingRequestID, Error: "playback acknowledgement timed out"})
			return
		case outcome := <-doneCh:
			if mode != "processing" {
				continue
			}
			if outcome.err != nil {
				if writeEvent(ctx, conn, event{Type: "error", SessionID: sessionID, TurnID: outcome.turnID, RequestID: outcome.requestID, Error: outcome.err.Error()}) != nil {
					return
				}
				if errors.Is(outcome.err, errAdapterUnfinished) {
					return
				}
				mode = "listening"
				if writeEvent(ctx, conn, event{Type: "ready", SessionID: sessionID, Generation: generation}) != nil {
					return
				}
				continue
			}
			playingID = outcome.result.ResultID
			playingTurnID = outcome.turnID
			playingRequestID = outcome.requestID
			playingGeneration = outcome.generation
			mode = "playing"
			if writeEvent(ctx, conn, event{Type: "reply", SessionID: sessionID, TurnID: outcome.turnID, RequestID: outcome.requestID, ResultID: outcome.result.ResultID, Generation: outcome.generation, Text: outcome.result.Text, Audio: outcome.result.Audio}) != nil {
				return
			}
			timeout := s.playbackTimeout
			if timeout <= 0 {
				timeout = defaultPlaybackTimeout
			}
			playbackTimer = time.NewTimer(timeout)
			playbackDeadline = playbackTimer.C
			playbackExpiresAt = time.Now().Add(timeout)
		case incoming := <-readCh:
			if incoming.err != nil {
				return
			}
			kind, message := incoming.kind, incoming.message
			if kind == websocket.MessageText {
				var control struct {
					Type       string `json:"type"`
					SessionID  string `json:"session_id"`
					TurnID     string `json:"turn_id"`
					RequestID  string `json:"request_id"`
					ResultID   string `json:"result_id"`
					Generation uint32 `json:"generation"`
				}
				if json.Unmarshal(message, &control) != nil {
					return
				}
				if control.Type == "stop" {
					return
				}
				if (control.Type == "played" || control.Type == "playback_failed" || control.Type == "abort") && mode == "playing" && playingID != "" &&
					control.SessionID == sessionID && control.TurnID == playingTurnID &&
					control.RequestID == playingRequestID && control.ResultID == playingID &&
					control.Generation == playingGeneration {
					if !time.Now().Before(playbackExpiresAt) {
						_ = writeEvent(ctx, conn, event{Type: "error", SessionID: sessionID, TurnID: playingTurnID, RequestID: playingRequestID, Error: "playback acknowledgement timed out"})
						return
					}
					stopPlaybackTimer()
					if control.Type != "played" {
						_ = writeEvent(ctx, conn, event{Type: "error", SessionID: sessionID, TurnID: playingTurnID, RequestID: playingRequestID, Error: "playback failed or aborted"})
						return
					}
					playingID = ""
					playingTurnID = ""
					playingRequestID = ""
					playingGeneration = 0
					mode = "listening"
					endpoint.Reset()
					if err := writeEvent(ctx, conn, event{Type: "ready", SessionID: sessionID, Generation: generation}); err != nil {
						return
					}
				}
				continue
			}
			if mode != "listening" || kind != websocket.MessageBinary || len(message) != 4+voice.SamplesPerFrame*2 {
				continue
			}
			if binary.LittleEndian.Uint32(message[:4]) != generation {
				continue
			}
			frame := make([]int16, voice.SamplesPerFrame)
			for i := range frame {
				frame[i] = int16(binary.LittleEndian.Uint16(message[4+i*2:]))
			}
			utterance, err := endpoint.Add(frame)
			if err != nil {
				if writeEvent(ctx, conn, event{Type: "error", SessionID: sessionID, Error: err.Error()}) != nil {
					return
				}
				generation++
				if writeEvent(ctx, conn, event{Type: "ready", SessionID: sessionID, Generation: generation}) != nil {
					return
				}
				continue
			}
			if utterance == nil {
				continue
			}
			oldGeneration := generation
			generation++
			turnID, err := randomID()
			if err != nil {
				return
			}
			requestID, err := randomID()
			if err != nil {
				return
			}
			if writeEvent(ctx, conn, event{Type: "processing", SessionID: sessionID, TurnID: turnID, RequestID: requestID, Generation: oldGeneration}) != nil {
				return
			}
			mode = "processing"
			exited := make(chan struct{})
			turnExited = exited
			go func(turnID, requestID string, oldGeneration uint32, samples []int16) {
				defer close(exited)
				result, err := s.processTurn(ctx, sessionID, turnID, requestID, samples, func(text string) error {
					return writeEvent(ctx, conn, event{Type: "transcript", SessionID: sessionID, TurnID: turnID, RequestID: requestID, Generation: oldGeneration, Text: text})
				})
				select {
				case doneCh <- turnDone{turnID: turnID, requestID: requestID, generation: oldGeneration, result: result, err: err}:
				case <-ctx.Done():
				}
			}(turnID, requestID, oldGeneration, utterance)
		}
	}
}

type processed struct {
	voice.TurnResult
	Audio []byte
}

type demoAdapter struct{}

func (demoAdapter) Exchange(_ context.Context, request voice.TurnRequest) (voice.TurnResult, error) {
	resultID, err := randomID()
	if err != nil {
		return voice.TurnResult{}, err
	}
	return voice.TurnResult{SessionID: request.SessionID, TurnID: request.TurnID, RequestID: request.RequestID, ResultID: resultID, Status: "done", Text: "聞き取りました: " + request.Text}, nil
}

// finalTranscript is Stage 1. It produces no adapter request on any failure.
func (s *server) finalTranscript(ctx context.Context, samples []int16) (string, error) {
	if len(samples) == 0 || len(samples) > voice.MaxUtteranceSecs*voice.SampleRate {
		return "", errors.New("invalid utterance length")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	text, err := s.asr.Transcribe(ctx, samples)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	text = strings.TrimSpace(text)
	if err != nil || text == "" {
		if err == nil {
			err = errors.New("no speech recognized")
		}
		return "", err
	}
	if len(text) > 4096 {
		return "", errors.New("transcript too long")
	}
	return text, nil
}

// submitFinalText is Stage 2's single, text-only adapter call. The current
// production adapter is explicitly unavailable until Postman #767 is ready.
func (s *server) submitFinalText(ctx context.Context, request voice.TurnRequest) (voice.TurnResult, error) {
	if request.Text == "" || strings.TrimSpace(request.Text) != request.Text || len(request.Text) > 4096 ||
		request.SessionID == "" || request.TurnID == "" || request.RequestID == "" || request.Target == "" || request.Deadline.IsZero() {
		return voice.TurnResult{}, errors.New("invalid final text request")
	}
	ctx, cancel := context.WithDeadline(ctx, request.Deadline)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return voice.TurnResult{}, err
	}
	s.mu.Lock()
	if s.adapterWork != nil {
		select {
		case <-s.adapterWork:
		default:
			s.mu.Unlock()
			return voice.TurnResult{}, errAdapterBusy
		}
	}
	workDone := make(chan struct{})
	s.adapterWork = workDone
	s.mu.Unlock()
	type adapterOutcome struct {
		result voice.TurnResult
		err    error
	}
	resultCh := make(chan adapterOutcome, 1)
	go func() {
		result, err := s.adapter.Exchange(ctx, request)
		close(workDone)
		resultCh <- adapterOutcome{result: result, err: err}
	}()
	var result voice.TurnResult
	var err error
	select {
	case outcome := <-resultCh:
		result, err = outcome.result, outcome.err
	case <-ctx.Done():
		return voice.TurnResult{}, errors.Join(errAdapterUnfinished, ctx.Err())
	}
	if err != nil {
		return voice.TurnResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return voice.TurnResult{}, err
	}
	if result.SessionID != request.SessionID || result.TurnID != request.TurnID || result.RequestID != request.RequestID || result.ResultID == "" {
		return voice.TurnResult{}, errors.New("adapter returned mismatched IDs")
	}
	if result.Status != "done" && result.Status != "blocked" {
		return voice.TurnResult{}, errors.New("adapter returned nonterminal status")
	}
	if result.Text == "" || len(result.Text) > 8192 {
		return voice.TurnResult{}, errors.New("invalid reply text")
	}
	return result, nil
}

func (s *server) processTurn(ctx context.Context, sessionID, turnID, requestID string, samples []int16, onTranscript func(string) error) (processed, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	text, err := s.finalTranscript(ctx, samples)
	if err != nil {
		return processed{}, err
	}
	if err := onTranscript(text); err != nil {
		return processed{}, err
	}
	adapterTimeout := s.adapterTimeout
	if adapterTimeout <= 0 {
		adapterTimeout = 60 * time.Second
	}
	request := voice.TurnRequest{SessionID: sessionID, TurnID: turnID, RequestID: requestID, Target: s.target, Text: text, Deadline: time.Now().Add(adapterTimeout)}
	result, err := s.submitFinalText(ctx, request)
	if err != nil {
		return processed{}, err
	}
	if result.Status == "blocked" {
		return processed{}, errors.New("agent blocked the request")
	}
	audio, err := s.tts.Speak(ctx, result.Text)
	if err != nil {
		return processed{}, err
	}
	if len(audio) == 0 || len(audio) > 4<<20 {
		return processed{}, errors.New("generated audio exceeds limit")
	}
	if err := validateWAV(audio); err != nil {
		return processed{}, err
	}
	return processed{TurnResult: result, Audio: audio}, nil
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func writeEvent(ctx context.Context, conn *websocket.Conn, value event) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > 6<<20 {
		return fmt.Errorf("event too large")
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, data)
}
