package main

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

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
