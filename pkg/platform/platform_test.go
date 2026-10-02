package platform

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"google.golang.org/grpc"
)

func TestServeFailsFastWhenPortIsTaken(t *testing.T) {
	ctx := context.Background()
	taken, err := new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	addr := taken.Addr().String()

	if _, err := ServeHTTP(ctx, addr, http.NewServeMux(), time.Second); err == nil {
		t.Error("ServeHTTP on a used port must fail at start-up")
	}
	if _, err := ServeGRPC(ctx, addr, grpc.NewServer(), time.Second); err == nil {
		t.Error("ServeGRPC on a used port must fail at start-up")
	}
}

func TestServeHTTPStopsOnCancel(t *testing.T) {
	task, err := ServeHTTP(context.Background(), "127.0.0.1:0", http.NewServeMux(), time.Second)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- task(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("task = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop")
	}
}

func TestOpenPostgresRejectsBadURL(t *testing.T) {
	if _, err := OpenPostgres(context.Background(), "not a url"); err == nil {
		t.Fatal("expected error for an invalid connection string")
	}
}
