// Package runner starts a service's long-running components and shuts them
// down together and gracefully.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
)

// Task is a long-running component. It must return when ctx is cancelled.
// Returning nil or context.Canceled is treated as a clean stop.
type Task func(ctx context.Context) error

// Run executes all tasks concurrently until ctx is cancelled, SIGINT or
// SIGTERM is received, or a task fails. When any task stops, the context
// given to the others is cancelled so they can shut down. Run returns the
// first non-cancellation error.
func Run(ctx context.Context, log *slog.Logger, tasks ...Task) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	g, ctx := errgroup.WithContext(ctx)
	for _, t := range tasks {
		g.Go(func() error {
			err := t(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		})
	}

	<-ctx.Done()
	log.Info("shutting down")
	return g.Wait()
}

// HTTPServer serves srv on lis and shuts it down gracefully (waiting up to
// timeout for in-flight requests) when ctx is cancelled.
func HTTPServer(srv *http.Server, lis net.Listener, timeout time.Duration) Task {
	return func(ctx context.Context) error {
		errCh := make(chan error, 1)
		go func() { errCh <- srv.Serve(lis) }()

		select {
		case err := <-errCh:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return fmt.Errorf("http server: %w", err)
		case <-ctx.Done():
		}

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}
		return nil
	}
}

// GRPCServer serves srv on lis and stops it gracefully when ctx is cancelled,
// forcing a stop if in-flight calls take longer than timeout.
func GRPCServer(srv *grpc.Server, lis net.Listener, timeout time.Duration) Task {
	return func(ctx context.Context) error {
		errCh := make(chan error, 1)
		go func() { errCh <- srv.Serve(lis) }()

		select {
		case err := <-errCh:
			if err != nil {
				return fmt.Errorf("grpc server: %w", err)
			}
			return nil
		case <-ctx.Done():
		}

		done := make(chan struct{})
		go func() {
			srv.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(timeout):
			srv.Stop()
		}
		return nil
	}
}
