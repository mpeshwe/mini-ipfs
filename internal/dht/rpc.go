package dht

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"go.uber.org/zap"
)

// RPCServer handles incoming DHT RPC requests
type RPCServer struct {
	node     *dhtNode
	listener net.Listener
	logger   *zap.Logger
	shutdown chan struct{}
	wg       sync.WaitGroup
}

// RPCClient handles outgoing DHT RPC requests
type RPCClient struct {
	logger *zap.Logger
}

// NewRPCServer creates a new RPC server for the DHT node
func NewRPCServer(node *dhtNode) *RPCServer {
	return &RPCServer{
		node:     node,
		logger:   node.logger.With(zap.String("component", "rpc-server")),
		shutdown: make(chan struct{}),
	}
}

// NewRPCClient creates a new RPC client
func NewRPCClient(logger *zap.Logger) *RPCClient {
	return &RPCClient{
		logger: logger.With(zap.String("component", "rpc-client")),
	}
}

// Start begins listening for RPC connections
func (s *RPCServer) Start(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to start RPC server: %w", err)
	}

	s.listener = listener
	s.logger.Info("RPC server started", zap.String("addr", addr))

	s.wg.Add(1)
	go s.acceptLoop()

	return nil
}

// Stop gracefully shuts down the RPC server
func (s *RPCServer) Stop() error {
	if s.listener == nil {
		return nil
	}

	close(s.shutdown)
	s.listener.Close()
	s.wg.Wait()

	s.logger.Info("RPC server stopped")
	return nil
}

// acceptLoop continuously accepts new connections
func (s *RPCServer) acceptLoop() {
	defer s.wg.Done()

	for {
		select {
		case <-s.shutdown:
			return
		default:
		}

		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.shutdown:
				return
			default:
				s.logger.Error("Failed to accept connection", zap.Error(err))
				continue
			}
		}

		s.wg.Add(1)
		go s.handleConnection(conn)
	}
}

// handleConnection processes a single RPC connection
func (s *RPCServer) handleConnection(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	// Set read timeout
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

        var msg RPCMessage
        if err := json.Unmarshal([]byte(line), &msg); err != nil {
            s.logger.Error("Failed to parse RPC message", zap.Error(err))
            continue
        }

        s.logger.Debug("Received RPC message",
            zap.String("type", msg.Type),
            zap.String("from", fmt.Sprintf("%x", msg.Sender.Id[:8])),
        )

        // Emit inbound RPC event for UI visibility
        if s.node != nil {
            switch msg.Type {
            case MessageTypePing:
                s.node.emitEvent("rpc_ping_in", map[string]interface{}{"from": conn.RemoteAddr().String()})
            case MessageTypeFindNode:
                s.node.emitEvent("rpc_find_node_in", map[string]interface{}{"from": conn.RemoteAddr().String()})
            case MessageTypeFindProviders:
                s.node.emitEvent("rpc_find_providers_in", map[string]interface{}{"from": conn.RemoteAddr().String()})
            case MessageTypeStoreProvider:
                s.node.emitEvent("rpc_store_provider_in", map[string]interface{}{"from": conn.RemoteAddr().String()})
            }
        }

		// Update routing table with sender info
		s.node.routing.InsertNode(msg.Sender)

		// Handle the message
		response := s.handleMessage(&msg)
		if response != nil {
			responseBytes, err := json.Marshal(response)
			if err != nil {
				s.logger.Error("Failed to marshal response", zap.Error(err))
				continue
			}

			// Send response
			responseBytes = append(responseBytes, '\n')
			if _, err := conn.Write(responseBytes); err != nil {
				s.logger.Error("Failed to send response", zap.Error(err))
				break
			}
		}
	}

	if err := scanner.Err(); err != nil {
		s.logger.Error("Connection scan error", zap.Error(err))
	}
}

// handleMessage processes a specific RPC message type
//
//	func (s *RPCServer) handleMessage(msg *RPCMessage) *RPCMessage {
//		switch msg.Type {
//		case MessageTypePing:
//			return s.handlePing(msg)
//		case MessageTypeFindNode:
//			return s.handleFindNode(msg)
//		default:
//			s.logger.Warn("Unknown message type", zap.String("type", msg.Type))
//			return nil
//		}
//	}
//
// handleMessage processes a specific RPC message type
func (s *RPCServer) handleMessage(msg *RPCMessage) *RPCMessage {
	switch msg.Type {
	case MessageTypePing:
		return s.handlePing(msg)
	case MessageTypeFindNode:
		return s.handleFindNode(msg)
	case MessageTypeStoreProvider:
		return s.handleStoreProvider(msg)
	case MessageTypeFindProviders:
		return s.handleFindProviders(msg)
	default:
		s.logger.Warn("Unknown message type", zap.String("type", msg.Type))
		return nil
	}
}

// handleStoreProvider processes STORE_PROVIDER requests
// func (s *RPCServer) handleStoreProvider(msg *RPCMessage) *RPCMessage {
// 	payload, ok := msg.Payload.(map[string]interface{})
// 	if !ok {
// 		s.logger.Error("Invalid StoreProvider payload")
// 		return &RPCMessage{
// 			Type:   MessageTypeStoreResp,
// 			ID:     msg.ID,
// 			Sender: s.node.nodeInfo,
// 			Payload: StoreResponse{
// 				Success: false,
// 				Error:   "Invalid payload",
// 			},
// 		}
// 	}

// 	hashHex, ok := payload["hash"].(string)
// 	if !ok {
// 		s.logger.Error("Missing hash in StoreProvider request")
// 		return &RPCMessage{
// 			Type:   MessageTypeStoreResp,
// 			ID:     msg.ID,
// 			Sender: s.node.nodeInfo,
// 			Payload: StoreResponse{
// 				Success: false,
// 				Error:   "Missing hash",
// 			},
// 		}
// 	}

// 	//providerData, ok := payload["provider"].(map[string]interface{})
// 	providerHTTP, ok := providerData["http"].(string)
// 	if !ok {
// 		s.logger.Error("Missing provider in StoreProvider request")
// 		return &RPCMessage{
// 			Type:   MessageTypeStoreResp,
// 			ID:     msg.ID,
// 			Sender: s.node.nodeInfo,
// 			Payload: StoreResponse{
// 				Success: false,
// 				Error:   "Missing provider",
// 			},
// 		}
// 	}

// 	// Parse provider info
// 	providerIdHex, ok := providerData["id"].(string)
// 	if !ok {
// 		return &RPCMessage{
// 			Type:   MessageTypeStoreResp,
// 			ID:     msg.ID,
// 			Sender: s.node.nodeInfo,
// 			Payload: StoreResponse{
// 				Success: false,
// 				Error:   "Invalid provider ID",
// 			},
// 		}
// 	}

// 	providerAddr, ok := providerData["addr"].(string)
// 	if !ok {
// 		return &RPCMessage{
// 			Type:   MessageTypeStoreResp,
// 			ID:     msg.ID,
// 			Sender: s.node.nodeInfo,
// 			Payload: StoreResponse{
// 				Success: false,
// 				Error:   "Invalid provider address",
// 			},
// 		}
// 	}

// 	// Decode provider ID
// 	providerId, err := hex.DecodeString(providerIdHex)
// 	if err != nil {
// 		return &RPCMessage{
// 			Type:   MessageTypeStoreResp,
// 			ID:     msg.ID,
// 			Sender: s.node.nodeInfo,
// 			Payload: StoreResponse{
// 				Success: false,
// 				Error:   "Invalid provider ID format",
// 			},
// 		}
// 	}

// 	provider := &NodeInfo{
// 		Id:   providerId,
// 		Addr: providerAddr,
// 	}

// 	// Store provider record locally
// 	s.node.storeProviderLocal(hashHex, provider)

// 	s.logger.Debug("Stored provider record",
// 		zap.String("hash", hashHex[:16]),
// 		zap.String("provider", fmt.Sprintf("%x", provider.Id[:8])))

// 	return &RPCMessage{
// 		Type:   MessageTypeStoreResp,
// 		ID:     msg.ID,
// 		Sender: s.node.nodeInfo,
// 		Payload: StoreResponse{
// 			Success: true,
// 		},
// 	}
// }

// handleStoreProvider processes STORE_PROVIDER requests
func (s *RPCServer) handleStoreProvider(msg *RPCMessage) *RPCMessage {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		s.logger.Error("Invalid StoreProvider payload")
		return &RPCMessage{
			Type:    MessageTypeStoreResp,
			ID:      msg.ID,
			Sender:  s.node.nodeInfo,
			Payload: StoreResponse{Success: false, Error: "Invalid payload"},
		}
	}

	hashHex, ok := payload["hash"].(string)
	if !ok {
		s.logger.Error("Missing hash in StoreProvider request")
		return &RPCMessage{
			Type:    MessageTypeStoreResp,
			ID:      msg.ID,
			Sender:  s.node.nodeInfo,
			Payload: StoreResponse{Success: false, Error: "Missing hash"},
		}
	}

	providerData, ok := payload["provider"].(map[string]interface{})
	if !ok {
		s.logger.Error("Missing provider in StoreProvider request")
		return &RPCMessage{
			Type:    MessageTypeStoreResp,
			ID:      msg.ID,
			Sender:  s.node.nodeInfo,
			Payload: StoreResponse{Success: false, Error: "Missing provider"},
		}
	}

	// Parse provider info
	providerIdHex, ok := providerData["id"].(string)
	if !ok {
		return &RPCMessage{
			Type:    MessageTypeStoreResp,
			ID:      msg.ID,
			Sender:  s.node.nodeInfo,
			Payload: StoreResponse{Success: false, Error: "Invalid provider ID"},
		}
	}

	providerAddr, ok := providerData["addr"].(string)
	if !ok {
		return &RPCMessage{
			Type:    MessageTypeStoreResp,
			ID:      msg.ID,
			Sender:  s.node.nodeInfo,
			Payload: StoreResponse{Success: false, Error: "Invalid provider address"},
		}
	}

	// NEW: optional HTTP address (host:port for HTTP API)
	providerHTTP, _ := providerData["http"].(string)

	// Decode provider ID
	providerId, err := hex.DecodeString(providerIdHex)
	if err != nil {
		return &RPCMessage{
			Type:    MessageTypeStoreResp,
			ID:      msg.ID,
			Sender:  s.node.nodeInfo,
			Payload: StoreResponse{Success: false, Error: "Invalid provider ID format"},
		}
	}

	provider := &NodeInfo{
		Id:   providerId,
		Addr: providerAddr,
		HTTP: providerHTTP, // keep the advertised HTTP address if present
	}

	// Store provider record locally
	s.node.storeProviderLocal(hashHex, provider)

	s.logger.Debug("Stored provider record",
		zap.String("hash", hashHex[:16]),
		zap.String("provider", fmt.Sprintf("%x", provider.Id[:8])),
		zap.String("http", provider.HTTP))

	return &RPCMessage{
		Type:    MessageTypeStoreResp,
		ID:      msg.ID,
		Sender:  s.node.nodeInfo,
		Payload: StoreResponse{Success: true},
	}
}

// handleFindProviders processes FIND_PROVIDERS requests
// func (s *RPCServer) handleFindProviders(msg *RPCMessage) *RPCMessage {
// 	payload, ok := msg.Payload.(map[string]interface{})
// 	if !ok {
// 		s.logger.Error("Invalid FindProviders payload")
// 		return nil
// 	}

// 	hashHex, ok := payload["hash"].(string)
// 	if !ok {
// 		s.logger.Error("Missing hash in FindProviders request")
// 		return nil
// 	}

// 	// Get local providers for this hash
// 	s.node.providersMux.RLock()
// 	providers := make([]*NodeInfo, len(s.node.providers[hashHex]))
// 	copy(providers, s.node.providers[hashHex])
// 	s.node.providersMux.RUnlock()

// 	s.logger.Debug("Returning providers",
// 		zap.String("hash", hashHex[:16]),
// 		zap.Int("provider_count", len(providers)))

//		return &RPCMessage{
//			Type:   MessageTypeProvidersResp,
//			ID:     msg.ID,
//			Sender: s.node.nodeInfo,
//			Payload: ProvidersResponse{
//				Providers: providers,
//			},
//		}
//	}
func (s *RPCServer) handleFindProviders(msg *RPCMessage) *RPCMessage {
    // Parse and validate payload
    payload, ok := msg.Payload.(map[string]interface{})
    if !ok {
        s.logger.Error("Invalid FindProviders payload")
        return &RPCMessage{
            Type:   MessageTypeProvidersResp,
            ID:     msg.ID,
            Sender: s.node.nodeInfo,
            Payload: ProvidersResponse{
                Providers: []*NodeInfo{},
            },
        }
    }

    hashHex, ok := payload["hash"].(string)
    if !ok || len(hashHex) == 0 {
        s.logger.Error("Missing or invalid hash in FindProviders request")
        return &RPCMessage{
            Type:   MessageTypeProvidersResp,
            ID:     msg.ID,
            Sender: s.node.nodeInfo,
            Payload: ProvidersResponse{
                Providers: []*NodeInfo{},
            },
        }
    }

    // Get local providers for this hash
    providers := s.node.loadProviders(hashHex)

    return &RPCMessage{
        Type:   MessageTypeProvidersResp,
        ID:     msg.ID,
        Sender: s.node.nodeInfo,
        Payload: ProvidersResponse{Providers: providers},
    }
}

// handlePing responds to ping requests
func (s *RPCServer) handlePing(msg *RPCMessage) *RPCMessage {
	return &RPCMessage{
		Type:    MessageTypePong,
		ID:      msg.ID,
		Sender:  s.node.nodeInfo,
		Payload: PongResponse{},
	}
}

// handleFindNode responds to find node requests
// handleFindNode responds to find node requests
func (s *RPCServer) handleFindNode(msg *RPCMessage) *RPCMessage {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		s.logger.Error("Invalid FindNode payload")
		return nil
	}

	targetHex, ok := payload["target"].(string)
	if !ok {
		s.logger.Error("Missing target in FindNode request")
		return nil
	}

	// Convert hex string back to bytes
	target, err := hex.DecodeString(targetHex)
	if err != nil {
		s.logger.Error("Invalid target hex in FindNode request", zap.Error(err))
		return nil
	}

	// Find closest nodes
	closestNodes := s.node.routing.ClosestK(target)

	return &RPCMessage{
		Type:   MessageTypeFindNodeResp,
		ID:     msg.ID,
		Sender: s.node.nodeInfo,
		Payload: FindNodeResponse{
			Nodes: closestNodes,
		},
	}
}

// SendMessage sends an RPC message to a remote node
func (c *RPCClient) SendMessage(addr string, msg *RPCMessage) (*RPCMessage, error) {
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %w", addr, err)
	}
	defer conn.Close()

	// Set timeouts
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))

	// Send message
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal message: %w", err)
	}

	msgBytes = append(msgBytes, '\n')
	if _, err := conn.Write(msgBytes); err != nil {
		return nil, fmt.Errorf("failed to send message: %w", err)
	}

	c.logger.Debug("Sent RPC message",
		zap.String("type", msg.Type),
		zap.String("to", addr),
	)

	// Read response
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("failed to read response: %w", err)
		}
		return nil, fmt.Errorf("no response received")
	}

	var response RPCMessage
	if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &response, nil
}
