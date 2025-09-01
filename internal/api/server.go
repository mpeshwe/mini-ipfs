package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	"go.uber.org/zap"

	"github.com/mpeshwe/mini-ipfs/internal/dht"
	"github.com/mpeshwe/mini-ipfs/internal/storage"
	"github.com/mpeshwe/mini-ipfs/internal/util"
)

// Server represents the HTTP API server
type Server struct {
	router     *mux.Router
	httpServer *http.Server
	logger     *zap.Logger
	config     *util.Config
	chunkStore *storage.ChunkStore
	chunker    *storage.Chunker
	dhtNode    dht.Node
}

// HealthResponse represents the health check response
type HealthResponse struct {
	Status    string             `json:"status"`
	NodeID    string             `json:"node_id"`
	Version   string             `json:"version"`
	Timestamp time.Time          `json:"timestamp"`
	Services  map[string]string  `json:"services"`
	Storage   storage.StoreStats `json:"storage"`
}

// NewServer creates a new HTTP API server
func NewServer(config *util.Config, logger *zap.Logger, chunkStore *storage.ChunkStore, chunker *storage.Chunker, dhtNode dht.Node) *Server {
	s := &Server{
		router:     mux.NewRouter(),
		logger:     logger.With(zap.String("component", "api")),
		config:     config,
		chunkStore: chunkStore,
		chunker:    chunker,
		dhtNode:    dhtNode,
	}

	s.setupRoutes()
	s.setupMiddleware()

	return s
}

// setupRoutes configures all HTTP routes
func (s *Server) setupRoutes() {
	// Health endpoints
	s.router.HandleFunc("/health", s.handleHealth).Methods("GET")
	s.router.HandleFunc("/", s.handleRoot).Methods("GET")

	// API v1 routes
	api := s.router.PathPrefix("/api/v1").Subrouter()
	api.HandleFunc("/status", s.handleStatus).Methods("GET")

	api.HandleFunc("/chunk", s.handlePostChunk).Methods("POST")
	api.HandleFunc("/chunk/{hash}", s.handleGetChunk).Methods("GET")

	// TODO Episode 6: Add file endpoints
	// api.HandleFunc("/file", s.handlePostFile).Methods("POST")
	// api.HandleFunc("/file/{hash}", s.handleGetFile).Methods("GET")
}

// ChunkResponse represents a chunk storage response
type ChunkResponse struct {
	Hash string `json:"hash"`
	Size int    `json:"size"`
}

// handlePostChunk stores a chunk and returns its hash
func (s *Server) handlePostChunk(w http.ResponseWriter, r *http.Request) {
	// Limit request size to prevent abuse
	maxSize := int64(10 * 1024 * 1024) // 10MB max
	if r.ContentLength > maxSize {
		http.Error(w, "Request too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Read request body
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSize))
	if err != nil {
		s.logger.Error("Failed to read request body", zap.Error(err))
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if len(body) == 0 {
		http.Error(w, "Empty request body", http.StatusBadRequest)
		return
	}

	// Store the chunk locally
	hash, err := s.chunkStore.PutChunk(body)
	if err != nil {
		s.logger.Error("Failed to store chunk", zap.Error(err))
		http.Error(w, "Failed to store chunk", http.StatusInternalServerError)
		return
	}

	// Build advertised HTTP address (reachable by peers)
	httpAddr, err := s.advertisedHTTPAddr()
	if err != nil {
		s.logger.Warn("no advertised HTTP addr", zap.Error(err))
	}

	// Announce to DHT that we provide this chunk
	nodeInfo := &dht.NodeInfo{
		Id:   s.dhtNode.ID(),
		Addr: s.dhtNode.Address(), // DHT contact address (do NOT overwrite with HTTP)
		HTTP: httpAddr,            // Optional: explicit HTTP address for fetching
	}

	// Announce provider in background (don't block HTTP response)
	go func() {
		if err := s.dhtNode.StoreProvider(hash, nodeInfo); err != nil {
			s.logger.Warn("Failed to announce provider to DHT",
				zap.String("hash", hex.EncodeToString(hash)[:16]),
				zap.Error(err))
		} else {
			s.logger.Debug("Announced chunk provider to DHT",
				zap.String("hash", hex.EncodeToString(hash)[:16]))
		}
	}()

	// Return response immediately
	response := ChunkResponse{
		Hash: hex.EncodeToString(hash),
		Size: len(body),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(response)

	s.logger.Info("Stored chunk",
		zap.String("hash", hex.EncodeToString(hash)[:16]),
		zap.Int("size", len(body)))
}

// handleGetChunk retrieves a chunk by its hash (local or remote)
func (s *Server) handleGetChunk(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	hashHex := vars["hash"]

	if len(hashHex) != 64 {
		http.Error(w, "Invalid hash format", http.StatusBadRequest)
		return
	}

	// Decode hex hash
	hash, err := hex.DecodeString(hashHex)
	if err != nil {
		http.Error(w, "Invalid hash format", http.StatusBadRequest)
		return
	}

	// Try to retrieve chunk locally first
	data, err := s.chunkStore.GetChunk(hash)
	if err == nil {
		// Found locally - return immediately
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)

		s.logger.Debug("Retrieved chunk locally",
			zap.String("hash", hashHex[:16]),
			zap.Int("size", len(data)))
		return
	}

	// Chunk not found locally - check if remote fetching is enabled
	enableRemote := r.URL.Query().Get("remote")
	if enableRemote != "1" && enableRemote != "true" {
		http.Error(w, "Chunk not found locally (use ?remote=1 to search network)", http.StatusNotFound)
		return
	}

	s.logger.Info("Chunk not found locally, searching network",
		zap.String("hash", hashHex[:16]))

	// Fetch chunk from network
	data, err = s.fetchChunkFromNetwork(hash, hashHex)
	if err != nil {
		s.logger.Warn("Failed to fetch chunk from network",
			zap.String("hash", hashHex[:16]),
			zap.Error(err))
		http.Error(w, "Chunk not found in network", http.StatusNotFound)
		return
	}

	// Store chunk locally (caching)
	if _, storeErr := s.chunkStore.PutChunk(data); storeErr != nil {
		s.logger.Warn("Failed to cache retrieved chunk",
			zap.String("hash", hashHex[:16]),
			zap.Error(storeErr))
	}

	// Return chunk data
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)

	s.logger.Info("Retrieved chunk from network",
		zap.String("hash", hashHex[:16]),
		zap.Int("size", len(data)))
}

// fetchChunkFromNetwork attempts to retrieve a chunk from other nodes
func (s *Server) fetchChunkFromNetwork(hash []byte, hashHex string) ([]byte, error) {
	// Find providers for this chunk
	providers, err := s.dhtNode.FindProviders(hash)
	if err != nil {
		return nil, fmt.Errorf("failed to find providers: %w", err)
	}

	if len(providers) == 0 {
		return nil, fmt.Errorf("no providers found for chunk")
	}

	s.logger.Debug("Found chunk providers",
		zap.String("hash", hashHex[:16]),
		zap.Int("provider_count", len(providers)))

	// Try each provider until successful
	for _, provider := range providers {
		// Prefer explicit HTTP address if provider announced it
		if provider.HTTP != "" {
			data, err := s.fetchChunkFromHTTP(provider.HTTP, hashHex)
			if err != nil {
				s.logger.Debug("Failed over HTTP from provider",
					zap.String("http", provider.HTTP),
					zap.Error(err))
				continue
			}
			// Verify chunk integrity
			actualHash := sha256.Sum256(data)
			if !bytes.Equal(hash, actualHash[:]) {
				s.logger.Warn("Chunk integrity check failed from provider (HTTP)",
					zap.String("http", provider.HTTP))
				continue
			}
			return data, nil
		}

		// Fallback: derive HTTP from DHT address host
		host, _, err := net.SplitHostPort(provider.Addr)
		if err != nil || host == "" {
			s.logger.Debug("bad provider addr", zap.String("addr", provider.Addr), zap.Error(err))
			continue
		}
		httpAddr := net.JoinHostPort(host, "8080")
		data, err := s.fetchChunkFromHTTP(httpAddr, hashHex)
		if err != nil {
			s.logger.Debug("Failed to fetch from provider",
				zap.String("dht_addr", provider.Addr),
				zap.String("http", httpAddr),
				zap.Error(err))
			continue
		}

		// Verify chunk integrity
		actualHash := sha256.Sum256(data)
		if !bytes.Equal(hash, actualHash[:]) {
			s.logger.Warn("Chunk integrity check failed from provider",
				zap.String("provider", provider.Addr))
			continue
		}

		return data, nil
	}

	return nil, fmt.Errorf("failed to fetch chunk from any provider")
}

// fetchChunkFromHTTP retrieves a chunk from a specific HTTP address (host:port)
func (s *Server) fetchChunkFromHTTP(httpAddr, hashHex string) ([]byte, error) {
	url := fmt.Sprintf("http://%s/api/v1/chunk/%s", httpAddr, hashHex)

	s.logger.Debug("Fetching chunk from provider",
		zap.String("http_url", url),
		zap.String("hash", hashHex[:16]))

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}
	return data, nil
}

// setupMiddleware adds logging and CORS middleware
func (s *Server) setupMiddleware() {
	s.router.Use(s.loggingMiddleware)
	s.router.Use(s.corsMiddleware)
}

// Start starts the HTTP server
func (s *Server) Start() error {
	s.httpServer = &http.Server{
		Addr:         s.config.Node.HTTPAddr,
		Handler:      s.router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	s.logger.Info("Starting HTTP server",
		zap.String("addr", s.config.Node.HTTPAddr),
	)

	return s.httpServer.ListenAndServe()
}

// Stop gracefully stops the HTTP server
func (s *Server) Stop(ctx context.Context) error {
	if s.httpServer == nil {
		return nil
	}

	s.logger.Info("Stopping HTTP server")
	return s.httpServer.Shutdown(ctx)
}

// HTTP Handlers

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	response := HealthResponse{
		Status:    "healthy",
		NodeID:    s.config.Node.ID,
		Version:   "0.2.0-dev",
		Timestamp: time.Now(),
		Services: map[string]string{
			"http":    "running",
			"dht":     "running",
			"storage": "ready",
		},
		Storage: s.chunkStore.GetStats(),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	response := map[string]interface{}{
		"service": "mini-ipfs",
		"version": "0.2.0-dev",
		"node_id": s.config.Node.ID,
		"message": "Mini-IPFS node is running",
		"endpoints": map[string]string{
			"health":     "/health",
			"status":     "/api/v1/status",
			"post_chunk": "POST /api/v1/chunk",
			"get_chunk":  "GET /api/v1/chunk/{hash}",
		},
	}
	_ = json.NewEncoder(w).Encode(response)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	stats := s.chunkStore.GetStats()
	response := map[string]interface{}{
		"node_id":    s.config.Node.ID,
		"http_addr":  s.config.Node.HTTPAddr,
		"dht_addr":   s.config.Node.DHTAddr,
		"data_dir":   s.config.Node.DataDir,
		"dht_status": "running",
		"storage": map[string]interface{}{
			"chunk_size":         s.config.Storage.ChunkSize,
			"max_storage":        s.config.Storage.MaxStorageBytes,
			"replication_factor": s.config.Storage.ReplicationFactor,
			"current_chunks":     stats.TotalChunks,
			"current_bytes":      stats.TotalBytes,
		},
	}

	_ = json.NewEncoder(w).Encode(response)
}

// Middleware

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Wrap response writer to capture status code
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		s.logger.Info("HTTP request",
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.String("remote_addr", r.RemoteAddr),
			zap.Int("status_code", wrapped.statusCode),
			zap.Duration("duration", time.Since(start)),
		)
	})
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// advertisedHTTPAddr builds a reachable HTTP address to advertise to peers.
func (s *Server) advertisedHTTPAddr() (string, error) {
	if s.config.Node.AdvertiseHost == "" {
		return "", fmt.Errorf("node.advertise_host not set")
	}
	_, port, err := net.SplitHostPort(s.config.Node.HTTPAddr)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(s.config.Node.AdvertiseHost, port), nil
}
