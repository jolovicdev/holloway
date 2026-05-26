package tunnel

import "time"

const (
	MessageRequest  = "request"
	MessageResponse = "response"
)

type Request struct {
	Type       string              `json:"type"`
	ID         string              `json:"id"`
	Method     string              `json:"method"`
	Path       string              `json:"path"`
	Headers    map[string][]string `json:"headers"`
	Body       []byte              `json:"body"`
	Replay     bool                `json:"replay"`
	ReceivedAt time.Time           `json:"received_at"`
}

func (r Request) WithType() Request {
	r.Type = MessageRequest
	return r
}

type Response struct {
	Type       string `json:"type"`
	ID         string `json:"id"`
	StatusCode int    `json:"status_code"`
	Body       string `json:"body,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (r Response) WithType() Response {
	r.Type = MessageResponse
	return r
}
