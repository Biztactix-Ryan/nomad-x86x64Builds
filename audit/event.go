// +build ent

package audit

import (
	"context"
	"time"

	"github.com/hashicorp/go-eventlogger"
)

const (
	OperationReceived Stage = "OperationReceived"
	OperationComplete Stage = "OperationComplete"
	AllStages         Stage = "*"
)

type Stage string
type HTTPOperation string
type FilterType string

// Event represents a audit log entry.
type Event struct {
	ID        string                `json:"id"`
	Stage     Stage                 `json:"stage"`
	Type      eventlogger.EventType `json:"type"`
	Timestamp time.Time             `json:"timestamp"`
	Version   int                   `json:"version"`
	Auth      *Auth                 `json:"auth"`
	Request   *Request              `json:"request"`
	Response  *Response             `json:"response,omitempty"`
}

type Auth struct {
	AccessorID string    `json:"accessor_id,omitempty"`
	Name       string    `json:"name,omitempty"`
	Type       string    `json:"type,omitempty"`
	Policies   []string  `json:"policies,omitempty"`
	Global     bool      `json:"global,omitempty"`
	CreateTime time.Time `json:"create_time,omitempty"`
}

type Request struct {
	ID          string            `json:"id"`
	Operation   string            `json:"operation"`
	Endpoint    string            `json:"endpoint"`
	Namespace   map[string]string `json:"namespace,omitempty"`
	RequestMeta map[string]string `json:"request_meta,omitempty"`
	NodeMeta    map[string]string `json:"node_meta,omitempty"`
}

type Response struct {
	StatusCode int    `json:"status_code"`
	Error      string `json:"error,omitempty"`
	raw        []byte
}

// Matches checks if stage matches a particular stage
// or if either stage is AllStages
func (s Stage) Matches(b Stage) bool {
	return s == b || s == AllStages || b == AllStages
}

func (s Stage) Valid() bool {
	switch s {
	case OperationReceived, OperationComplete, AllStages:
		return true
	default:
		return false
	}
}

func (s Stage) String() string {
	return string(s)
}

func (f FilterType) Valid() bool {
	switch f {
	case HTTPEvent:
		return true
	default:
		return false
	}
}

type Eventer interface {
	Event(ctx context.Context, s Stage, e Event) error
}
