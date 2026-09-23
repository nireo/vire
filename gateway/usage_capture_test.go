package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func readUsageResponse(t *testing.T, capture *usageCapture, response string) {
	t.Helper()
	body := &usageBody{ReadCloser: io.NopCloser(strings.NewReader(response)), capture: capture}
	if _, err := io.Copy(io.Discard, body); err != nil {
		t.Fatal(err)
	}
}

func TestUsageCaptureJSON(t *testing.T) {
	capture := &usageCapture{status: http.StatusOK}
	readUsageResponse(t, capture, `{"choices":[{"message":{"usage":{"prompt_tokens":999}}}],"usage":{"prompt_tokens":120,"completion_tokens":45}}`)
	status, outcome, prompt, completion := capture.result(false)
	if status != http.StatusOK || outcome != "complete" || prompt == nil || completion == nil || *prompt != 120 || *completion != 45 {
		t.Fatalf("status=%d outcome=%s prompt=%v completion=%v", status, outcome, prompt, completion)
	}
}

func TestUsageCaptureBoundsLargeJSON(t *testing.T) {
	capture := &usageCapture{status: http.StatusOK}
	readUsageResponse(t, capture, `{"choices":[{"message":{"content":"`+strings.Repeat("x", 2<<20)+`"}}],"usage":{"prompt_tokens":120,"completion_tokens":45}}`)
	_, outcome, prompt, completion := capture.result(false)
	if outcome != "incomplete" || prompt != nil || completion != nil || len(capture.body) > maxJSONUsageResponse {
		t.Fatalf("outcome=%s prompt=%v completion=%v captured=%d", outcome, prompt, completion, len(capture.body))
	}
}

func TestUsageCaptureStreamTail(t *testing.T) {
	capture := &usageCapture{status: http.StatusOK, stream: true}
	response := strings.Repeat("data: {\"choices\":[]}\n\n", 10_000) +
		"data: {\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\r\n\r\n" +
		"data: [DONE]\n\n"
	readUsageResponse(t, capture, response)
	_, outcome, prompt, completion := capture.result(false)
	if outcome != "complete" || prompt == nil || completion == nil || *prompt != 4 || *completion != 2 || len(capture.body) != maxSSEUsageTail {
		t.Fatalf("outcome=%s prompt=%v completion=%v captured=%d", outcome, prompt, completion, len(capture.body))
	}
}

func TestUsageCaptureIncompleteStream(t *testing.T) {
	for _, response := range []string{
		"data: {\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n\n",
		"data: {\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n\ndata: [DONE]",
		"data: {\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n\ndata: [DONE]\n",
		"data: {\"usage\":{\"completion_tokens\":2}}\n\ndata: [DONE]\n\n",
	} {
		capture := &usageCapture{status: http.StatusOK, stream: true}
		readUsageResponse(t, capture, response)
		_, outcome, prompt, completion := capture.result(false)
		if outcome != "incomplete" || prompt != nil || completion != nil {
			t.Fatalf("response=%q outcome=%s prompt=%v completion=%v", response, outcome, prompt, completion)
		}
	}
}

func TestUsageCaptureRejectsPartialJSONUsage(t *testing.T) {
	capture := &usageCapture{status: http.StatusOK}
	readUsageResponse(t, capture, `{"usage":{"completion_tokens":2},"choices":[]}`)
	_, outcome, prompt, completion := capture.result(false)
	if outcome != "incomplete" || prompt != nil || completion != nil {
		t.Fatalf("outcome=%s prompt=%v completion=%v", outcome, prompt, completion)
	}
}
