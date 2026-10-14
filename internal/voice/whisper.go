package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxTranscriptBytes = 16 << 10

type Whisper struct {
	endpoint string
	client   *http.Client
}

func NewWhisper(endpoint string) (*Whisper, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Path != "/inference" {
		return nil, errors.New("whisper endpoint must be loopback http /inference")
	}
	host := u.Hostname()
	if host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback()) {
		return nil, errors.New("whisper endpoint must be loopback")
	}
	return &Whisper{endpoint: endpoint, client: &http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}, nil
}

func (w *Whisper) Transcribe(ctx context.Context, samples []int16) (string, error) {
	if len(samples) == 0 || len(samples) > MaxUtteranceSecs*SampleRate {
		return "", errors.New("invalid utterance length")
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	file, err := mw.CreateFormFile("file", "utterance.wav")
	if err != nil {
		return "", err
	}
	if _, err = file.Write(wav(samples)); err != nil {
		return "", err
	}
	for key, value := range map[string]string{"response_format": "json", "language": "ja", "translate": "false"} {
		if err = mw.WriteField(key, value); err != nil {
			return "", err
		}
	}
	if err = mw.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.endpoint, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := w.client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("whisper returned HTTP %d", res.StatusCode)
	}
	var result struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxTranscriptBytes)).Decode(&result); err != nil {
		return "", err
	}
	text := strings.TrimSpace(result.Text)
	if len(text) > maxTranscriptBytes {
		return "", errors.New("transcript too large")
	}
	return text, nil
}

func wav(samples []int16) []byte {
	const header = 44
	b := make([]byte, header+2*len(samples))
	copy(b[0:], "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], SampleRate)
	binary.LittleEndian.PutUint32(b[28:], SampleRate*2)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(2*len(samples)))
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(b[header+i*2:], uint16(sample))
	}
	return b
}
