package main

import (
	"encoding/binary"
	"testing"
)

func validTestWAV() []byte {
	b := make([]byte, 46)
	copy(b[0:], "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 22050)
	binary.LittleEndian.PutUint32(b[28:], 44100)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], 2)
	return b
}

func TestValidateWAVRejectsTruncatedAndMismatchedContainers(t *testing.T) {
	valid := validTestWAV()
	if err := validateWAV(valid); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func([]byte) []byte{
		"truncated":       func(b []byte) []byte { return b[:len(b)-1] },
		"trailing data":   func(b []byte) []byte { return append(b, 0, 0) },
		"not WAVE":        func(b []byte) []byte { copy(b[8:], "NOPE"); return b },
		"compressed":      func(b []byte) []byte { binary.LittleEndian.PutUint16(b[20:], 3); return b },
		"stereo":          func(b []byte) []byte { binary.LittleEndian.PutUint16(b[22:], 2); return b },
		"wrong byte rate": func(b []byte) []byte { binary.LittleEndian.PutUint32(b[28:], 1); return b },
		"wrong data size": func(b []byte) []byte { binary.LittleEndian.PutUint32(b[40:], 4); return b },
		"wrong bit depth": func(b []byte) []byte { binary.LittleEndian.PutUint16(b[34:], 8); return b },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			b := mutate(append([]byte(nil), valid...))
			if err := validateWAV(b); err == nil {
				t.Fatal("invalid WAV accepted")
			}
		})
	}
}
