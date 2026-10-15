package voice

import (
	"errors"
	"math"
)

// The browser sends 20 ms, mono, signed little-endian PCM frames at 16 kHz.
const (
	SampleRate       = 16000
	SamplesPerFrame  = 320
	MaxUtteranceSecs = 20
)

var ErrUtteranceTooLong = errors.New("utterance exceeded 20 seconds")

// Endpoint is a deliberately simple receiver-side energy gate. It is not a
// speech classifier; device testing must tune it for actual microphones.
type Endpoint struct {
	threshold float64
	preRoll   []int16
	utterance []int16
	voiced    int
	silent    int
	active    bool
}

func NewEndpoint(threshold float64) *Endpoint {
	return &Endpoint{threshold: threshold}
}

// Add accepts exactly one frame. It returns an utterance after 700 ms of
// silence following speech. On overflow it discards the utterance.
func (e *Endpoint) Add(frame []int16) ([]int16, error) {
	if len(frame) != SamplesPerFrame {
		return nil, errors.New("invalid PCM frame length")
	}
	var energy float64
	for _, sample := range frame {
		v := float64(sample) / 32768
		energy += v * v
	}
	voiced := math.Sqrt(energy/float64(len(frame))) >= e.threshold
	if !e.active {
		e.preRoll = append(e.preRoll, frame...)
		if len(e.preRoll) > 15*SamplesPerFrame {
			e.preRoll = e.preRoll[len(e.preRoll)-15*SamplesPerFrame:]
		}
		if voiced {
			e.voiced++
		} else {
			e.voiced = 0
		}
		if e.voiced < 2 {
			return nil, nil
		}
		e.active = true
		e.utterance = append(e.utterance[:0], e.preRoll...)
		e.preRoll = nil
		return nil, nil
	}
	if len(e.utterance)+len(frame) > MaxUtteranceSecs*SampleRate {
		e.Reset()
		return nil, ErrUtteranceTooLong
	}
	e.utterance = append(e.utterance, frame...)
	if voiced {
		e.silent = 0
	} else {
		e.silent++
	}
	if e.silent < 35 {
		return nil, nil
	}
	result := append([]int16(nil), e.utterance...)
	e.Reset()
	return result, nil
}

func (e *Endpoint) Reset() {
	e.preRoll = nil
	e.utterance = nil
	e.voiced = 0
	e.silent = 0
	e.active = false
}
