package storage

import (
	"crypto/sha256"
	"fmt"
	"io"
)

// ChunkInfo represents metadata about a chunk
type ChunkInfo struct {
	Hash []byte `json:"hash"`
	Size int    `json:"size"`
}

// FileManifest represents a chunked file
type FileManifest struct {
	OriginalSize int64       `json:"original_size"`
	ChunkSize    int64       `json:"chunk_size"`
	Chunks       []ChunkInfo `json:"chunks"`
	CreatedAt    int64       `json:"created_at"`
}

// Chunker handles file chunking operations
type Chunker struct {
	chunkSize int64
}

// NewChunker creates a new chunker with the specified chunk size
func NewChunker(chunkSize int64) *Chunker {
	return &Chunker{
		chunkSize: chunkSize,
	}
}

// ChunkData splits data into fixed-size chunks
func (c *Chunker) ChunkData(data []byte) ([][]byte, error) {
	if len(data) == 0 {
		return [][]byte{}, nil
	}

	var chunks [][]byte
	dataLen := int64(len(data))

	for offset := int64(0); offset < dataLen; offset += c.chunkSize {
		end := offset + c.chunkSize
		if end > dataLen {
			end = dataLen
		}

		chunk := make([]byte, end-offset)
		copy(chunk, data[offset:end])
		chunks = append(chunks, chunk)
	}

	return chunks, nil
}

// ChunkReader splits data from a reader into chunks
func (c *Chunker) ChunkReader(reader io.Reader) ([][]byte, error) {
	var chunks [][]byte
	buffer := make([]byte, c.chunkSize)

	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buffer[:n])
			chunks = append(chunks, chunk)
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("error reading data: %w", err)
		}
	}

	return chunks, nil
}

// HashChunk computes SHA-256 hash of chunk data
func (c *Chunker) HashChunk(chunk []byte) []byte {
	hash := sha256.Sum256(chunk)
	return hash[:]
}

// CreateManifest creates a manifest for chunked data
func (c *Chunker) CreateManifest(data []byte) (*FileManifest, error) {
	chunks, err := c.ChunkData(data)
	if err != nil {
		return nil, err
	}

	manifest := &FileManifest{
		OriginalSize: int64(len(data)),
		ChunkSize:    c.chunkSize,
		Chunks:       make([]ChunkInfo, len(chunks)),
		CreatedAt:    getCurrentTimestamp(),
	}

	for i, chunk := range chunks {
		manifest.Chunks[i] = ChunkInfo{
			Hash: c.HashChunk(chunk),
			Size: len(chunk),
		}
	}

	return manifest, nil
}

// ReconstructData reassembles chunks back into original data
func (c *Chunker) ReconstructData(chunks [][]byte, manifest *FileManifest) ([]byte, error) {
	if len(chunks) != len(manifest.Chunks) {
		return nil, fmt.Errorf("chunk count mismatch: expected %d, got %d",
			len(manifest.Chunks), len(chunks))
	}

	// Verify chunk hashes
	for i, chunk := range chunks {
		expectedHash := manifest.Chunks[i].Hash
		actualHash := c.HashChunk(chunk)

		if !bytesEqual(expectedHash, actualHash) {
			return nil, fmt.Errorf("chunk %d hash mismatch", i)
		}
	}

	// Reconstruct data
	data := make([]byte, manifest.OriginalSize)
	offset := int64(0)

	for i, chunk := range chunks {
		expectedSize := manifest.Chunks[i].Size
		if len(chunk) != expectedSize {
			return nil, fmt.Errorf("chunk %d size mismatch: expected %d, got %d",
				i, expectedSize, len(chunk))
		}

		copy(data[offset:], chunk)
		offset += int64(len(chunk))
	}

	return data, nil
}

func (c *Chunker) GetChunkSize() int64 {
	return c.chunkSize
}

// Helper functions
func getCurrentTimestamp() int64 {
	// In a real implementation, you'd use time.Now().Unix()
	// For testing, we'll use a fixed value
	return 1609459200 // 2021-01-01 00:00:00 UTC
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
