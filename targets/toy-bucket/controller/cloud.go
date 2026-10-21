package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// The states of a bucket in the cloud.
const (
	Creating = "CREATING"
	Ready    = "READY"
	Deleting = "DELETING"
)

// CloudBucket is a bucket as the cloud's JSON shows it.
type CloudBucket struct {
	ID     string            `json:"id,omitempty"`
	Name   string            `json:"name"`
	Region string            `json:"region,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
	State  string            `json:"state,omitempty"`
}

// Cloud is a client of the cloud's /buckets API.
type Cloud struct {
	Endpoint string
	// HTTP defaults to a client with a 10 s timeout.
	HTTP *http.Client
}

// StatusError is a non-2xx answer.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("cloud answered %d: %s", e.Code, e.Body) }

// unknownOutcome wraps a transport error: the request may have committed.
type unknownOutcome struct{ error }

func (e unknownOutcome) Unwrap() error { return e.error }

// IsUnknownOutcome says whether err left the request's outcome unknown.
func IsUnknownOutcome(err error) bool { return errors.As(err, new(unknownOutcome)) }

// IsNotFound says whether err is a 404.
func IsNotFound(err error) bool { return statusCode(err) == http.StatusNotFound }

// IsConflict says whether err is a 409.
func IsConflict(err error) bool { return statusCode(err) == http.StatusConflict }

func statusCode(err error) int {
	var s *StatusError
	if errors.As(err, &s) {
		return s.Code
	}
	return 0
}

var defaultHTTP = &http.Client{Timeout: 10 * time.Second}

func (c *Cloud) Create(ctx context.Context, b CloudBucket) (CloudBucket, error) {
	return c.one(ctx, http.MethodPost, "/buckets", b)
}

func (c *Cloud) Get(ctx context.Context, id string) (CloudBucket, error) {
	return c.one(ctx, http.MethodGet, "/buckets/"+url.PathEscape(id), nil)
}

func (c *Cloud) Patch(ctx context.Context, id, region string) (CloudBucket, error) {
	return c.one(ctx, http.MethodPatch, "/buckets/"+url.PathEscape(id), map[string]string{"region": region})
}

func (c *Cloud) Delete(ctx context.Context, id string) (CloudBucket, error) {
	return c.one(ctx, http.MethodDelete, "/buckets/"+url.PathEscape(id), nil)
}

// FindByName lists the buckets named name.
func (c *Cloud) FindByName(ctx context.Context, name string) ([]CloudBucket, error) {
	var list struct{ Items []CloudBucket }
	err := c.do(ctx, http.MethodGet, "/buckets?name="+url.QueryEscape(name), nil, &list)
	return list.Items, err
}

func (c *Cloud) one(ctx context.Context, method, path string, body any) (CloudBucket, error) {
	var b CloudBucket
	err := c.do(ctx, method, path, body, &b)
	return b, err
}

func (c *Cloud) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Endpoint+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = defaultHTTP
	}
	resp, err := client.Do(req)
	if err != nil {
		return unknownOutcome{err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return unknownOutcome{err}
	}
	if resp.StatusCode/100 != 2 {
		return &StatusError{Code: resp.StatusCode, Body: string(bytes.TrimSpace(data))}
	}
	return json.Unmarshal(data, out)
}
