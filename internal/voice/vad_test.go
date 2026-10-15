package voice

import "testing"

func TestEndpointSplitsTwoUtterances(t *testing.T) {
	e := NewEndpoint(0.02)
	quiet := make([]int16, SamplesPerFrame)
	loud := make([]int16, SamplesPerFrame)
	for i := range loud {
		loud[i] = 4000
	}
	var turns int
	for cycle := 0; cycle < 2; cycle++ {
		for i := 0; i < 20; i++ {
			if utterance, err := e.Add(loud); err != nil || utterance != nil {
				t.Fatalf("early endpoint: %v %d", err, len(utterance))
			}
		}
		for i := 0; i < 35; i++ {
			utterance, err := e.Add(quiet)
			if err != nil {
				t.Fatal(err)
			}
			if utterance != nil {
				turns++
				if len(utterance) < 20*SamplesPerFrame {
					t.Fatalf("truncated utterance: %d", len(utterance))
				}
			}
		}
	}
	if turns != 2 {
		t.Fatalf("got %d turns, want 2", turns)
	}
}

func TestEndpointDropsOverflow(t *testing.T) {
	e := NewEndpoint(0.02)
	frame := make([]int16, SamplesPerFrame)
	for i := range frame {
		frame[i] = 4000
	}
	var overflow bool
	for i := 0; i < MaxUtteranceSecs*50+1; i++ {
		_, err := e.Add(frame)
		if err == ErrUtteranceTooLong {
			overflow = true
			break
		}
	}
	if !overflow {
		t.Fatal("utterance was not bounded")
	}
}
