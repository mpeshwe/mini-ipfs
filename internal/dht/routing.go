package dht

import (
	"bytes"
	"errors"
	"sync"
)

// Bucket represents a k-bucket containing up to k nodes
type Bucket struct {
	nodes []*NodeInfo
}

// routingTable implements the RoutingTable interface
type routingTable struct {
	self    *NodeInfo
	k       int
	buckets []*Bucket
	mutex   sync.Mutex
	depth   int // tracks active bucket levels
}

// NewRoutingTable creates a new routing table for the given node
func NewRoutingTable(node *NodeInfo, k int) (RoutingTable, error) {
	if node == nil || len(node.Id) != KeyBytes {
		return nil, ErrInvalidNode
	}
	if k <= 0 {
		return nil, errors.New("k must be positive")
	}

	rt := &routingTable{
		self:    node,
		k:       k,
		buckets: make([]*Bucket, 256), // 256 buckets for 256-bit keys
		depth:   0,
	}

	// Initialize most distant bucket (255) with self
	rt.buckets[255] = &Bucket{
		nodes: []*NodeInfo{node},
	}

	return rt, nil
}

// XOR distance between two keys
func xorDistance(a, b []byte) []byte {
	if len(a) != len(b) {
		return nil
	}
	result := make([]byte, len(a))
	for i := range a {
		result[i] = a[i] ^ b[i]
	}
	return result
}

// Find the bit position of the most significant differing bit
func distanceBucket(distance []byte) int {
	for i, b := range distance {
		if b != 0 {
			for j := 7; j >= 0; j-- {
				if (b & (1 << uint(j))) != 0 {
					return (len(distance)-1-i)*8 + j
				}
			}
		}
	}
	return 0
}

// ensureBucket creates a bucket if it doesn't exist
func (rt *routingTable) ensureBucket(index int) {
	if rt.buckets[index] == nil {
		rt.buckets[index] = &Bucket{
			nodes: make([]*NodeInfo, 0, rt.k),
		}
	}
}

// getBucketIndex determines which bucket a node belongs in
func (rt *routingTable) getBucketIndex(nodeId []byte) int {
	distance := xorDistance(rt.self.Id, nodeId)
	standardBucket := distanceBucket(distance)

	maxActiveBucket := 255
	minActiveBucket := 255 - rt.depth

	if standardBucket >= minActiveBucket && standardBucket <= maxActiveBucket {
		return standardBucket
	}
	if standardBucket > maxActiveBucket {
		return maxActiveBucket
	}
	return minActiveBucket
}

// InsertNode adds a node to the routing table
func (rt *routingTable) InsertNode(node *NodeInfo) {
	rt.mutex.Lock()
	defer rt.mutex.Unlock()

	// Don't insert self
	if bytes.Equal(node.Id, rt.self.Id) {
		return
	}

	bucketIndex := rt.getBucketIndex(node.Id)
	rt.ensureBucket(bucketIndex)
	bucket := rt.buckets[bucketIndex]

	// Update existing node
	for i, existing := range bucket.nodes {
		if bytes.Equal(existing.Id, node.Id) {
			bucket.nodes[i] = node
			return
		}
	}

	// Add if space available
	if len(bucket.nodes) < rt.k {
		bucket.nodes = append(bucket.nodes, node)
		return
	}

	// Split bucket if it contains local node
	containsLocal := false
	for _, existing := range bucket.nodes {
		if bytes.Equal(existing.Id, rt.self.Id) {
			containsLocal = true
			break
		}
	}

	if containsLocal {
		rt.splitBucket(bucketIndex)
		rt.insertNodeInternal(node)
	}
}

// splitBucket creates a new bucket and redistributes nodes
func (rt *routingTable) splitBucket(bucketIndex int) {
	currentDepth := 255 - bucketIndex
	if currentDepth >= 255 {
		return
	}

	if currentDepth >= rt.depth {
		rt.depth = currentDepth + 1
	}

	newBucketIndex := bucketIndex - 1
	rt.ensureBucket(newBucketIndex)

	oldBucket := rt.buckets[bucketIndex]
	var remainingNodes []*NodeInfo

	for _, node := range oldBucket.nodes {
		newBucketForNode := rt.getBucketIndex(node.Id)
		if newBucketForNode == newBucketIndex {
			rt.buckets[newBucketIndex].nodes = append(rt.buckets[newBucketIndex].nodes, node)
		} else {
			remainingNodes = append(remainingNodes, node)
		}
	}

	oldBucket.nodes = remainingNodes
}

// insertNodeInternal handles insertion after bucket splits
func (rt *routingTable) insertNodeInternal(node *NodeInfo) {
	bucketIndex := rt.getBucketIndex(node.Id)
	rt.ensureBucket(bucketIndex)
	bucket := rt.buckets[bucketIndex]

	for i, existing := range bucket.nodes {
		if bytes.Equal(existing.Id, node.Id) {
			bucket.nodes[i] = node
			return
		}
	}

	if len(bucket.nodes) < rt.k {
		bucket.nodes = append(bucket.nodes, node)
	}
}

// RemoveNode removes a node from the routing table
func (rt *routingTable) RemoveNode(key []byte) error {
	rt.mutex.Lock()
	defer rt.mutex.Unlock()

	if bytes.Equal(key, rt.self.Id) {
		return ErrInvalidNode
	}

	bucketIndex := rt.getBucketIndex(key)
	if rt.buckets[bucketIndex] == nil {
		return ErrNotFound
	}

	bucket := rt.buckets[bucketIndex]
	for i, node := range bucket.nodes {
		if bytes.Equal(node.Id, key) {
			bucket.nodes = append(bucket.nodes[:i], bucket.nodes[i+1:]...)
			return nil
		}
	}

	return ErrNotFound
}

// Lookup finds a specific node by ID
func (rt *routingTable) Lookup(key []byte) (node *NodeInfo, ok bool) {
	rt.mutex.Lock()
	defer rt.mutex.Unlock()

	if bytes.Equal(rt.self.Id, key) {
		return rt.self, true
	}

	bucketIndex := rt.getBucketIndex(key)
	if rt.buckets[bucketIndex] == nil {
		return nil, false
	}

	for _, existing := range rt.buckets[bucketIndex].nodes {
		if bytes.Equal(key, existing.Id) {
			return existing, true
		}
	}

	return nil, false
}

// GetNodes returns all nodes in a specific bucket
func (rt *routingTable) GetNodes(bucket int) []*NodeInfo {
	rt.mutex.Lock()
	defer rt.mutex.Unlock()

	if bucket < 0 || bucket >= len(rt.buckets) || rt.buckets[bucket] == nil {
		return []*NodeInfo{}
	}

	result := make([]*NodeInfo, len(rt.buckets[bucket].nodes))
	copy(result, rt.buckets[bucket].nodes)
	return result
}

// ClosestK returns the k closest nodes to a target key
func (rt *routingTable) ClosestK(key []byte) []*NodeInfo {
	rt.mutex.Lock()
	defer rt.mutex.Unlock()

	var candidates []*NodeInfo
	candidates = append(candidates, rt.self)

	// Find starting bucket
	distance := xorDistance(rt.self.Id, key)
	startBucket := distanceBucket(distance)

	// Collect candidates from nearby buckets
	visited := make(map[int]bool)
	for radius := 0; len(candidates) < rt.k*3 && radius <= 128; radius++ {
		bucketsToCheck := []int{}
		if radius == 0 {
			bucketsToCheck = append(bucketsToCheck, startBucket)
		} else {
			if startBucket+radius < 256 {
				bucketsToCheck = append(bucketsToCheck, startBucket+radius)
			}
			if startBucket-radius >= 0 {
				bucketsToCheck = append(bucketsToCheck, startBucket-radius)
			}
		}

		for _, bucketIndex := range bucketsToCheck {
			if visited[bucketIndex] || rt.buckets[bucketIndex] == nil {
				continue
			}
			visited[bucketIndex] = true
			for _, node := range rt.buckets[bucketIndex].nodes {
				if !bytes.Equal(rt.self.Id, node.Id) {
					candidates = append(candidates, node)
				}
			}
		}
	}

	// Sort by distance
	type nodeDistance struct {
		node     *NodeInfo
		distance []byte
	}
	var nodeDistances []nodeDistance

	for _, node := range candidates {
		dist := xorDistance(key, node.Id)
		nodeDistances = append(nodeDistances, nodeDistance{
			node:     node,
			distance: dist,
		})
	}

	// Simple sort by distance
	for i := 0; i < len(nodeDistances); i++ {
		for j := i + 1; j < len(nodeDistances); j++ {
			if isDistanceSmaller(nodeDistances[j].distance, nodeDistances[i].distance) {
				nodeDistances[i], nodeDistances[j] = nodeDistances[j], nodeDistances[i]
			}
		}
	}

	// Return k closest
	result := make([]*NodeInfo, 0, rt.k)
	limit := rt.k
	if len(nodeDistances) < limit {
		limit = len(nodeDistances)
	}

	for i := 0; i < limit; i++ {
		result = append(result, nodeDistances[i].node)
	}

	return result
}

// Helper to compare distances
func isDistanceSmaller(a, b []byte) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] < b[i] {
			return true
		}
		if a[i] > b[i] {
			return false
		}
	}
	return false
}

// Buckets returns number of non-empty buckets
func (rt *routingTable) Buckets() int {
	rt.mutex.Lock()
	defer rt.mutex.Unlock()

	count := 0
	for i := 255; i >= 0; i-- {
		if rt.buckets[i] != nil && len(rt.buckets[i].nodes) > 0 {
			count++
		}
	}
	return count
}

// K returns the k parameter
func (rt *routingTable) K() int {
	return rt.k
}
