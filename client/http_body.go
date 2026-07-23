package main

import (
	"fmt"
	"io"
)

const (
	maxCaptchaBootstrapResponseBytes int64 = 8 << 20
	maxCaptchaAPIResponseBytes       int64 = 2 << 20
	maxVKAPIResponseBytes            int64 = 2 << 20
	maxConferenceResponseBytes       int64 = 2 << 20
)

func readResponseBodyLimited(body io.Reader, maxBytes int64, description string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", description, maxBytes)
	}
	return data, nil
}
