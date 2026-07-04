package connectrpc

import (
	"context"

	"connectrpc.com/connect"
	"github.com/pobochiigo/silo/endpoint"
)

type CallFn[ProtoReq, ProtoResp any] func(context.Context, *connect.Request[ProtoReq]) (*connect.Response[ProtoResp], error)

type EncodeRequestFn[Req, ProtoReq any] func(context.Context, Req) (*ProtoReq, error)

type DecodeResponseFn[ProtoResp, Resp any] func(context.Context, *ProtoResp) (Resp, error)

// NewConnectClient constructs a Go-kit style Endpoint around a ConnectRPC method.
func NewConnectClient[Req any, Resp any, ProtoReq any, ProtoResp any](
	call CallFn[ProtoReq, ProtoResp],
	enc EncodeRequestFn[Req, ProtoReq],
	dec DecodeResponseFn[ProtoResp, Resp],
) endpoint.Endpoint[Req, Resp] {
	return func(ctx context.Context, request Req) (Resp, error) {
		var empty Resp
		protoReq, err := enc(ctx, request)
		if err != nil {
			return empty, err
		}

		resp, err := call(ctx, connect.NewRequest(protoReq))
		if err != nil {
			return empty, err
		}

		return dec(ctx, resp.Msg)
	}
}
