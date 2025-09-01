package dht

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// Constants for SHA-256 based keys
const (
	KeyBytes = sha256.Size  // 32 bytes
	KeyBits  = 8 * KeyBytes // 256 bits
)

// Common errors
var (
	ErrInvalidNode = errors.New("invalid node")
	ErrNotFound    = errors.New("not found")
)

// NodeInfo represents a node in the DHT network

type NodeInfo struct {
	Id   []byte `json:"id"`   // peer id bytes or hash
	Addr string `json:"addr"` // DHT dial address "host:7000"
	HTTP string `json:"http"` // HTTP fetch address "host:8080"
}

func (n *NodeInfo) MarshalJSON() ([]byte, error) {
	type alias struct {
		Id   string `json:"id"`
		Addr string `json:"addr"`
		HTTP string `json:"http,omitempty"`
	}
	return json.Marshal(alias{
		Id:   hex.EncodeToString(n.Id),
		Addr: n.Addr,
		HTTP: n.HTTP,
	})
}

func (n *NodeInfo) UnmarshalJSON(b []byte) error {
	var a struct {
		Id   string `json:"id"`
		Addr string `json:"addr"`
		HTTP string `json:"http"`
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	id, err := hex.DecodeString(a.Id)
	if err != nil {
		return err
	}
	n.Id, n.Addr, n.HTTP = id, a.Addr, a.HTTP
	return nil
}

// RoutingTable manages the k-buckets for storing known peers
type RoutingTable interface {
	InsertNode(node *NodeInfo)
	RemoveNode(key []byte) error
	Lookup(key []byte) (node *NodeInfo, ok bool)
	GetNodes(bucket int) []*NodeInfo
	ClosestK(key []byte) []*NodeInfo
	Buckets() int
	K() int
}

// Node represents a DHT node with Mini-IPFS capabilities
type Node interface {
	// Basic DHT operations
	Ping(addr string) error
	FindNode(target []byte) ([]*NodeInfo, error)

	// Mini-IPFS specific: content provider discovery
	StoreProvider(hash []byte, provider *NodeInfo) error
	FindProviders(hash []byte) ([]*NodeInfo, error)

	// Lifecycle
	Start() error
	Stop() error

	// Info
	ID() []byte
	Address() string
	RoutingTable() RoutingTable

	// Observability: optional sink for emitting node-level events (e.g., RPC activity)
	SetEventSink(func(event string, payload map[string]interface{}))
}

// RPC message structure for node-to-node communication
type RPCMessage struct {
	Type    string      `json:"type"`
	ID      string      `json:"id"` // Unique request ID
	Sender  *NodeInfo   `json:"sender"`
	Payload interface{} `json:"payload"`
}

// Message types
const (
	MessageTypePing          = "PING"
	MessageTypePong          = "PONG"
	MessageTypeFindNode      = "FIND_NODE"
	MessageTypeFindNodeResp  = "FIND_NODE_RESP"
	MessageTypeStoreProvider = "STORE_PROVIDER"
	MessageTypeStoreResp     = "STORE_RESP"
	MessageTypeFindProviders = "FIND_PROVIDERS"
	MessageTypeProvidersResp = "PROVIDERS_RESP"
)

// Request/Response payloads
type PingRequest struct{}

type PongResponse struct{}

type FindNodeRequest struct {
	Target string `json:"target"` // Hex-encoded target ID
}

type FindNodeResponse struct {
	Nodes []*NodeInfo `json:"nodes"`
}

type StoreProviderRequest struct {
	Hash     string    `json:"hash"`     // Hex-encoded content hash
	Provider *NodeInfo `json:"provider"` // Node that has this content
}

type StoreResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

type FindProvidersRequest struct {
	Hash string `json:"hash"` // Hex-encoded content hash
}

type ProvidersResponse struct {
	Providers []*NodeInfo `json:"providers"`
}
