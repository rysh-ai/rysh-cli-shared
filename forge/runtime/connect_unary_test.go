// SPDX-License-Identifier: Apache-2.0

package runtime_test

// Unary gRPC calls against a REAL connect-go server (E23 W2 task 2). Ops come
// from ingest.GRPC, exactly as forge builds them, so the test exercises the
// IR → executor path a forged gRPC API actually takes. The read-verb method
// (GetEcho) is the interesting one: the ingester marks it non-mutating for the
// approval heuristic, and the executor must still send the request message.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/rysh-ai/rysh-cli-shared/forge/ingest"
	"github.com/rysh-ai/rysh-cli-shared/forge/ir"
	"github.com/rysh-ai/rysh-cli-shared/forge/runtime"
)

// echoAPI ingests: package echo; service Echo { rpc GetEcho(Msg) returns (Msg);
// rpc CreateEcho(Msg) returns (Msg); }
func echoAPI(t *testing.T) *ir.API {
	t.Helper()
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	fds := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name:    proto.String("echo.proto"),
		Package: proto.String("echo"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name:  proto.String("Msg"),
			Field: []*descriptorpb.FieldDescriptorProto{{Name: proto.String("text"), Number: proto.Int32(1), Label: &opt, Type: &str}},
		}},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("Echo"),
			Method: []*descriptorpb.MethodDescriptorProto{
				{Name: proto.String("GetEcho"), InputType: proto.String(".echo.Msg"), OutputType: proto.String(".echo.Msg")},
				{Name: proto.String("CreateEcho"), InputType: proto.String(".echo.Msg"), OutputType: proto.String(".echo.Msg")},
			},
		}},
	}}}
	b, err := proto.Marshal(fds)
	if err != nil {
		t.Fatal(err)
	}
	api, err := ingest.GRPC(b)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	return api
}

func opByID(t *testing.T, api *ir.API, id string) *ir.Operation {
	t.Helper()
	for i := range api.Operations {
		if api.Operations[i].ID == id {
			return &api.Operations[i]
		}
	}
	t.Fatalf("no op %q", id)
	return nil
}

// connectEchoServer serves both methods with a real connect-go unary handler
// that echoes "<method>:<text>". handlerOpts lets a test make it strict.
func connectEchoServer(t *testing.T, handlerOpts ...connect.HandlerOption) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for _, m := range []string{"GetEcho", "CreateEcho"} {
		proc := "/echo.Echo/" + m
		name := m
		mux.Handle(proc, connect.NewUnaryHandler(proc,
			func(_ context.Context, req *connect.Request[structpb.Struct]) (*connect.Response[structpb.Struct], error) {
				out, _ := structpb.NewStruct(map[string]any{
					"text": name + ":" + req.Msg.GetFields()["text"].GetStringValue(),
				})
				return connect.NewResponse(out), nil
			}, handlerOpts...))
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestUnaryCallsReachConnectServer: in BOTH stream-protocol modes, a read-verb
// and a write-verb unary method reach a real Connect server with their request
// message intact.
func TestUnaryCallsReachConnectServer(t *testing.T) {
	api := echoAPI(t)
	srv := connectEchoServer(t)
	for _, mode := range []string{"", runtime.StreamProtocolGateway, runtime.StreamProtocolConnect} {
		for _, id := range []string{"Echo_GetEcho", "Echo_CreateEcho"} {
			t.Run(mode+"/"+id, func(t *testing.T) {
				exec := runtime.NewHTTPExecutor(srv.URL, nil, runtime.Credential{}, runtime.Options{StreamProtocol: mode, MaxRetries: 1})
				out, err := exec.Call(context.Background(), opByID(t, api, id), map[string]any{"text": "hi"}, "")
				if err != nil {
					t.Fatalf("call: %v", err)
				}
				want := strings.TrimPrefix(id, "Echo_") + ":hi"
				if !strings.Contains(out, want) {
					t.Fatalf("response %q, want it to carry %q — the request message did not arrive", out, want)
				}
			})
		}
	}
}

// TestUnaryEmptyArgsSendsEmptyMessage: a call with no arguments still sends a
// (empty) request message, which a Connect server accepts.
func TestUnaryEmptyArgsSendsEmptyMessage(t *testing.T) {
	api := echoAPI(t)
	srv := connectEchoServer(t)
	exec := runtime.NewHTTPExecutor(srv.URL, nil, runtime.Credential{}, runtime.Options{MaxRetries: 1})
	out, err := exec.Call(context.Background(), opByID(t, api, "Echo_GetEcho"), nil, "")
	if err != nil || !strings.Contains(out, "GetEcho:") {
		t.Fatalf("out=%q err=%v, want the empty-message echo", out, err)
	}
}

// TestUnaryConnectModeSatisfiesStrictServer: a Connect server configured to
// require Connect-Protocol-Version is satisfied in connect mode.
func TestUnaryConnectModeSatisfiesStrictServer(t *testing.T) {
	api := echoAPI(t)
	srv := connectEchoServer(t, connect.WithRequireConnectProtocolHeader())
	exec := runtime.NewHTTPExecutor(srv.URL, nil, runtime.Credential{}, runtime.Options{StreamProtocol: runtime.StreamProtocolConnect, MaxRetries: 1})
	out, err := exec.Call(context.Background(), opByID(t, api, "Echo_GetEcho"), map[string]any{"text": "strict"}, "")
	if err != nil || !strings.Contains(out, "GetEcho:strict") {
		t.Fatalf("out=%q err=%v, want the strict server to accept a connect-mode call", out, err)
	}
}

// TestUnaryGatewayModeAgainstStrictServerNamesTheFix: the same strict server
// refuses a gateway-mode call; the error must point at --grpc-protocol connect.
func TestUnaryGatewayModeAgainstStrictServerNamesTheFix(t *testing.T) {
	api := echoAPI(t)
	srv := connectEchoServer(t, connect.WithRequireConnectProtocolHeader())
	exec := runtime.NewHTTPExecutor(srv.URL, nil, runtime.Credential{}, runtime.Options{MaxRetries: 1})
	_, err := exec.Call(context.Background(), opByID(t, api, "Echo_GetEcho"), map[string]any{"text": "x"}, "")
	if err == nil || !strings.Contains(err.Error(), "--grpc-protocol connect") {
		t.Fatalf("err=%v, want a refusal naming --grpc-protocol connect", err)
	}
}

// TestStreamGatewayModeAgainstConnectServerNamesTheFix: a gateway-mode stream
// against a Connect server gets HTTP 415; the end reason must name the fix.
func TestStreamGatewayModeAgainstConnectServerNamesTheFix(t *testing.T) {
	const proc = "/feed.Feed/Watch"
	mux := http.NewServeMux()
	mux.Handle(proc, connect.NewServerStreamHandler(proc,
		func(context.Context, *connect.Request[structpb.Struct], *connect.ServerStream[structpb.Struct]) error {
			return nil
		}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := runtime.NewStreamManager(runtime.StreamOptions{NoSweeper: true})
	defer m.CloseAll()
	exec := runtime.NewHTTPExecutor(srv.URL, nil, runtime.Credential{}, runtime.Options{})
	id, err := exec.StartServerStream(m, &ir.Operation{ID: "Feed_Watch", Method: "POST", Path: proc}, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	var res *runtime.PollResult
	for i := 0; i < 500; i++ {
		if res, _ = m.Poll(id, 0); res != nil && res.Done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if res == nil || !res.Done || !strings.Contains(res.Reason, "HTTP 415") || !strings.Contains(res.Reason, "--grpc-protocol connect") {
		t.Fatalf("end state %+v, want HTTP 415 naming --grpc-protocol connect", res)
	}
}
