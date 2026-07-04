package connectrpc

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
)

type dummyReq struct {
	Name string
}

type dummyResp struct {
	Greeting string
}

type dummyProtoReq struct {
	ProtoName string
}

type dummyProtoResp struct {
	ProtoGreeting string
}

func TestNewConnectServer(t *testing.T) {
	t.Run("succeeded", func(t *testing.T) {
		epCalled := false
		decCalled := false
		encCalled := false

		endpoint := func(ctx context.Context, req dummyReq) (dummyResp, error) {
			epCalled = true
			assert.Equal(t, "Alice", req.Name)
			return dummyResp{Greeting: "Hello Alice"}, nil
		}

		dec := func(ctx context.Context, pr *dummyProtoReq) (dummyReq, error) {
			decCalled = true
			assert.Equal(t, "Alice", pr.ProtoName)
			return dummyReq{Name: pr.ProtoName}, nil
		}

		enc := func(ctx context.Context, dr dummyResp) (*dummyProtoResp, error) {
			encCalled = true
			assert.Equal(t, "Hello Alice", dr.Greeting)
			return &dummyProtoResp{ProtoGreeting: dr.Greeting}, nil
		}

		handler := NewConnectServer(endpoint, dec, enc)
		ctx := context.Background()
		req := connect.NewRequest(&dummyProtoReq{ProtoName: "Alice"})

		resp, err := handler(ctx, req)
		assert.NoError(t, err)
		assert.NotNil(t, resp)
		assert.Equal(t, "Hello Alice", resp.Msg.ProtoGreeting)
		assert.True(t, epCalled)
		assert.True(t, decCalled)
		assert.True(t, encCalled)
	})

	t.Run("decoder error", func(t *testing.T) {
		expectedErr := errors.New("decoder error")
		endpoint := func(ctx context.Context, req dummyReq) (dummyResp, error) {
			t.Fatal("endpoint should not be called")
			return dummyResp{}, nil
		}

		dec := func(ctx context.Context, pr *dummyProtoReq) (dummyReq, error) {
			return dummyReq{}, expectedErr
		}

		enc := func(ctx context.Context, dr dummyResp) (*dummyProtoResp, error) {
			t.Fatal("encoder should not be called")
			return nil, nil
		}

		handler := NewConnectServer(endpoint, dec, enc)
		ctx := context.Background()
		req := connect.NewRequest(&dummyProtoReq{ProtoName: "Alice"})

		resp, err := handler(ctx, req)
		assert.Equal(t, expectedErr, err)
		assert.Nil(t, resp)
	})

	t.Run("endpoint error", func(t *testing.T) {
		expectedErr := errors.New("endpoint error")
		endpoint := func(ctx context.Context, req dummyReq) (dummyResp, error) {
			return dummyResp{}, expectedErr
		}

		dec := func(ctx context.Context, pr *dummyProtoReq) (dummyReq, error) {
			return dummyReq{Name: pr.ProtoName}, nil
		}

		enc := func(ctx context.Context, dr dummyResp) (*dummyProtoResp, error) {
			t.Fatal("encoder should not be called")
			return nil, nil
		}

		handler := NewConnectServer(endpoint, dec, enc)
		ctx := context.Background()
		req := connect.NewRequest(&dummyProtoReq{ProtoName: "Alice"})

		resp, err := handler(ctx, req)
		assert.Equal(t, expectedErr, err)
		assert.Nil(t, resp)
	})

	t.Run("encoder error", func(t *testing.T) {
		expectedErr := errors.New("encoder error")
		endpoint := func(ctx context.Context, req dummyReq) (dummyResp, error) {
			return dummyResp{Greeting: "Hello Alice"}, nil
		}

		dec := func(ctx context.Context, pr *dummyProtoReq) (dummyReq, error) {
			return dummyReq{Name: pr.ProtoName}, nil
		}

		enc := func(ctx context.Context, dr dummyResp) (*dummyProtoResp, error) {
			return nil, expectedErr
		}

		handler := NewConnectServer(endpoint, dec, enc)
		ctx := context.Background()
		req := connect.NewRequest(&dummyProtoReq{ProtoName: "Alice"})

		resp, err := handler(ctx, req)
		assert.Equal(t, expectedErr, err)
		assert.Nil(t, resp)
	})
}

func TestNewConnectClient(t *testing.T) {
	t.Run("succeeded", func(t *testing.T) {
		callCalled := false
		encCalled := false
		decCalled := false

		call := func(ctx context.Context, req *connect.Request[dummyProtoReq]) (*connect.Response[dummyProtoResp], error) {
			callCalled = true
			assert.Equal(t, "Bob", req.Msg.ProtoName)
			return connect.NewResponse(&dummyProtoResp{ProtoGreeting: "Hi Bob"}), nil
		}

		enc := func(ctx context.Context, req dummyReq) (*dummyProtoReq, error) {
			encCalled = true
			assert.Equal(t, "Bob", req.Name)
			return &dummyProtoReq{ProtoName: req.Name}, nil
		}

		dec := func(ctx context.Context, pr *dummyProtoResp) (dummyResp, error) {
			decCalled = true
			assert.Equal(t, "Hi Bob", pr.ProtoGreeting)
			return dummyResp{Greeting: pr.ProtoGreeting}, nil
		}

		clientEndpoint := NewConnectClient(call, enc, dec)
		ctx := context.Background()

		resp, err := clientEndpoint(ctx, dummyReq{Name: "Bob"})
		assert.NoError(t, err)
		assert.Equal(t, "Hi Bob", resp.Greeting)
		assert.True(t, callCalled)
		assert.True(t, encCalled)
		assert.True(t, decCalled)
	})

	t.Run("encoder error", func(t *testing.T) {
		expectedErr := errors.New("encoder error")
		call := func(ctx context.Context, req *connect.Request[dummyProtoReq]) (*connect.Response[dummyProtoResp], error) {
			t.Fatal("client call should not be invoked")
			return nil, nil
		}

		enc := func(ctx context.Context, req dummyReq) (*dummyProtoReq, error) {
			return nil, expectedErr
		}

		dec := func(ctx context.Context, pr *dummyProtoResp) (dummyResp, error) {
			t.Fatal("decoder should not be invoked")
			return dummyResp{}, nil
		}

		clientEndpoint := NewConnectClient(call, enc, dec)
		ctx := context.Background()

		resp, err := clientEndpoint(ctx, dummyReq{Name: "Bob"})
		assert.Equal(t, expectedErr, err)
		assert.Equal(t, dummyResp{}, resp)
	})

	t.Run("call error", func(t *testing.T) {
		expectedErr := errors.New("call error")
		call := func(ctx context.Context, req *connect.Request[dummyProtoReq]) (*connect.Response[dummyProtoResp], error) {
			return nil, expectedErr
		}

		enc := func(ctx context.Context, req dummyReq) (*dummyProtoReq, error) {
			return &dummyProtoReq{ProtoName: req.Name}, nil
		}

		dec := func(ctx context.Context, pr *dummyProtoResp) (dummyResp, error) {
			t.Fatal("decoder should not be invoked")
			return dummyResp{}, nil
		}

		clientEndpoint := NewConnectClient(call, enc, dec)
		ctx := context.Background()

		resp, err := clientEndpoint(ctx, dummyReq{Name: "Bob"})
		assert.Equal(t, expectedErr, err)
		assert.Equal(t, dummyResp{}, resp)
	})

	t.Run("decoder error", func(t *testing.T) {
		expectedErr := errors.New("decoder error")
		call := func(ctx context.Context, req *connect.Request[dummyProtoReq]) (*connect.Response[dummyProtoResp], error) {
			return connect.NewResponse(&dummyProtoResp{ProtoGreeting: "Hi Bob"}), nil
		}

		enc := func(ctx context.Context, req dummyReq) (*dummyProtoReq, error) {
			return &dummyProtoReq{ProtoName: req.Name}, nil
		}

		dec := func(ctx context.Context, pr *dummyProtoResp) (dummyResp, error) {
			return dummyResp{}, expectedErr
		}

		clientEndpoint := NewConnectClient(call, enc, dec)
		ctx := context.Background()

		resp, err := clientEndpoint(ctx, dummyReq{Name: "Bob"})
		assert.Equal(t, expectedErr, err)
		assert.Equal(t, dummyResp{}, resp)
	})
}
