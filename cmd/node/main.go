package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/mpeshwe/mini-ipfs/internal/api"
	"github.com/mpeshwe/mini-ipfs/internal/dht"
	"github.com/mpeshwe/mini-ipfs/internal/storage"
	"github.com/mpeshwe/mini-ipfs/internal/util"
)

func main() {
	// Load configuration
	config, err := util.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Initialize structured logger
	if err := util.InitLogger(&config.Logging, config.Node.ID); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	logger := util.GetLogger()

	logger.Info("Starting Mini-IPFS node",
		zap.String("node_id", config.Node.ID),
		zap.String("http_addr", config.Node.HTTPAddr),
		zap.String("dht_addr", config.Node.DHTAddr),
		zap.String("data_dir", config.Node.DataDir),
	)

	// Ensure data directory exists
	if err := os.MkdirAll(config.Node.DataDir, 0o755); err != nil {
		logger.Fatal("Failed to create data directory",
			zap.String("data_dir", config.Node.DataDir),
			zap.Error(err),
		)
	}

	// Initialize storage components
	chunkStore, err := storage.NewChunkStore(config.Node.DataDir, logger)
	if err != nil {
		logger.Fatal("Failed to create chunk store", zap.Error(err))
	}

	chunker := storage.NewChunker(config.Storage.ChunkSize)

	// Log storage initialization
	stats := chunkStore.GetStats()
	logger.Info("Storage initialized",
		zap.Int64("existing_chunks", stats.TotalChunks),
		zap.Int64("total_bytes", stats.TotalBytes),
		zap.Int64("chunk_size", config.Storage.ChunkSize))

	// ----------------------------------------------------------------------------
	// Start HTTP server ASAP so Docker healthcheck (/health) succeeds early
	// ----------------------------------------------------------------------------
	apiServer := api.NewServer(config, logger, chunkStore, chunker)
	serverErrCh := make(chan error, 1)
	go func() {
		if err := apiServer.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrCh <- err
			return
		}
		serverErrCh <- nil
	}()

	// ----------------------------------------------------------------------------
	// Initialize and start DHT asynchronously (bootstrap can take time)
	// ----------------------------------------------------------------------------
	dhtNode, err := dht.NewNode(config, logger)
	if err != nil {
		logger.Fatal("Failed to create DHT node", zap.Error(err))
	}
	go func() {
		if err := dhtNode.Start(); err != nil {
			logger.Fatal("Failed to start DHT node", zap.Error(err))
		}
	}()

	// Signal handling for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	logger.Info("Mini-IPFS node is running - press Ctrl+C to stop",
		zap.String("health_check", fmt.Sprintf("curl http://localhost%s/health", config.Node.HTTPAddr)),
		zap.String("test_storage", fmt.Sprintf("echo 'hello world' | curl -X POST --data-binary @- http://localhost%s/api/v1/chunk", config.Node.HTTPAddr)),
	)

	// Wait for either server error or shutdown signal
	select {
	case err := <-serverErrCh:
		if err != nil {
			logger.Error("HTTP server error", zap.Error(err))
		}
	case sig := <-sigCh:
		logger.Info("Received shutdown signal", zap.String("signal", sig.String()))
	}

	// Graceful shutdown
	logger.Info("Shutting down gracefully…")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := apiServer.Stop(ctx); err != nil {
		logger.Error("Error stopping HTTP server", zap.Error(err))
	}
	if err := dhtNode.Stop(); err != nil {
		logger.Error("Error stopping DHT node", zap.Error(err))
	}

	// Log final storage stats
	finalStats := chunkStore.GetStats()
	logger.Info("Final storage statistics",
		zap.Int64("total_chunks", finalStats.TotalChunks),
		zap.Int64("total_bytes", finalStats.TotalBytes))

	logger.Info("Shutdown complete")
}
