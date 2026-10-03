// Package clients creates the gRPC connections the gateway uses to reach the
// backend services.
package clients

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/HuzaifaMH/go-ecommerce-microservices/services/gateway/internal/requestid"
)

// Dial returns a connection to addr. It connects lazily, so the gateway starts
// even when a backend is not up yet.
//
// Every call gets a deadline (the caller's if it set one, otherwise
// defaultTimeout) and carries the request ID as metadata, so backend logs can
// be matched to the HTTP request that caused them.
func Dial(addr string, defaultTimeout time.Duration, extra ...grpc.DialOption) (*grpc.ClientConn, error) {
	opts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(deadline(defaultTimeout), propagateRequestID),
	}, extra...)

	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("create client for %s: %w", addr, err)
	}
	return conn, nil
}

// deadline applies defaultTimeout to calls that have no deadline of their own.
func deadline(defaultTimeout time.Duration) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if _, ok := ctx.Deadline(); !ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
			defer cancel()
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// propagateRequestID forwards the request ID to the backend.
func propagateRequestID(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	if id := requestid.From(ctx); id != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, requestid.MetadataKey, id)
	}
	return invoker(ctx, method, req, reply, cc, opts...)
}
