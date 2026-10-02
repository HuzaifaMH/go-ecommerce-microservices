package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"google.golang.org/grpc"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestRunStopsOthersWhenOneFails(t *testing.T) {
	boom := errors.New("boom")
	stopped := make(chan struct{})

	err := Run(context.Background(), discard,
		func(ctx context.Context) error {
			<-ctx.Done()
			close(stopped)
			return ctx.Err()
		},
		func(context.Context) error { return boom },
	)

	if !errors.Is(err, boom) {
		t.Fatalf("Run error = %v, want %v", err, boom)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("sibling task was not cancelled")
	}
}

func TestRunCleanStopOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, discard, func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return")
	}
}

func TestHTTPServerShutsDownGracefully(t *testing.T) {
	lis, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), ReadHeaderTimeout: time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- HTTPServer(srv, lis, time.Second)(ctx) }()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+lis.Addr().String(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("HTTPServer = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func TestGRPCServerStopsOnCancel(t *testing.T) {
	lis, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- GRPCServer(grpc.NewServer(), lis, time.Second)(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("GRPCServer = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("grpc server did not stop")
	}
}
