package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boot.dev/linko/internal/store"
)

type closeFunc func() error

func initializeLogger() (*log.Logger, closeFunc, error) {
	logfile := os.Getenv("LINKO_LOG_FILE")
	if logfile != "" {
		file, err := os.OpenFile(logfile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to open log file: %v", err)
		}
		bufferedFile := bufio.NewWriterSize(file, 8192)
		multiWriter := io.MultiWriter(os.Stderr, bufferedFile)
		return log.New(multiWriter, "", log.LstdFlags), func() error {
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
	return log.New(os.Stderr, "", log.LstdFlags), func() error { return nil }, nil
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
		appLogger.Printf("failed to create store: %v\n", err)
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

	appLogger.Println("Linko is shutting down")
	if err := s.shutdown(shutdownCtx); err != nil {
		appLogger.Printf("failed to shutdown server: %v\n", err)
		return 1
	}
	if serverErr != nil {
		appLogger.Printf("server error: %v\n", serverErr)
		return 1
	}
	return 0
}
