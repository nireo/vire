package gateway

import (
	"bytes"
	"encoding/json"
	"io"
)

const (
	maxJSONUsageResponse = 1 << 20  // Keep ordinary responses only up to 1 MiB.
	maxSSEUsageTail      = 64 << 10 // vLLM puts usage just before [DONE].
)

type tokenUsage struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
}

// usageCapture receives bytes as the reverse proxy reads them from vLLM. The
// proxy still writes each read to the client immediately. A large ordinary
// response has unknown usage; a stream retains only its last 64 KiB.
type usageCapture struct {
	status        int
	stream        bool
	eof           bool
	overflow      bool
	body          []byte
	tailNext      int
	tailSize      int
	tailTruncated bool
}

type usageBody struct {
	io.ReadCloser
	capture *usageCapture
}

func (body *usageBody) Read(p []byte) (int, error) {
	n, err := body.ReadCloser.Read(p)
	if n > 0 {
		body.capture.observe(p[:n])
	}
	if err == io.EOF {
		body.capture.eof = true
	}
	return n, err
}

func (capture *usageCapture) observe(p []byte) {
	if !capture.stream {
		if capture.overflow {
			return
		}
		if len(capture.body)+len(p) > maxJSONUsageResponse {
			capture.body = nil
			capture.overflow = true
			return
		}
		capture.body = append(capture.body, p...)
		return
	}

	if capture.body == nil {
		capture.body = make([]byte, maxSSEUsageTail)
	}
	if len(p) >= maxSSEUsageTail {
		capture.tailTruncated = capture.tailSize > 0 || len(p) > maxSSEUsageTail
		copy(capture.body, p[len(p)-maxSSEUsageTail:])
		capture.tailNext = 0
		capture.tailSize = maxSSEUsageTail
		return
	}
	if capture.tailSize+len(p) > maxSSEUsageTail {
		capture.tailTruncated = true
	}
	n := copy(capture.body[capture.tailNext:], p)
	copy(capture.body, p[n:])
	capture.tailNext = (capture.tailNext + len(p)) % maxSSEUsageTail
	capture.tailSize = min(maxSSEUsageTail, capture.tailSize+len(p))
}

func (capture *usageCapture) tail() []byte {
	if capture.tailSize < maxSSEUsageTail {
		return capture.body[:capture.tailSize]
	}
	result := make([]byte, maxSSEUsageTail)
	n := copy(result, capture.body[capture.tailNext:])
	copy(result[n:], capture.body[:capture.tailNext])
	return result
}

func (capture *usageCapture) result(interrupted bool) (int, string, *int64, *int64) {
	if interrupted || capture.status == 0 {
		return capture.status, "incomplete", nil, nil
	}
	if capture.status < 200 || capture.status >= 300 {
		return capture.status, "failed", nil, nil
	}
	if !capture.eof || capture.overflow {
		return capture.status, "incomplete", nil, nil
	}

	var usage *tokenUsage
	if capture.stream {
		usage = capture.streamUsage()
	} else {
		var response struct {
			Usage *tokenUsage `json:"usage"`
		}
		if json.Unmarshal(capture.body, &response) == nil {
			usage = response.Usage
		}
	}
	if !validUsage(usage) {
		return capture.status, "incomplete", nil, nil
	}
	return capture.status, "complete", usage.PromptTokens, usage.CompletionTokens
}

func (capture *usageCapture) streamUsage() *tokenUsage {
	tail := capture.tail()
	if !bytes.HasSuffix(tail, []byte("\n\n")) && !bytes.HasSuffix(tail, []byte("\r\n\r\n")) {
		return nil // [DONE] must terminate an SSE event.
	}
	lines := bytes.Split(tail, []byte{'\n'})
	if capture.tailTruncated {
		lines = lines[1:] // The ring may start in the middle of a line.
	}
	if len(lines) == 0 || len(lines[len(lines)-1]) != 0 {
		return nil // The last SSE line must be complete.
	}
	var usage *tokenUsage
	done := false
	for _, line := range lines[:len(lines)-1] {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if bytes.Equal(data, []byte("[DONE]")) {
			done = validUsage(usage)
			continue
		}
		done = false
		var event struct {
			Usage *tokenUsage `json:"usage"`
		}
		if json.Unmarshal(data, &event) == nil && validUsage(event.Usage) {
			usage = event.Usage
		}
	}
	if done {
		return usage
	}
	return nil
}

func validUsage(usage *tokenUsage) bool {
	return usage != nil && usage.PromptTokens != nil && usage.CompletionTokens != nil &&
		*usage.PromptTokens >= 0 && *usage.CompletionTokens >= 0
}
