package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boot.dev/linko/internal/store"
	pkgerr "github.com/pkg/errors"
)

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if a.Key == "error" {
		err, ok := a.Value.Any().(error)
		if !ok {
			return a
		}
		if stackErr, ok := errors.AsType[stackTracer](err); ok {
			return slog.GroupAttrs("error", slog.Attr{
				Key:   "message",
				Value: slog.StringValue(stackErr.Error()),
			}, slog.Attr{
				Key:   "stack_trace",
				Value: slog.StringValue(fmt.Sprintf("%+v", stackErr.StackTrace())),
			})
		}
	}
	return a
}

type stackTracer interface {
	error
	StackTrace() pkgerr.StackTrace
}

type closeFunc func() error

func initializeLogger() (*slog.Logger, closeFunc, error) {
	logfile := os.Getenv("LINKO_LOG_FILE")

	debugHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level:       slog.LevelDebug,
		ReplaceAttr: replaceAttr,
	})
	if logfile != "" {
		file, err := os.OpenFile(logfile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to open log file: %v", err)
		}
		bufferedFile := bufio.NewWriterSize(file, 8192)
		writer := io.Writer(bufferedFile)
		infoHandler := slog.NewJSONHandler(writer, &slog.HandlerOptions{
			Level:       slog.LevelInfo,
			ReplaceAttr: replaceAttr,
		})
		return slog.New(slog.NewMultiHandler(debugHandler, infoHandler)), func() error {
			err = bufferedFile.Flush()
			if err != nil {
				return fmt.Errorf("buffered file failed to flush")
			}
			err = file.Close()
			if err != nil {
				return fmt.Errorf("file failed to close")
			}
			return nil
		}, nil
	}
	return slog.New(debugHandler), func() error { return nil }, nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, *httpPort, *dataDir)
	cancel()
	os.Exit(status)
}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {
	appLogger, closeLog, err := initializeLogger()

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing logger: %s ", err)
		return 1
	}

	defer func() {
		if err := closeLog(); err != nil {
			fmt.Fprintf(os.Stderr, "error closing logger: %v\n", err)
		}
	}()

	st, err := store.New(dataDir, appLogger)
	if err != nil {
		appLogger.Error(fmt.Sprintf("failed to create store: %v\n", err))
		return 1
	}
	s := newServer(*st, httpPort, appLogger, cancel)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	appLogger.Debug("Linko is shutting down")
	if err := s.shutdown(shutdownCtx); err != nil {
		appLogger.Info(fmt.Sprintf("failed to shutdown server: %v\n", err))
		return 1
	}
	if serverErr != nil {
		appLogger.Info(fmt.Sprintf("server error: %v\n", serverErr))
		return 1
	}
	return 0
}
