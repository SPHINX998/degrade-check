package relayupstream

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// maxResponseBody caps what is read from one upstream response.
const maxResponseBody = 16 << 20

// Doer sends one HTTP request. *http.Client satisfies it.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// UpstreamError is a non-success answer from an upstream site.
type UpstreamError struct {
	Status  int
	Code    string
	Message string
}

func (e *UpstreamError) Error() string {
	parts := []string{}
	if e.Status != 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", e.Status))
	}
	if e.Code != "" {
		parts = append(parts, e.Code)
	}
	head := strings.Join(parts, " ")
	if e.Message == "" {
		return "上游拒绝：" + head
	}
	if head == "" {
		return "上游拒绝：" + e.Message
	}
	return "上游拒绝：" + head + "：" + e.Message
}

// snippet is a short, single-line view of a body for error messages.
func snippet(body []byte) string {
	text := strings.Join(strings.Fields(string(body)), " ")
	if len([]rune(text)) > 160 {
		text = string([]rune(text)[:160]) + "…"
	}
	return text
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func randomKey() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
