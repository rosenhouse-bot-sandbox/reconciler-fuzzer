package interpose

import (
	"encoding/json"
	"net/http"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// API maps one system's HTTP API to resources. A nil API in Options is the
// Kubernetes map.
type API interface {
	// Classify reads a request. It sets Unmapped, and leaves Ref.Resource
	// empty, for a request it cannot place. It leaves r's body readable.
	Classify(r *http.Request) Request
	// Objects reads the objects a 2xx response to req shows, a write's
	// result first, from its header and body. It returns nil for a
	// response it cannot read, and an empty slice for one that shows no
	// object.
	Objects(req Request, header http.Header, body []byte) []Object
	// Error is the body and content type of an injected error.
	Error(code int, message string) (contentType string, body []byte)
}

// Object is one resource a response showed.
type Object struct {
	Ref  trace.Ref
	UID  string
	RV   string
	Body json.RawMessage
	// Partial says the response named the instance but not its whole
	// body, as an S3 PUT does. A listener takes the call's result from
	// it but records no Seen, and a fake records no version.
	Partial bool
}
