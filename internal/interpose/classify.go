package interpose

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Request is what a listener reads from a Kubernetes API request.
type Request struct {
	Verb        string
	APIVersion  string
	Ref         trace.Ref
	Subresource string
	Watch       bool
	// RequestName is a create's requested name, read by an API map.
	RequestName string
	Unmapped    bool
	// Partial says a list or watch has a label or field selector, or a
	// continue token, so it may leave objects out.
	Partial bool
}

// namespaceSubresources follow a namespace name instead of scoping a resource.
var namespaceSubresources = map[string]bool{"status": true, "finalize": true}

// Classify reads /api/<version> or /apis/<group>/<version>, an optional
// /namespaces/<namespace>, then <resource>[/<name>[/<subresource>]]. A
// non-resource URL leaves Ref.Resource empty and takes the lowercased method
// as its verb. A namespace is cluster-scoped, so its Ref has no Namespace.
func Classify(r *http.Request) Request {
	query := r.URL.Query()
	watch, err := strconv.ParseBool(query.Get("watch"))
	req := Request{Watch: err == nil && watch}
	req.parsePath(strings.Split(strings.Trim(r.URL.Path, "/"), "/"))
	req.Verb = verb(r.Method, req)
	req.Partial = (req.Verb == "list" || req.Verb == "watch") &&
		(query.Get("labelSelector") != "" || query.Get("fieldSelector") != "" || query.Get("continue") != "")
	return req
}

func (r *Request) parsePath(segments []string) {
	switch {
	case len(segments) >= 2 && segments[0] == "api":
		r.APIVersion = segments[1]
		segments = segments[2:]
	case len(segments) >= 3 && segments[0] == "apis":
		r.Ref.Group, r.APIVersion = segments[1], segments[2]
		segments = segments[3:]
	default:
		return
	}
	if len(segments) > 2 && segments[0] == "namespaces" && !namespaceSubresources[segments[2]] {
		r.Ref.Namespace = segments[1]
		segments = segments[2:]
	}
	if len(segments) > 0 {
		r.Ref.Resource = segments[0]
	}
	if len(segments) > 1 {
		r.Ref.Name = segments[1]
	}
	if len(segments) > 2 {
		r.Subresource = segments[2]
	}
}

func verb(method string, r Request) string {
	if r.Ref.Resource == "" {
		return strings.ToLower(method)
	}
	switch method {
	case http.MethodPost:
		return "create"
	case http.MethodPut:
		return "update"
	case http.MethodPatch:
		return "patch"
	case http.MethodDelete:
		if r.Ref.Name == "" {
			return "deletecollection"
		}
		return "delete"
	case http.MethodGet, http.MethodHead:
		switch {
		case r.Watch:
			return "watch"
		case r.Ref.Name == "":
			return "list"
		default:
			return "get"
		}
	}
	return strings.ToLower(method)
}

func reading(verb string) bool { return verb == "get" || verb == "list" || verb == "watch" }

func mutating(verb string) bool {
	switch verb {
	case "create", "update", "patch", "delete", "deletecollection":
		return true
	}
	return false
}

// Matches reports whether m selects r.
func Matches(m plan.Match, r Request) bool {
	return field(m.Verb, r.Verb) &&
		field(m.Group, r.Ref.Group) &&
		field(m.Resource, r.Ref.Resource) &&
		subresource(m.Subresource, r.Subresource) &&
		field(m.Name, r.Ref.Name)
}

func field(want, got string) bool { return want == "" || want == got }

func subresource(want, got string) bool {
	if want == plan.NoSubresource {
		return got == ""
	}
	return field(want, got)
}
