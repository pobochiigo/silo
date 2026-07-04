package connectrpc

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/pobochiigo/silo/endpoint"
)

type DecodeRequestFn[ProtoReq, Req any] func(context.Context, *ProtoReq) (Req, error)

type EncodeResponseFn[Resp, ProtoResp any] func(context.Context, Resp) (*ProtoResp, error)

// Handler represents a ConnectRPC server-side handler for a specific method.
type Handler[ProtoReq, ProtoResp any] func(context.Context, *connect.Request[ProtoReq]) (*connect.Response[ProtoResp], error)

// NewConnectServer constructs a Go-kit style handler from a generic Endpoint.
//
// Decode failures are reported to clients as CodeInvalidArgument and encode
// failures as CodeInternal, unless the returned error already is (or wraps)
// a *connect.Error, which is passed through untouched. Endpoint errors are
// always passed through so business code stays in charge of its own codes.
func NewConnectServer[Req any, Resp any, ProtoReq any, ProtoResp any](
	e endpoint.Endpoint[Req, Resp],
	dec DecodeRequestFn[ProtoReq, Req],
	enc EncodeResponseFn[Resp, ProtoResp],
) Handler[ProtoReq, ProtoResp] {
	return func(ctx context.Context, req *connect.Request[ProtoReq]) (*connect.Response[ProtoResp], error) {
		bizReq, err := dec(ctx, req.Msg)
		if err != nil {
			return nil, asConnectError(connect.CodeInvalidArgument, err)
		}
		bizResp, err := e(ctx, bizReq)
		if err != nil {
			return nil, err
		}
		protoResp, err := enc(ctx, bizResp)
		if err != nil {
			return nil, asConnectError(connect.CodeInternal, err)
		}
		return connect.NewResponse(protoResp), nil
	}
}

// asConnectError wraps err with the given code unless it already carries a
// Connect error code.
func asConnectError(code connect.Code, err error) error {
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		return err
	}
	return connect.NewError(code, err)
}
