package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		log.Printf("ERROR %v", err)
		os.Exit(1)
	}
}

func run() error {
	logger := log.New(os.Stdout, "", log.LstdFlags)
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	api := NewAPI(cfg, nil, logger)

	startupCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	checkCtx, cancelCheck := context.WithTimeout(startupCtx, 15*time.Second)
	api.CheckShares(checkCtx)
	cancelCheck()

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- server.ListenAndServe()
	}()
	logger.Printf("INFO listening on %s", cfg.ListenAddr)

	select {
	case err := <-serveErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-startupCtx.Done():
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		err := <-serveErrors
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
