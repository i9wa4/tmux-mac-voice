package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os/exec"
	"strings"
)

type swiftTTS struct{ path string }

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errors.New("generated audio exceeds limit")
	}
	return b.Buffer.Write(p)
}

func (t swiftTTS) Speak(ctx context.Context, text string) ([]byte, error) {
	if t.path == "" {
		return nil, errors.New("TTS helper is not configured")
	}
	cmd := exec.CommandContext(ctx, t.path)
	cmd.Stdin = strings.NewReader(text)
	output := &limitedBuffer{limit: 4 << 20}
	cmd.Stdout = output
	var stderr limitedBuffer
	stderr.limit = 1024
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, errors.New("TTS helper failed: " + err.Error())
	}
	if err := validateWAV(output.Bytes()); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// The helper emits a canonical 44-byte, mono PCM16 RIFF/WAVE header.
// Validate the complete container before sending any bytes to the browser.
func validateWAV(b []byte) error {
	invalid := errors.New("TTS helper returned invalid WAV")
	if len(b) < 46 || len(b) > 4<<20 || string(b[0:4]) != "RIFF" ||
		binary.LittleEndian.Uint32(b[4:8]) != uint32(len(b)-8) ||
		string(b[8:16]) != "WAVEfmt " ||
		binary.LittleEndian.Uint32(b[16:20]) != 16 ||
		binary.LittleEndian.Uint16(b[20:22]) != 1 ||
		binary.LittleEndian.Uint16(b[22:24]) != 1 ||
		binary.LittleEndian.Uint16(b[32:34]) != 2 ||
		binary.LittleEndian.Uint16(b[34:36]) != 16 ||
		string(b[36:40]) != "data" ||
		binary.LittleEndian.Uint32(b[40:44]) != uint32(len(b)-44) ||
		(len(b)-44)%2 != 0 {
		return invalid
	}
	rate := binary.LittleEndian.Uint32(b[24:28])
	if rate < 8000 || rate > 96000 || binary.LittleEndian.Uint32(b[28:32]) != rate*2 {
		return invalid
	}
	return nil
}
