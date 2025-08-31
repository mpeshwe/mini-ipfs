package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"go.uber.org/zap"

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
func NewServer(config *util.Config, logger *zap.Logger, chunkStore *storage.ChunkStore, chunker *storage.Chunker) *Server {
	s := &Server{
		router:     mux.NewRouter(),
		logger:     logger.With(zap.String("component", "api")),
		config:     config,
		chunkStore: chunkStore,
		chunker:    chunker,
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

	// TODO Episode 4: Add chunk endpoints
	// api.HandleFunc("/chunk", s.handlePostChunk).Methods("POST")
	// api.HandleFunc("/chunk/{hash}", s.handleGetChunk).Methods("GET")

	// TODO Episode 6: Add file endpoints
	// api.HandleFunc("/file", s.handlePostFile).Methods("POST")
	// api.HandleFunc("/file/{hash}", s.handleGetFile).Methods("GET")
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
	json.NewEncoder(w).Encode(response)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	response := map[string]interface{}{
		"service": "mini-ipfs",
		"version": "0.2.0-dev",
		"node_id": s.config.Node.ID,
		"message": "Mini-IPFS node is running",
		"endpoints": map[string]string{
			"health": "/health",
			"status": "/api/v1/status",
		},
	}
	json.NewEncoder(w).Encode(response)
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

	json.NewEncoder(w).Encode(response)
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
