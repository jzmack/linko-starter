package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boot.dev/linko/internal/build"
	"boot.dev/linko/internal/linkoerr"
	"boot.dev/linko/internal/store"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	pkgerr "github.com/pkg/errors"
	"gopkg.in/natefinch/lumberjack.v2"
)

type multiError interface {
	error
	Unwrap() []error
}

func errorAttrs(err error) []slog.Attr {
	errAttribs := linkoerr.Attrs(err)
	base := []slog.Attr{
		slog.String("message", err.Error()),
	}
	combined := append(base, errAttribs...)
	if stackErr, ok := errors.AsType[stackTracer](err); ok {
		stackTraceAttr := slog.Attr{
			Key:   "stack_trace",
			Value: slog.StringValue(fmt.Sprintf("%+v", stackErr.StackTrace())),
		}
		combined = append(combined, stackTraceAttr)
	}
	return combined
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if a.Key == "error" {
		err, ok := a.Value.Any().(error)
		if !ok {
			return a
		}
		if multiErr, ok := errors.AsType[multiError](err); ok {
			errs := multiErr.Unwrap()
			errGroups := []slog.Attr{}
			for i, er := range errs {
				errorN := slog.GroupAttrs(fmt.Sprintf("error_%d", i+1), errorAttrs(er)...)
				errGroups = append(errGroups, errorN)
			}
			return slog.GroupAttrs("errors", errGroups...)
		}

		errAttrs := errorAttrs(err)
		return slog.GroupAttrs("error", errAttrs...)
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

	noColor := false
	terminal := isatty.IsTerminal(os.Stderr.Fd())
	cygwinTerminal := isatty.IsCygwinTerminal(os.Stderr.Fd())
	if terminal == false && cygwinTerminal == false {
		noColor = true
	}
	debugHandler := tint.NewHandler(os.Stderr, &tint.Options{
		Level:       slog.LevelDebug,
		ReplaceAttr: replaceAttr,
		NoColor:     noColor,
	})

	if logfile != "" {

		logger := &lumberjack.Logger{
			Filename:   logfile,
			MaxSize:    1,
			MaxAge:     28,
			MaxBackups: 10,
			LocalTime:  false,
			Compress:   true,
		}
		infoHandler := slog.NewJSONHandler(logger, &slog.HandlerOptions{
			Level:       slog.LevelInfo,
			ReplaceAttr: replaceAttr,
		})
		return slog.New(slog.NewMultiHandler(debugHandler, infoHandler)), func() error {
			return logger.Close()
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
	env := os.Getenv("ENV")
	hostname, _ := os.Hostname()
	appLogger = appLogger.With(
		slog.String("git_sha", build.GitSHA),
		slog.String("build_time", build.BuildTime),
		slog.String("env", env),
		slog.String("hostname", hostname),
	)

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
