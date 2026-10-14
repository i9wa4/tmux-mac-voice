package voice

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWhisperSendsBoundedJapaneseWAV(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/inference" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		if r.FormValue("language") != "ja" || r.FormValue("response_format") != "json" {
			t.Errorf("unexpected fields: %#v", r.Form)
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		header := make([]byte, 44)
		if _, err := f.Read(header); err != nil {
			t.Error(err)
		}
		if string(header[:4]) != "RIFF" || binary.LittleEndian.Uint32(header[24:]) != SampleRate {
			t.Errorf("not a 16 kHz WAV")
		}
		w.Write([]byte(`{"text":"  こんにちは  "}`))
	}))
	defer server.Close()
	w, err := NewWhisper(server.URL + "/inference")
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.Transcribe(context.Background(), make([]int16, SampleRate))
	if err != nil || got != "こんにちは" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestWhisperRejectsNonLoopback(t *testing.T) {
	_, err := NewWhisper("http://example.com/inference")
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWhisperDoesNotFollowRedirects(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var escaped atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				escaped.Add(1)
				w.Write([]byte(`{"text":"escaped"}`))
			}))
			defer target.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+"/capture", status)
			}))
			defer source.Close()
			whisper, err := NewWhisper(source.URL + "/inference")
			if err != nil {
				t.Fatal(err)
			}
			_, err = whisper.Transcribe(context.Background(), make([]int16, SampleRate))
			if err == nil || !strings.Contains(err.Error(), strconv.Itoa(status)) {
				t.Fatalf("redirect status: %v", err)
			}
			if escaped.Load() != 0 {
				t.Fatal("multipart body escaped to redirect target")
			}
		})
	}
}
