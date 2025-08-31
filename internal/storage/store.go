package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"go.uber.org/zap"
)

// ChunkStore manages local chunk storage
type ChunkStore struct {
	dataDir string
	logger  *zap.Logger
	mutex   sync.RWMutex
	stats   StoreStats
}

// StoreStats tracks storage statistics
type StoreStats struct {
	TotalChunks int64 `json:"total_chunks"`
	TotalBytes  int64 `json:"total_bytes"`
}

// NewChunkStore creates a new chunk store
func NewChunkStore(dataDir string, logger *zap.Logger) (*ChunkStore, error) {
	// Ensure data directory exists
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	store := &ChunkStore{
		dataDir: dataDir,
		logger:  logger.With(zap.String("component", "chunk-store")),
	}

	// Initialize stats
	if err := store.updateStats(); err != nil {
		store.logger.Warn("Failed to update initial stats", zap.Error(err))
	}

	return store, nil
}

// PutChunk stores a chunk and returns its hash
func (s *ChunkStore) PutChunk(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("cannot store empty chunk")
	}

	// Compute hash
	hash := sha256.Sum256(data)
	hashBytes := hash[:]
	hashHex := hex.EncodeToString(hashBytes)

	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Check if chunk already exists
	chunkPath := s.getChunkPath(hashHex)
	if _, err := os.Stat(chunkPath); err == nil {
		s.logger.Debug("Chunk already exists", zap.String("hash", hashHex[:16]))
		return hashBytes, nil
	}

	// Ensure directory exists
	dir := filepath.Dir(chunkPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create chunk directory: %w", err)
	}

	// Write chunk to temporary file first
	tempPath := chunkPath + ".tmp"
	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		return nil, fmt.Errorf("failed to write chunk: %w", err)
	}

	// Atomic move to final location
	if err := os.Rename(tempPath, chunkPath); err != nil {
		os.Remove(tempPath) // Clean up temp file
		return nil, fmt.Errorf("failed to move chunk to final location: %w", err)
	}

	// Update stats
	s.stats.TotalChunks++
	s.stats.TotalBytes += int64(len(data))

	s.logger.Debug("Stored chunk",
		zap.String("hash", hashHex[:16]),
		zap.Int("size", len(data)))

	return hashBytes, nil
}

// GetChunk retrieves a chunk by its hash
func (s *ChunkStore) GetChunk(hash []byte) ([]byte, error) {
	if len(hash) != 32 {
		return nil, fmt.Errorf("invalid hash length: %d", len(hash))
	}

	hashHex := hex.EncodeToString(hash)
	chunkPath := s.getChunkPath(hashHex)

	s.mutex.RLock()
	defer s.mutex.RUnlock()

	// Read chunk data
	data, err := os.ReadFile(chunkPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("chunk not found: %s", hashHex[:16])
		}
		return nil, fmt.Errorf("failed to read chunk: %w", err)
	}

	// Verify integrity
	actualHash := sha256.Sum256(data)
	if !bytesEqual(hash, actualHash[:]) {
		s.logger.Error("Chunk integrity check failed",
			zap.String("expected", hashHex[:16]),
			zap.String("actual", hex.EncodeToString(actualHash[:])[:16]))
		return nil, fmt.Errorf("chunk integrity check failed")
	}

	s.logger.Debug("Retrieved chunk",
		zap.String("hash", hashHex[:16]),
		zap.Int("size", len(data)))

	return data, nil
}

// HasChunk checks if a chunk exists without reading it
func (s *ChunkStore) HasChunk(hash []byte) bool {
	if len(hash) != 32 {
		return false
	}

	hashHex := hex.EncodeToString(hash)
	chunkPath := s.getChunkPath(hashHex)

	s.mutex.RLock()
	defer s.mutex.RUnlock()

	_, err := os.Stat(chunkPath)
	return err == nil
}

// DeleteChunk removes a chunk from storage
func (s *ChunkStore) DeleteChunk(hash []byte) error {
	if len(hash) != 32 {
		return fmt.Errorf("invalid hash length: %d", len(hash))
	}

	hashHex := hex.EncodeToString(hash)
	chunkPath := s.getChunkPath(hashHex)

	s.mutex.Lock()
	defer s.mutex.Unlock()

	// Get file size before deletion for stats
	info, err := os.Stat(chunkPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("chunk not found: %s", hashHex[:16])
		}
		return fmt.Errorf("failed to stat chunk: %w", err)
	}

	// Delete the chunk
	if err := os.Remove(chunkPath); err != nil {
		return fmt.Errorf("failed to delete chunk: %w", err)
	}

	// Update stats
	s.stats.TotalChunks--
	s.stats.TotalBytes -= info.Size()

	s.logger.Debug("Deleted chunk",
		zap.String("hash", hashHex[:16]),
		zap.Int64("size", info.Size()))

	return nil
}

// GetStats returns current storage statistics
func (s *ChunkStore) GetStats() StoreStats {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return s.stats
}

// ListChunks returns all chunk hashes in storage
func (s *ChunkStore) ListChunks() ([][]byte, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	var hashes [][]byte

	err := filepath.WalkDir(s.dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		// Skip temporary files
		if filepath.Ext(path) == ".tmp" {
			return nil
		}

		// Extract hash from filename
		filename := d.Name()
		if len(filename) == 64 { // SHA-256 hex string
			hash, err := hex.DecodeString(filename)
			if err == nil {
				hashes = append(hashes, hash)
			}
		}

		return nil
	})

	return hashes, err
}

// getChunkPath returns the file path for a chunk hash
func (s *ChunkStore) getChunkPath(hashHex string) string {
	// Use first 2 characters as subdirectory for better filesystem performance
	// e.g., hash "abcdef..." becomes "data/ab/abcdef..."
	if len(hashHex) < 2 {
		return filepath.Join(s.dataDir, hashHex)
	}

	subdir := hashHex[:2]
	return filepath.Join(s.dataDir, subdir, hashHex)
}

// updateStats recalculates storage statistics
func (s *ChunkStore) updateStats() error {
	var totalChunks int64
	var totalBytes int64

	err := filepath.WalkDir(s.dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() || filepath.Ext(path) == ".tmp" {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		totalChunks++
		totalBytes += info.Size()
		return nil
	})

	if err != nil {
		return err
	}

	s.mutex.Lock()
	s.stats.TotalChunks = totalChunks
	s.stats.TotalBytes = totalBytes
	s.mutex.Unlock()

	return nil
}

// // Helper function (reused from chunker)
// func bytesEqual(a, b []byte) bool {
// 	if len(a) != len(b) {
// 		return false
// 	}
// 	for i := range a {
// 		if a[i] != b[i] {
// 			return false
// 		}
// 	}
// 	return true
// }
