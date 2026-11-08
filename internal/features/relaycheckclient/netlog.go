package relaycheckclient

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const netLogSize = 300

// NetEvent is one thing this program sent out, for the page's network record, so
// a user can see where their key went. It carries the address and what kind of
// content was sent, never the content, the key or a query string.
type NetEvent struct {
	At     string `json:"at"`
	Host   string `json:"host"`
	Path   string `json:"path"`
	Sent   string `json:"sent"`
	Status int    `json:"status,omitempty"`
	Error  bool   `json:"error,omitempty"`
}

// NetLog keeps the most recent events of this run in memory.
type NetLog struct {
	mu     sync.Mutex
	events []NetEvent
	server string
}

// NewNetLog starts a record; server is check-api, told apart from relays.
func NewNetLog(server string) *NetLog {
	host := server
	if parsed, err := url.Parse(server); err == nil && parsed.Host != "" {
		host = parsed.Host
	}
	return &NetLog{server: host}
}

func (l *NetLog) add(event NetEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
	if len(l.events) > netLogSize {
		l.events = l.events[len(l.events)-netLogSize:]
	}
}

// Events returns the record, newest last.
func (l *NetLog) Events() []NetEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]NetEvent{}, l.events...)
}

// describe says in words what a request carries, from where it goes.
func (l *NetLog) describe(req *http.Request) string {
	if req.URL.Host == l.server {
		switch {
		case strings.HasSuffix(req.URL.Path, "/judge"):
			return "模型名、题目长度、回答文本"
		case strings.HasSuffix(req.URL.Path, "/challenge"):
			return "模型名"
		default:
			return "无"
		}
	}
	switch {
	case strings.HasSuffix(req.URL.Path, "/v1/usage"), strings.HasSuffix(req.URL.Path, "/v1/models"):
		return "你配置给这个地址的密钥"
	case req.Method == http.MethodGet:
		return "无"
	default:
		return "你配置给这个地址的密钥、检测题目"
	}
}

// Transport records every request that passes through base.
func (l *NetLog) Transport(base http.RoundTripper) http.RoundTripper {
	return recordingTransport{log: l, base: base}
}

type recordingTransport struct {
	log  *NetLog
	base http.RoundTripper
}

func (t recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	event := NetEvent{At: time.Now().Format(time.RFC3339), Host: req.URL.Host, Path: req.URL.Path, Sent: t.log.describe(req)}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		event.Error = true
	} else {
		event.Status = resp.StatusCode
	}
	t.log.add(event)
	return resp, err
}

// RecordCommand notes an official CLI run: what that program sends is its own and
// passes through no transport of ours.
func (l *NetLog) RecordCommand(name string) {
	l.add(NetEvent{At: time.Now().Format(time.RFC3339), Host: "本机命令", Path: name, Sent: "检测题目（由官方命令行自己发出）"})
}
