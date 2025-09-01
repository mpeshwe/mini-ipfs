package api

import (
	"bytes"
	"context"
	"crypto/sha256"
    "errors"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
    "sync"

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
    sse        *sseHub
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
        sse:        newSSEHub(),
    }

    s.setupRoutes()
    s.setupMiddleware()

    // Wire DHT events into SSE so the UI can see inter-node activity
    dhtNode.SetEventSink(func(event string, payload map[string]interface{}) {
        s.emitEvent(event, payload)
    })

    return s
}

// setupRoutes configures all HTTP routes
func (s *Server) setupRoutes() {
    // Health endpoints (accept GET and HEAD to satisfy container healthchecks)
    s.router.HandleFunc("/health", s.handleHealth).Methods("GET", "HEAD")
	s.router.HandleFunc("/", s.handleRoot).Methods("GET")

	// API v1 routes
	api := s.router.PathPrefix("/api/v1").Subrouter()
	api.HandleFunc("/status", s.handleStatus).Methods("GET")

    api.HandleFunc("/chunk", s.handlePostChunk).Methods("POST", "OPTIONS")
    api.HandleFunc("/chunk/{hash}", s.handleGetChunk).Methods("GET")
    api.HandleFunc("/chunk/{hash}", s.handleDeleteChunk).Methods("DELETE", "OPTIONS")

	// File endpoints
    api.HandleFunc("/file", s.handlePostFile).Methods("POST", "OPTIONS")
    api.HandleFunc("/file/{hash}", s.handleGetFile).Methods("GET")
    api.HandleFunc("/file/{hash}", s.handleDeleteFile).Methods("DELETE", "OPTIONS")
    api.HandleFunc("/file/stream", s.handlePostFileStream).Methods("POST", "OPTIONS")

    // SSE events
    api.HandleFunc("/events", s.handleSSE).Methods("GET")
}

// ChunkResponse represents a chunk storage response
type ChunkResponse struct {
    Hash string `json:"hash"`
    Size int    `json:"size"`
}

// handleDeleteChunk deletes a single chunk by hash (no manifest needed).
// If `global=1`, it also propagates deletion to other providers via DHT,
// guarded by `propagate=0` to prevent loops.
func (s *Server) handleDeleteChunk(w http.ResponseWriter, r *http.Request) {
    vars := mux.Vars(r)
    hashHex := vars["hash"]
    if len(hashHex) != 64 {
        http.Error(w, "Invalid hash format", http.StatusBadRequest)
        return
    }
    hash, err := hex.DecodeString(hashHex)
    if err != nil {
        http.Error(w, "Invalid hash format", http.StatusBadRequest)
        return
    }

    // Delete locally if present
    _ = s.chunkStore.DeleteChunk(hash)

    // Optionally propagate to other providers
    wantGlobal := r.URL.Query().Get("global") == "1" || r.URL.Query().Get("global") == "true"
    propagate := r.URL.Query().Get("propagate")
    doPropagate := propagate != "0" && propagate != "false"

    contacted, deletedOK := 0, 0
    if wantGlobal && doPropagate {
        if providers, perr := s.dhtNode.FindProviders(hash); perr == nil {
            client := &http.Client{Timeout: 5 * time.Second}
            for _, p := range providers {
                if len(p.Id) > 0 && len(s.dhtNode.ID()) > 0 && bytes.Equal(p.Id, s.dhtNode.ID()) {
                    continue
                }
                var httpAddr string
                if p.HTTP != "" { httpAddr = p.HTTP } else {
                    host, _, herr := net.SplitHostPort(p.Addr)
                    if herr != nil || host == "" { continue }
                    httpAddr = net.JoinHostPort(host, "8080")
                }
                url := fmt.Sprintf("http://%s/api/v1/chunk/%s?propagate=0", httpAddr, hashHex)
                req, _ := http.NewRequest("DELETE", url, nil)
                contacted++
                if resp, rerr := client.Do(req); rerr == nil {
                    if resp.StatusCode >= 200 && resp.StatusCode < 300 { deletedOK++ }
                    if resp.Body != nil { io.Copy(io.Discard, resp.Body); resp.Body.Close() }
                }
            }
        }
    }

    resp := map[string]interface{}{
        "status":              "deleted",
        "hash":                hashHex,
        "providers_contacted": contacted,
        "providers_deleted":   deletedOK,
    }
    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(resp)

    s.emitEvent("chunk_deleted", map[string]interface{}{
        "hash":                hashHex,
        "providers_contacted": contacted,
        "providers_deleted":   deletedOK,
    })
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

	// Emit SSE event
	s.emitEvent("chunk_stored", map[string]interface{}{
		"hash": hex.EncodeToString(hash),
		"size": len(body),
	})
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
		// Detect content type for better client handling (audio/video/images)
		if len(data) > 0 {
			sniff := data
			if len(sniff) > 512 {
				sniff = sniff[:512]
			}
			w.Header().Set("Content-Type", http.DetectContentType(sniff))
		} else {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
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
    } else {
        // Emit cache event for UI visibility
        s.emitEvent("chunk_cached", map[string]interface{}{
            "hash": hashHex,
        })
        // Become a provider for this chunk (so others can find us and for deletes)
        if httpAddr, err := s.advertisedHTTPAddr(); err == nil {
            go func(hh []byte) {
                _ = s.dhtNode.StoreProvider(hh, &dht.NodeInfo{Id: s.dhtNode.ID(), Addr: s.dhtNode.Address(), HTTP: httpAddr})
            }(hash)
        }
    }

	// Return chunk data
	if len(data) > 0 {
		sniff := data
		if len(sniff) > 512 {
			sniff = sniff[:512]
		}
		w.Header().Set("Content-Type", http.DetectContentType(sniff))
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)

	s.logger.Info("Retrieved chunk from network",
		zap.String("hash", hashHex[:16]),
		zap.Int("size", len(data)))
}

// FileResponse represents a file storage response (manifest based)
type FileResponse struct {
    Hash       string `json:"hash"`
    Size       int64  `json:"size"`
    Chunks     int    `json:"chunks"`
    ManifestSz int    `json:"manifest_size"`
}

// handlePostFile accepts raw file bytes, splits into chunks, stores them,
// creates a manifest (stored as a content-addressed blob), and returns its hash
func (s *Server) handlePostFile(w http.ResponseWriter, r *http.Request) {
	// Sanity limit (larger than chunk endpoint). Adjust as needed.
	maxSize := int64(100 * 1024 * 1024) // 100MB
	if r.ContentLength > maxSize {
		http.Error(w, "Request too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Read all data (simple implementation)
	data, err := io.ReadAll(io.LimitReader(r.Body, maxSize))
	if err != nil {
		s.logger.Error("Failed to read file body", zap.Error(err))
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if len(data) == 0 {
		http.Error(w, "Empty request body", http.StatusBadRequest)
		return
	}

	// Chunk the data
	chunks, err := s.chunker.ChunkData(data)
	if err != nil {
		s.logger.Error("Failed to chunk data", zap.Error(err))
		http.Error(w, "Failed to chunk data", http.StatusInternalServerError)
		return
	}

	manifest := &storage.FileManifest{
		OriginalSize: int64(len(data)),
		ChunkSize:    s.chunker.GetChunkSize(),
		Chunks:       make([]storage.ChunkInfo, len(chunks)),
		CreatedAt:    time.Now().Unix(),
	}

	// Store chunks and fill manifest
	for i, ch := range chunks {
		h, err := s.chunkStore.PutChunk(ch)
		if err != nil {
			s.logger.Error("Failed to store chunk", zap.Error(err))
			http.Error(w, "Failed to store chunk", http.StatusInternalServerError)
			return
		}
		manifest.Chunks[i] = storage.ChunkInfo{Hash: h, Size: len(ch)}
	}

	// Announce each chunk in background
	httpAddr, err := s.advertisedHTTPAddr()
	if err != nil {
		s.logger.Warn("no advertised HTTP addr", zap.Error(err))
	}
	for _, ci := range manifest.Chunks {
		ci := ci // capture
		go func() {
			nodeInfo := &dht.NodeInfo{Id: s.dhtNode.ID(), Addr: s.dhtNode.Address(), HTTP: httpAddr}
			if err := s.dhtNode.StoreProvider(ci.Hash, nodeInfo); err != nil {
				s.logger.Debug("Failed to announce file chunk provider", zap.Error(err))
			}
		}()
	}

	// Store manifest as a content-addressed blob (JSON)
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		s.logger.Error("Failed to marshal manifest", zap.Error(err))
		http.Error(w, "Failed to create manifest", http.StatusInternalServerError)
		return
	}
	manifestHash, err := s.chunkStore.PutChunk(manifestBytes)
	if err != nil {
		s.logger.Error("Failed to store manifest", zap.Error(err))
		http.Error(w, "Failed to store manifest", http.StatusInternalServerError)
		return
	}

	// Announce manifest provider as well
	go func() {
		nodeInfo := &dht.NodeInfo{Id: s.dhtNode.ID(), Addr: s.dhtNode.Address(), HTTP: httpAddr}
		if err := s.dhtNode.StoreProvider(manifestHash, nodeInfo); err != nil {
			s.logger.Debug("Failed to announce manifest provider", zap.Error(err))
		}
	}()

	resp := FileResponse{
		Hash:       hex.EncodeToString(manifestHash),
		Size:       int64(len(data)),
		Chunks:     len(chunks),
		ManifestSz: len(manifestBytes),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)

	s.logger.Info("Stored file",
		zap.String("manifest", resp.Hash[:16]),
		zap.Int64("size", resp.Size),
		zap.Int("chunks", resp.Chunks))

	s.emitEvent("file_stored", map[string]interface{}{
		"manifest": resp.Hash,
		"size":     resp.Size,
		"chunks":   resp.Chunks,
	})
}

// handleGetFile retrieves a file by manifest hash; optionally searches the network
func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	manifestHex := vars["hash"]
	if len(manifestHex) != 64 {
		http.Error(w, "Invalid hash format", http.StatusBadRequest)
		return
	}
	manifestHash, err := hex.DecodeString(manifestHex)
	if err != nil {
		http.Error(w, "Invalid hash format", http.StatusBadRequest)
		return
	}

	allowRemote := r.URL.Query().Get("remote") == "1" || r.URL.Query().Get("remote") == "true"

	// Load manifest (local or remote)
	manifestBytes, err := s.chunkStore.GetChunk(manifestHash)
	if err != nil {
		if !allowRemote {
			http.Error(w, "Manifest not found locally (use ?remote=1)", http.StatusNotFound)
			return
		}
		// Fetch from network
		mb, ferr := s.fetchChunkFromNetwork(manifestHash, manifestHex)
		if ferr != nil {
			s.logger.Warn("Failed to fetch manifest from network", zap.Error(ferr))
			http.Error(w, "Manifest not found in network", http.StatusNotFound)
			return
		}
		manifestBytes = mb
		// Cache manifest
		if _, perr := s.chunkStore.PutChunk(manifestBytes); perr != nil {
			s.logger.Debug("Failed to cache manifest", zap.Error(perr))
		}

		s.emitEvent("manifest_cached", map[string]interface{}{
			"manifest": manifestHex,
		})
	}

	var manifest storage.FileManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		s.logger.Error("Failed to decode manifest", zap.Error(err))
		http.Error(w, "Invalid manifest", http.StatusBadRequest)
		return
	}

	// Retrieve all chunks
	chunks := make([][]byte, len(manifest.Chunks))
	for i, ci := range manifest.Chunks {
		b, err := s.chunkStore.GetChunk(ci.Hash)
		if err != nil {
			if !allowRemote {
				http.Error(w, "Chunk not found locally (use ?remote=1)", http.StatusNotFound)
				return
			}
        b, err = s.fetchChunkFromNetwork(ci.Hash, hex.EncodeToString(ci.Hash))
        if err != nil {
				s.logger.Warn("Failed to fetch chunk from network",
					zap.String("hash", hex.EncodeToString(ci.Hash)[:16]),
					zap.Error(err))
				http.Error(w, "Chunk not found in network", http.StatusNotFound)
				return
			}
            // Cache chunk
            if _, perr := s.chunkStore.PutChunk(b); perr != nil {
                s.logger.Debug("Failed to cache chunk", zap.Error(perr))
            } else {
                s.emitEvent("chunk_cached", map[string]interface{}{
                    "hash": hex.EncodeToString(ci.Hash),
                })
                // Become a provider for this chunk too
                if httpAddr, aerr := s.advertisedHTTPAddr(); aerr == nil {
                    h := make([]byte, len(ci.Hash)); copy(h, ci.Hash)
                    go func(hh []byte) {
                        _ = s.dhtNode.StoreProvider(hh, &dht.NodeInfo{Id: s.dhtNode.ID(), Addr: s.dhtNode.Address(), HTTP: httpAddr})
                    }(h)
                }
            }
		}
		chunks[i] = b
	}

	// Reconstruct and respond
	data, err := s.chunker.ReconstructData(chunks, &manifest)
	if err != nil {
		s.logger.Error("Failed to reconstruct data", zap.Error(err))
		http.Error(w, "Failed to reconstruct file", http.StatusInternalServerError)
		return
	}

	// Detect content type for full file responses too
	if len(data) > 0 {
		sniff := data
		if len(sniff) > 512 {
			sniff = sniff[:512]
		}
		w.Header().Set("Content-Type", http.DetectContentType(sniff))
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Content-Length", strconv.FormatInt(int64(len(data)), 10))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)

	s.logger.Info("Served file",
		zap.String("manifest", manifestHex[:16]),
		zap.Int("chunks", len(manifest.Chunks)),
		zap.Int("size", len(data)))

	s.emitEvent("file_served", map[string]interface{}{
		"manifest": manifestHex,
		"chunks":   len(manifest.Chunks),
		"size":     len(data),
		"remote":   allowRemote,
	})
}

// handlePostFileStream streams upload: reads request body in chunk-size pieces,
// stores each chunk immediately, builds the manifest on the fly, stores the manifest,
// and returns the manifest hash. Uses a size cap via MaxBytesReader.
func (s *Server) handlePostFileStream(w http.ResponseWriter, r *http.Request) {
    // Apply a streaming size cap
    const maxSize = int64(100 * 1024 * 1024) // 100MB, adjust if needed
    r.Body = http.MaxBytesReader(w, r.Body, maxSize)
    defer r.Body.Close()

    // Prepare manifest fields
    manifest := &storage.FileManifest{
        OriginalSize: 0,
        ChunkSize:    s.chunker.GetChunkSize(),
        Chunks:       make([]storage.ChunkInfo, 0, 16),
        CreatedAt:    time.Now().Unix(),
    }

    buf := make([]byte, s.chunker.GetChunkSize())

    // Build advertised HTTP address once
    httpAddr, err := s.advertisedHTTPAddr()
    if err != nil {
        s.logger.Warn("no advertised HTTP addr", zap.Error(err))
        httpAddr = ""
    }

    for {
        n, err := r.Body.Read(buf)
        if n > 0 {
            // Copy the bytes actually read to avoid retaining large buffer
            chunk := make([]byte, n)
            copy(chunk, buf[:n])

            // Store chunk
            hash, err := s.chunkStore.PutChunk(chunk)
            if err != nil {
                s.logger.Error("Failed to store streamed chunk", zap.Error(err))
                http.Error(w, "Failed to store chunk", http.StatusInternalServerError)
                return
            }

            manifest.Chunks = append(manifest.Chunks, storage.ChunkInfo{Hash: hash, Size: n})
            manifest.OriginalSize += int64(n)

            // Announce provider in background
            if httpAddr != "" {
                h := make([]byte, len(hash))
                copy(h, hash)
                go func(hh []byte) {
                    _ = s.dhtNode.StoreProvider(hh, &dht.NodeInfo{Id: s.dhtNode.ID(), Addr: s.dhtNode.Address(), HTTP: httpAddr})
                }(h)
            }
        }

        if err == io.EOF {
            break
        }
        if err != nil {
            // Detect client exceeding max bytes
            var mbe *http.MaxBytesError
            if errors.As(err, &mbe) {
                http.Error(w, "Request too large", http.StatusRequestEntityTooLarge)
                return
            }
            s.logger.Error("Stream read error", zap.Error(err))
            http.Error(w, "Read error", http.StatusBadRequest)
            return
        }
    }

    if manifest.OriginalSize == 0 {
        http.Error(w, "Empty request body", http.StatusBadRequest)
        return
    }

    // Store manifest as content-addressed JSON
    manifestBytes, err := json.Marshal(manifest)
    if err != nil {
        s.logger.Error("Failed to marshal manifest", zap.Error(err))
        http.Error(w, "Failed to create manifest", http.StatusInternalServerError)
        return
    }
    manifestHash, err := s.chunkStore.PutChunk(manifestBytes)
    if err != nil {
        s.logger.Error("Failed to store manifest", zap.Error(err))
        http.Error(w, "Failed to store manifest", http.StatusInternalServerError)
        return
    }

    // Announce manifest
    if httpAddr != "" {
        mh := make([]byte, len(manifestHash))
        copy(mh, manifestHash)
        go func(hh []byte) { _ = s.dhtNode.StoreProvider(hh, &dht.NodeInfo{Id: s.dhtNode.ID(), Addr: s.dhtNode.Address(), HTTP: httpAddr}) }(mh)
    }

    resp := FileResponse{
        Hash:       hex.EncodeToString(manifestHash),
        Size:       manifest.OriginalSize,
        Chunks:     len(manifest.Chunks),
        ManifestSz: len(manifestBytes),
    }

    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusCreated)
    _ = json.NewEncoder(w).Encode(resp)

    s.logger.Info("Stored file (stream)",
        zap.String("manifest", resp.Hash[:16]),
        zap.Int64("size", resp.Size),
        zap.Int("chunks", resp.Chunks))

    s.emitEvent("file_stored", map[string]interface{}{
        "manifest": resp.Hash,
        "size":     resp.Size,
        "chunks":   resp.Chunks,
    })
}

// handleDeleteFile deletes a file (manifest + referenced chunks) from local storage
func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
    vars := mux.Vars(r)
    manifestHex := vars["hash"]
    if len(manifestHex) != 64 {
        http.Error(w, "Invalid hash format", http.StatusBadRequest)
        return
    }
    manifestHash, err := hex.DecodeString(manifestHex)
    if err != nil {
        http.Error(w, "Invalid hash format", http.StatusBadRequest)
        return
    }

    // Try local manifest first
    manifestBytes, err := s.chunkStore.GetChunk(manifestHash)
    if err != nil {
        // Optionally fetch manifest from network to know which chunks to remove locally
        allowRemote := r.URL.Query().Get("remote") == "1" || r.URL.Query().Get("remote") == "true"
        if allowRemote {
            if mb, ferr := s.fetchChunkFromNetwork(manifestHash, manifestHex); ferr == nil {
                manifestBytes = mb
            } else {
                http.Error(w, "Manifest not found", http.StatusNotFound)
                return
            }
        } else {
            http.Error(w, "Manifest not found", http.StatusNotFound)
            return
        }
    }

    var manifest storage.FileManifest
    if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
        http.Error(w, "Invalid manifest", http.StatusBadRequest)
        return
    }

    removed := 0
    for _, ci := range manifest.Chunks {
        if err := s.chunkStore.DeleteChunk(ci.Hash); err == nil {
            removed++
        }
    }
    _ = s.chunkStore.DeleteChunk(manifestHash)

    // Optionally propagate to other providers for global deletion
    contacted := 0
    deletedOK := 0
    chunkContacted := 0
    chunkDeletedOK := 0
    wantGlobal := r.URL.Query().Get("global") == "1" || r.URL.Query().Get("global") == "true"
    // Guard to prevent infinite propagation
    propagate := r.URL.Query().Get("propagate")
    doPropagate := propagate != "0" && propagate != "false"
    if wantGlobal && doPropagate {
        // 1) Ask manifest providers to delete as well
        if providers, perr := s.dhtNode.FindProviders(manifestHash); perr == nil {
            client := &http.Client{Timeout: 5 * time.Second}
            for _, p := range providers {
                if len(p.Id) > 0 && len(s.dhtNode.ID()) > 0 && bytes.Equal(p.Id, s.dhtNode.ID()) {
                    continue
                }
                var httpAddr string
                if p.HTTP != "" { httpAddr = p.HTTP } else {
                    host, _, herr := net.SplitHostPort(p.Addr)
                    if herr != nil || host == "" { continue }
                    httpAddr = net.JoinHostPort(host, "8080")
                }
                url := fmt.Sprintf("http://%s/api/v1/file/%s?remote=1&propagate=0", httpAddr, manifestHex)
                req, _ := http.NewRequest("DELETE", url, nil)
                contacted++
                if resp, rerr := client.Do(req); rerr == nil {
                    if resp.StatusCode >= 200 && resp.StatusCode < 300 { deletedOK++ }
                    if resp.Body != nil { io.Copy(io.Discard, resp.Body); resp.Body.Close() }
                }
            }
        }
        // 2) Ask all chunk providers to delete their single chunks
        for _, ci := range manifest.Chunks {
            if providers, perr := s.dhtNode.FindProviders(ci.Hash); perr == nil {
                client := &http.Client{Timeout: 5 * time.Second}
                for _, p := range providers {
                    if len(p.Id) > 0 && len(s.dhtNode.ID()) > 0 && bytes.Equal(p.Id, s.dhtNode.ID()) {
                        continue
                    }
                    var httpAddr string
                    if p.HTTP != "" { httpAddr = p.HTTP } else {
                        host, _, herr := net.SplitHostPort(p.Addr)
                        if herr != nil || host == "" { continue }
                        httpAddr = net.JoinHostPort(host, "8080")
                    }
                    hashHex := hex.EncodeToString(ci.Hash)
                    url := fmt.Sprintf("http://%s/api/v1/chunk/%s?propagate=0", httpAddr, hashHex)
                    req, _ := http.NewRequest("DELETE", url, nil)
                    chunkContacted++
                    if resp, rerr := client.Do(req); rerr == nil {
                        if resp.StatusCode >= 200 && resp.StatusCode < 300 { chunkDeletedOK++ }
                        if resp.Body != nil { io.Copy(io.Discard, resp.Body); resp.Body.Close() }
                    }
                }
            }
        }
    }

    resp := map[string]interface{}{
        "status":              "deleted",
        "manifest":            manifestHex,
        "chunks_removed":      removed,
        "total_chunks":        len(manifest.Chunks),
        "providers_contacted": contacted,
        "providers_deleted":   deletedOK,
        "chunk_providers_contacted": chunkContacted,
        "chunk_providers_deleted":   chunkDeletedOK,
    }
    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(resp)

    s.emitEvent("file_deleted", map[string]interface{}{
        "manifest":            manifestHex,
        "removed":             removed,
        "chunks":              len(manifest.Chunks),
        "providers_contacted": contacted,
        "providers_deleted":   deletedOK,
        "chunk_providers_contacted": chunkContacted,
        "chunk_providers_deleted":   chunkDeletedOK,
    })
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
            s.emitEvent("http_fetch_attempt", map[string]interface{}{"http": provider.HTTP, "hash": hashHex})
            data, err := s.fetchChunkFromHTTP(provider.HTTP, hashHex)
            if err != nil {
                s.logger.Debug("Failed over HTTP from provider",
                    zap.String("http", provider.HTTP),
                    zap.Error(err))
                s.emitEvent("http_fetch_error", map[string]interface{}{"http": provider.HTTP, "hash": hashHex, "error": err.Error()})
                continue
            }
			// Verify chunk integrity
			actualHash := sha256.Sum256(data)
			if !bytes.Equal(hash, actualHash[:]) {
				s.logger.Warn("Chunk integrity check failed from provider (HTTP)",
					zap.String("http", provider.HTTP))
				continue
			}
            s.emitEvent("http_fetch_success", map[string]interface{}{"http": provider.HTTP, "hash": hashHex})
            return data, nil
        }

		// Fallback: derive HTTP from DHT address host
		host, _, err := net.SplitHostPort(provider.Addr)
		if err != nil || host == "" {
			s.logger.Debug("bad provider addr", zap.String("addr", provider.Addr), zap.Error(err))
			continue
		}
		httpAddr := net.JoinHostPort(host, "8080")
        s.emitEvent("http_fetch_attempt", map[string]interface{}{"http": httpAddr, "hash": hashHex})
        data, err := s.fetchChunkFromHTTP(httpAddr, hashHex)
        if err != nil {
            s.logger.Debug("Failed to fetch from provider",
                zap.String("dht_addr", provider.Addr),
                zap.String("http", httpAddr),
                zap.Error(err))
            s.emitEvent("http_fetch_error", map[string]interface{}{"http": httpAddr, "hash": hashHex, "error": err.Error()})
            continue
        }

		// Verify chunk integrity
		actualHash := sha256.Sum256(data)
		if !bytes.Equal(hash, actualHash[:]) {
			s.logger.Warn("Chunk integrity check failed from provider",
				zap.String("provider", provider.Addr))
			continue
		}

        s.emitEvent("http_fetch_success", map[string]interface{}{"http": httpAddr, "hash": hashHex})
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
			"post_file":  "POST /api/v1/file",
			"get_file":   "GET /api/v1/file/{manifest_hash}",
			"delete_file":"DELETE /api/v1/file/{manifest_hash}",
			"events":     "GET /api/v1/events",
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
        origin := r.Header.Get("Origin")
        if origin != "" {
            w.Header().Set("Access-Control-Allow-Origin", origin)
        } else {
            w.Header().Set("Access-Control-Allow-Origin", "*")
        }
        // Ensure caches and proxies treat responses per-origin
        w.Header().Add("Vary", "Origin")
        w.Header().Add("Vary", "Access-Control-Request-Headers")

        // Allowed methods
        w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")

        // Reflect requested headers if provided (robust across browsers)
        if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
            w.Header().Set("Access-Control-Allow-Headers", req)
        } else {
            w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
        }
        w.Header().Set("Access-Control-Max-Age", "600")

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

// SSE hub and helpers
type sseHub struct {
    mu      sync.Mutex
    clients map[chan []byte]struct{}
}

func newSSEHub() *sseHub {
    return &sseHub{clients: make(map[chan []byte]struct{})}
}

func (h *sseHub) add() (chan []byte, func()) {
    ch := make(chan []byte, 16)
    h.mu.Lock()
    h.clients[ch] = struct{}{}
    h.mu.Unlock()
    remove := func() {
        h.mu.Lock()
        delete(h.clients, ch)
        h.mu.Unlock()
        close(ch)
    }
    return ch, remove
}

func (h *sseHub) broadcast(b []byte) {
    h.mu.Lock()
    for ch := range h.clients {
        select { case ch <- b: default: }
    }
    h.mu.Unlock()
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")
    w.Header().Set("Connection", "keep-alive")
    if f, ok := w.(http.Flusher); ok { f.Flush() }

    ch, remove := s.sse.add()
    defer remove()

    // Send initial hello directly to this connection
    hello := map[string]interface{}{
        "event":     "hello",
        "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
        "node_id":   s.config.Node.ID,
        "message":   "connected",
    }
    if b, err := json.Marshal(hello); err == nil {
        fmt.Fprintf(w, "data: %s\n\n", b)
        if f, ok := w.(http.Flusher); ok { f.Flush() }
    }

    ticker := time.NewTicker(25 * time.Second)
    defer ticker.Stop()
    notify := r.Context().Done()

    for {
        select {
        case <-notify:
            return
        case b := <-ch:
            fmt.Fprintf(w, "data: %s\n\n", b)
            if f, ok := w.(http.Flusher); ok { f.Flush() }
        case <-ticker.C:
            fmt.Fprintf(w, ": ping\n\n")
            if f, ok := w.(http.Flusher); ok { f.Flush() }
        }
    }
}

func (s *Server) emitEvent(event string, payload map[string]interface{}) {
    m := map[string]interface{}{
        "event":     event,
        "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
        "node_id":   s.config.Node.ID,
    }
    for k, v := range payload { m[k] = v }
    b, _ := json.Marshal(m)
    s.sse.broadcast(b)
}
