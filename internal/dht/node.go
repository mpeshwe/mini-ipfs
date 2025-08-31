package dht

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/mpeshwe/mini-ipfs/internal/util"
)

// dhtNode implements the Node interface
type dhtNode struct {
	nodeInfo  *NodeInfo
	config    *util.Config
	logger    *zap.Logger
	routing   RoutingTable
	rpcServer *RPCServer
	rpcClient *RPCClient
	requestID uint64
	idMutex   sync.Mutex
}

// NewNode creates a new DHT node
func NewNode(config *util.Config, logger *zap.Logger) (Node, error) {
	// Generate node ID from DHT address
	nodeID := generateNodeID(config.Node.DHTAddr)

	nodeInfo := &NodeInfo{
		Id:   nodeID,
		Addr: config.Node.DHTAddr,
	}

	// Create routing table
	routing, err := NewRoutingTable(nodeInfo, config.DHT.K)
	if err != nil {
		return nil, fmt.Errorf("failed to create routing table: %w", err)
	}

	node := &dhtNode{
		nodeInfo:  nodeInfo,
		config:    config,
		logger:    logger.With(zap.String("component", "dht")),
		routing:   routing,
		rpcClient: NewRPCClient(logger),
	}

	// Create RPC server
	node.rpcServer = NewRPCServer(node)

	return node, nil
}

// generateNodeID creates a SHA-256 hash of the node's address
func generateNodeID(addr string) []byte {
	hash := sha256.Sum256([]byte(addr))
	return hash[:]
}

// Start initializes the DHT node and starts the RPC server
func (n *dhtNode) Start() error {
	n.logger.Info("Starting DHT node",
		zap.String("node_id", fmt.Sprintf("%x", n.nodeInfo.Id[:8])),
		zap.String("address", n.nodeInfo.Addr),
	)

	// Start RPC server
	if err := n.rpcServer.Start(n.nodeInfo.Addr); err != nil {
		return fmt.Errorf("failed to start RPC server: %w", err)
	}

	// Bootstrap from configured nodes
	go n.bootstrap()

	n.logger.Info("DHT node started")
	return nil
}

// Stop gracefully shuts down the DHT node
func (n *dhtNode) Stop() error {
	n.logger.Info("Stopping DHT node")

	if n.rpcServer != nil {
		if err := n.rpcServer.Stop(); err != nil {
			n.logger.Error("Error stopping RPC server", zap.Error(err))
		}
	}

	return nil
}

// generateRequestID creates a unique request ID
func (n *dhtNode) generateRequestID() string {
	n.idMutex.Lock()
	defer n.idMutex.Unlock()
	n.requestID++
	return fmt.Sprintf("%x-%d", n.nodeInfo.Id[:4], n.requestID)
}

// bootstrap connects to bootstrap nodes and populates routing table
func (n *dhtNode) bootstrap() {
	if len(n.config.DHT.BootstrapNodes) == 0 {
		n.logger.Info("No bootstrap nodes configured")
		return
	}

	n.logger.Info("Starting bootstrap process",
		zap.Strings("bootstrap_nodes", n.config.DHT.BootstrapNodes))

	// Wait a bit for RPC server to be fully ready
	time.Sleep(2 * time.Second)

	for _, addr := range n.config.DHT.BootstrapNodes {
		// Skip self
		if addr == n.nodeInfo.Addr {
			continue
		}

		n.logger.Info("Bootstrapping to node", zap.String("addr", addr))

		// Try to ping the bootstrap node
		if err := n.Ping(addr); err != nil {
			n.logger.Warn("Failed to ping bootstrap node",
				zap.String("addr", addr), zap.Error(err))
			continue
		}

		// Perform FindNode for self to populate routing table
		n.logger.Info("Performing self-lookup to populate routing table")
		nodes, err := n.FindNode(n.nodeInfo.Id)
		if err != nil {
			n.logger.Error("Self-lookup failed", zap.Error(err))
			continue
		}

		n.logger.Info("Bootstrap completed",
			zap.String("bootstrap_addr", addr),
			zap.Int("discovered_nodes", len(nodes)))
		break // Successfully bootstrapped to one node
	}
}

// ID returns the node's 32-byte identifier
func (n *dhtNode) ID() []byte {
	return n.nodeInfo.Id
}

// Address returns the node's network address
func (n *dhtNode) Address() string {
	return n.nodeInfo.Addr
}

// RoutingTable returns the node's routing table
func (n *dhtNode) RoutingTable() RoutingTable {
	return n.routing
}

// DHT Operations - now with real network implementation

// Ping checks if a node is alive and reachable
func (n *dhtNode) Ping(addr string) error {
	msg := &RPCMessage{
		Type:    MessageTypePing,
		ID:      n.generateRequestID(),
		Sender:  n.nodeInfo,
		Payload: PingRequest{},
	}

	response, err := n.rpcClient.SendMessage(addr, msg)
	if err != nil {
		return fmt.Errorf("ping failed to %s: %w", addr, err)
	}

	if response.Type != MessageTypePong {
		return fmt.Errorf("unexpected response type: %s", response.Type)
	}

	// Add responding node to routing table
	n.routing.InsertNode(response.Sender)

	n.logger.Debug("Ping successful",
		zap.String("target", addr),
		zap.String("responder", fmt.Sprintf("%x", response.Sender.Id[:8])))

	return nil
}

// FindNode performs iterative node lookup to find nodes closest to target
func (n *dhtNode) FindNode(target []byte) ([]*NodeInfo, error) {
	n.logger.Debug("Starting FindNode lookup",
		zap.String("target", fmt.Sprintf("%x", target[:8])))

	// Start with K closest nodes from local routing table
	candidates := n.routing.ClosestK(target)
	contacted := make(map[string]bool)
	alpha := n.config.DHT.Alpha // Parallelism factor

	for iteration := 0; iteration < 10; iteration++ { // Max 10 iterations
		// Select up to alpha uncontacted nodes closest to target
		var toContact []*NodeInfo
		for _, node := range candidates {
			if len(toContact) >= alpha {
				break
			}
			if !contacted[string(node.Id)] && string(node.Id) != string(n.nodeInfo.Id) {
				toContact = append(toContact, node)
				contacted[string(node.Id)] = true
			}
		}

		if len(toContact) == 0 {
			break // No more nodes to contact
		}

		// Send FindNode requests in parallel
		responses := make(chan []*NodeInfo, len(toContact))
		for _, node := range toContact {
			go func(addr string) {
				nodes, err := n.findNodeSingle(target, addr)
				if err != nil {
					n.logger.Debug("FindNode request failed",
						zap.String("target_addr", addr), zap.Error(err))
					responses <- nil
					return
				}
				responses <- nodes
			}(node.Addr)
		}

		// Collect responses
		newNodes := make([]*NodeInfo, 0)
		for i := 0; i < len(toContact); i++ {
			select {
			case nodes := <-responses:
				if nodes != nil {
					newNodes = append(newNodes, nodes...)
				}
			case <-time.After(5 * time.Second):
				n.logger.Debug("FindNode request timed out")
			}
		}

		// Add new nodes to candidates and update routing table
		for _, node := range newNodes {
			n.routing.InsertNode(node)
			// Check if this node is closer than our current candidates
			candidates = append(candidates, node)
		}

		// Re-sort candidates by distance to target
		candidates = n.routing.ClosestK(target)
	}

	// Return K closest nodes found
	result := candidates
	if len(result) > n.config.DHT.K {
		result = result[:n.config.DHT.K]
	}

	n.logger.Debug("FindNode lookup complete",
		zap.String("target", fmt.Sprintf("%x", target[:8])),
		zap.Int("found_nodes", len(result)))

	return result, nil
}

// findNodeSingle sends a FindNode request to a single node
// findNodeSingle sends a FindNode request to a single node
func (n *dhtNode) findNodeSingle(target []byte, addr string) ([]*NodeInfo, error) {
	msg := &RPCMessage{
		Type:   MessageTypeFindNode,
		ID:     n.generateRequestID(),
		Sender: n.nodeInfo,
		Payload: FindNodeRequest{
			Target: hex.EncodeToString(target),
		},
	}

	response, err := n.rpcClient.SendMessage(addr, msg)
	if err != nil {
		return nil, err
	}

	if response.Type != MessageTypeFindNodeResp {
		return nil, fmt.Errorf("unexpected response type: %s", response.Type)
	}

	// Parse response payload
	payload, ok := response.Payload.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid response payload")
	}

	nodesData, ok := payload["nodes"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("missing nodes in response")
	}

	var nodes []*NodeInfo
	for _, nodeData := range nodesData {
		nodeMap, ok := nodeData.(map[string]interface{})
		if !ok {
			continue
		}

		idHex, ok := nodeMap["id"].(string)
		if !ok {
			continue
		}

		addr, ok := nodeMap["addr"].(string)
		if !ok {
			continue
		}

		// Decode hex string to bytes
		id, err := hex.DecodeString(idHex)
		if err != nil {
			n.logger.Warn("Invalid node ID hex", zap.Error(err)) // Fixed: n.logger instead of s.logger
			continue
		}

		nodes = append(nodes, &NodeInfo{
			Id:   id,
			Addr: addr,
		})
	}

	return nodes, nil
}

// Provider operations (stubs for Episode 3)
func (n *dhtNode) StoreProvider(hash []byte, provider *NodeInfo) error {
	n.logger.Debug("StoreProvider called (stub)",
		zap.String("hash", fmt.Sprintf("%x", hash[:8])),
		zap.String("provider", fmt.Sprintf("%x", provider.Id[:8])))

	// TODO Episode 3: Implement provider storage
	return nil
}

func (n *dhtNode) FindProviders(hash []byte) ([]*NodeInfo, error) {
	n.logger.Debug("FindProviders called (stub)",
		zap.String("hash", fmt.Sprintf("%x", hash[:8])))

	// TODO Episode 3: Implement provider lookup
	return []*NodeInfo{}, nil
}
