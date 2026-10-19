// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/forge/ir"
)

func TestGuardRefusesBeforeDial(t *testing.T) {
	dialled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dialled = true
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	refuse := errors.New("not public")
	exec := NewHTTPExecutor(srv.URL, nil, Credential{}, Options{
		Guard: func(_ context.Context, u *url.URL) error { return refuse },
	})
	op := &ir.Operation{ID: "ping", Method: http.MethodGet, Path: "/ping"}
	_, err := exec.Call(context.Background(), op, nil, "")
	var g *GuardRefusedError
	if !errors.As(err, &g) || !errors.Is(err, refuse) {
		t.Fatalf("err = %v, want GuardRefusedError wrapping the guard's reason", err)
	}
	if dialled {
		t.Fatal("the request was dialled despite the refusal")
	}

	// An allowing guard lets the call through, and the host's client is the one used.
	used := false
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		used = true
		return http.DefaultTransport.RoundTrip(r)
	})}
	exec = NewHTTPExecutor(srv.URL, nil, Credential{}, Options{
		HTTPClient: client,
		Guard:      func(_ context.Context, u *url.URL) error { return nil },
	})
	out, err := exec.Call(context.Background(), op, nil, "")
	if err != nil || out == "" {
		t.Fatalf("allowed call failed: %v (%q)", err, out)
	}
	if !used {
		t.Fatal("the host-supplied client was not used")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
