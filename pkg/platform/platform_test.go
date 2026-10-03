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

func TestOpenPostgresRejectsBadURLImmediately(t *testing.T) {
	start := time.Now()
	if _, err := OpenPostgres(context.Background(), "not a url"); err == nil {
		t.Fatal("expected error for an invalid connection string")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %v: a malformed connection string must not be retried", time.Since(start))
	}
}

func TestOpenPostgresKeepsTryingWhileTheDatabaseIsDownThenGivesUp(t *testing.T) {
	// Nothing listens on this port.
	lis, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()

	start := time.Now()
	_, err = openPostgres(context.Background(), "postgres://u:p@"+addr+"/db?sslmode=disable&connect_timeout=1", 700*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error when the database never comes up")
	}
	if elapsed < 300*time.Millisecond {
		t.Errorf("gave up after %v; it should keep trying until its wait is used up", elapsed)
	}
	if elapsed > 10*time.Second {
		t.Errorf("took %v; it must stop waiting", elapsed)
	}
}
