// Package session derives stable conversation identities and records hierarchical session trees.
package session

import (
	"container/list"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const (
	defaultMaxTreeNodes = 65536
	defaultTreeTTL      = 24 * time.Hour
	maxTreeDepth        = 128
)

// SessionTreeNode represents a node in the hierarchical session tree.
type SessionTreeNode struct {
	SessionID       string         `json:"session_id"`
	ParentSessionID string         `json:"parent_session_id,omitempty"`
	RootSessionID   string         `json:"root_session_id"`
	TreePath        string         `json:"tree_path"` // e.g. "root_id/parent_id/self_id"
	TreeDepth       int            `json:"tree_depth"`
	AgentName       string         `json:"agent_name,omitempty"`
	ClientType      string         `json:"client_type,omitempty"`
	CallerScope     string         `json:"caller_scope,omitempty"`
	LastAuthID      string         `json:"last_auth_id,omitempty"`
	LastProvider    string         `json:"last_provider,omitempty"`
	LastModel       string         `json:"last_model,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	Metadata        map[string]any `json:"metadata,omitempty"`
}

// Clone returns a deep copy of the node.
func (n *SessionTreeNode) Clone() *SessionTreeNode {
	if n == nil {
		return nil
	}
	res := *n
	if n.Metadata != nil {
		res.Metadata = cloneTreeMetadata(n.Metadata)
	}
	return &res
}

func cloneTreeMetadata(metadata map[string]any) map[string]any {
	if metadata == nil {
		return nil
	}
	cloned := make(map[string]any, len(metadata))
	for key, value := range metadata {
		cloned[key] = cloneTreeMetadataValue(value)
	}
	return cloned
}

func cloneTreeMetadataValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneTreeMetadata(typed)
	case map[string]string:
		cloned := make(map[string]string, len(typed))
		for k, v := range typed {
			cloned[k] = v
		}
		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneTreeMetadataValue(item)
		}
		return cloned
	case []string:
		return append([]string(nil), typed...)
	case []int:
		return append([]int(nil), typed...)
	case []byte:
		return append([]byte(nil), typed...)
	default:
		return value
	}
}

// SessionTreeInfo encapsulates incoming request details needed to record or update a tree node.
type SessionTreeInfo struct {
	SessionID       string
	ParentSessionID string
	AgentName       string
	ClientType      string
	CallerScope     string
	AuthID          string
	Provider        string
	Model           string
	Metadata        map[string]any
}

// ExtractTreeInfo extracts session hierarchy and client identification from request attributes.
func ExtractTreeInfo(headers http.Header, payload []byte, metadata map[string]any) (SessionTreeInfo, bool) {
	var info SessionTreeInfo
	if metadata != nil {
		if scope, ok := metadata[cliproxyexecutor.CallerScopeMetadataKey].(string); ok {
			info.CallerScope = strings.TrimSpace(scope)
		}
	}

	// 1. Client Type & Explicit Header Detection
	switch {
	case sessionHeaderValue(headers, "X-Claude-Code-Session-Id") != "":
		info.ClientType = "claude"
		rawSID := sessionHeaderValue(headers, "X-Claude-Code-Session-Id")
		agentID := sessionHeaderValue(headers, "X-Claude-Code-Agent-Id")
		parentAgentID := sessionHeaderValue(headers, "X-Claude-Code-Parent-Agent-Id")
		if agentID != "" && agentID != "main" {
			info.AgentName = agentID
			info.ParentSessionID = "claude:" + rawSID
			if parentAgentID != "" && parentAgentID != "main" && parentAgentID != agentID {
				info.ParentSessionID = "claude:" + rawSID + ":agent:" + parentAgentID
			}
			info.SessionID = "claude:" + rawSID + ":agent:" + agentID
		} else {
			info.AgentName = "main"
			info.SessionID = "claude:" + rawSID
		}

	case sessionHeaderValue(headers, "Session-Id") != "" || sessionHeaderValue(headers, "Session_id") != "":
		info.ClientType = "codex"
		sid := sessionHeaderValue(headers, "Session-Id")
		if sid == "" {
			sid = sessionHeaderValue(headers, "Session_id")
		}
		info.SessionID = "codex:" + sid
		parentThread := sessionHeaderValue(headers, "x-codex-parent-thread-id")
		if parentThread == "" {
			parentThread = sessionHeaderValue(headers, "X-Codex-Parent-Thread-Id")
		}
		if parentThread != "" && parentThread != sid {
			info.ParentSessionID = "codex:" + parentThread
			info.AgentName = "subagent"
		} else {
			info.AgentName = "main"
		}

	case sessionHeaderValue(headers, "X-Slot-Session-Id") != "":
		info.ClientType = "pi"
		sid := sessionHeaderValue(headers, "X-Slot-Session-Id")
		info.SessionID = "slot:" + sid
		parentSID := sessionHeaderValue(headers, "X-Parent-Session-ID")
		if parentSID == "" {
			parentSID = sessionHeaderValue(headers, "X-Parent-Session-Id")
		}
		if parentSID != "" && parentSID != sid {
			info.ParentSessionID = "slot:" + parentSID
			info.AgentName = "subagent"
		} else {
			info.AgentName = "slot"
		}

	case sessionHeaderValue(headers, "X-Http-Session-Id") != "":
		info.ClientType = "agy"
		sid := sessionHeaderValue(headers, "X-Http-Session-Id")
		info.SessionID = "agy:" + sid
		parentSID := sessionHeaderValue(headers, "X-Parent-Session-ID")
		if parentSID == "" {
			parentSID = sessionHeaderValue(headers, "X-Parent-Session-Id")
		}
		if parentSID != "" && parentSID != sid {
			info.ParentSessionID = "agy:" + parentSID
			info.AgentName = "subagent"
		} else {
			info.AgentName = "main"
		}

	case sessionHeaderValue(headers, "X-Session-ID") != "":
		info.ClientType = "generic"
		sid := sessionHeaderValue(headers, "X-Session-ID")
		info.SessionID = "header:" + sid
		parentSID := sessionHeaderValue(headers, "X-Parent-Session-ID")
		if parentSID == "" {
			parentSID = sessionHeaderValue(headers, "X-Parent-Session-Id")
		}
		if parentSID != "" && parentSID != sid {
			info.ParentSessionID = "header:" + parentSID
			info.AgentName = "subagent"
		} else {
			info.AgentName = "main"
		}

	case sessionHeaderValue(headers, "X-Session-Affinity") != "":
		info.ClientType = "opencode"
		sid := sessionHeaderValue(headers, "X-Session-Affinity")
		info.SessionID = "affinity:" + sid
		parentAffinity := sessionHeaderValue(headers, "X-Parent-Session-Affinity")
		if parentAffinity == "" {
			parentAffinity = sessionHeaderValue(headers, "X-Parent-Session-ID")
		}
		if parentAffinity != "" && parentAffinity != sid {
			info.ParentSessionID = "affinity:" + parentAffinity
			info.AgentName = "subagent"
		} else {
			info.AgentName = "main"
		}

	case sessionHeaderValue(headers, "X-Thread-Id") != "" || sessionHeaderValue(headers, "X-Thread-ID") != "" || sessionHeaderValue(headers, "Thread-Id") != "":
		info.ClientType = "openai-thread"
		tid := sessionHeaderValue(headers, "X-Thread-Id")
		if tid == "" {
			tid = sessionHeaderValue(headers, "X-Thread-ID")
		}
		if tid == "" {
			tid = sessionHeaderValue(headers, "Thread-Id")
		}
		info.SessionID = "thread:" + tid
		info.AgentName = "main"

	case sessionHeaderValue(headers, "X-Conversation-Id") != "" || sessionHeaderValue(headers, "X-Conversation-ID") != "":
		info.ClientType = "conv"
		cid := sessionHeaderValue(headers, "X-Conversation-Id")
		if cid == "" {
			cid = sessionHeaderValue(headers, "X-Conversation-ID")
		}
		info.SessionID = "conv:" + cid
		info.AgentName = "main"
	}

	// 2. Payload Inspection if headers didn't fully resolve
	if len(payload) > 0 {
		root := util.ParseGJSONBytesNoCopy(payload)
		var parentCandidate string
		for _, p := range []string{
			"parent_session_id", "parentSessionId",
			"parent_thread_id", "parentThreadId",
			"forked_from_thread_id", "forked_from_id",
			"parent_conversation_id", "parentConversationId",
			"metadata.parent_session_id", "metadata.parent_thread_id",
			"extra_body.parent_session_id", "extra_body.parent_thread_id",
		} {
			if val := normalizedSessionCandidate(root.Get(p).String()); val != "" {
				parentCandidate = val
				break
			}
		}
		if parentCandidate == "" {
			parentCandidate = ClaudeMetadataParentSessionID(payload)
		}

		if info.SessionID == "" {
			// Claude metadata.user_id
			if sid, parentSID, agentID := ClaudeMetadataIdentities(payload); sid != "" {
				info.ClientType = "claude"
				if agentID == "" {
					agentID = normalizedSessionCandidate(root.Get("metadata.agent_id").String())
				}
				if agentID != "" && agentID != "main" {
					info.SessionID = "claude:" + sid + ":agent:" + agentID
					info.ParentSessionID = "claude:" + sid
					if parentSID != "" && parentSID != sid {
						info.ParentSessionID = "claude:" + parentSID
					}
					info.AgentName = agentID
				} else {
					info.SessionID = "claude:" + sid
					if parentSID != "" && parentSID != sid {
						info.ParentSessionID = "claude:" + parentSID
						info.AgentName = "subagent"
					} else {
						info.AgentName = "main"
					}
				}
			}

			// Gemini context caching
			if info.SessionID == "" {
				for _, cachePath := range []string{"cachedContent", "cached_content"} {
					if cacheID := normalizedSessionCandidate(root.Get(cachePath).String()); cacheID != "" {
						info.ClientType = "gemini"
						info.SessionID = "geminicache:" + cacheID
						if parentCandidate != "" && parentCandidate != cacheID {
							info.ParentSessionID = "geminicache:" + parentCandidate
							info.AgentName = "subagent"
						} else {
							info.AgentName = "main"
						}
						break
					}
				}
			}

			// OpenAI thread in payload
			if info.SessionID == "" {
				for _, threadPath := range []string{"thread_id", "threadId", "metadata.thread_id"} {
					if tid := normalizedSessionCandidate(root.Get(threadPath).String()); tid != "" {
						info.ClientType = "openai-thread"
						info.SessionID = "thread:" + tid
						if parentCandidate != "" && parentCandidate != tid {
							info.ParentSessionID = "thread:" + parentCandidate
							info.AgentName = "subagent"
						} else {
							info.AgentName = "main"
						}
						break
					}
				}
			}

			// Generic session in payload
			if info.SessionID == "" {
				agentID := normalizedSessionCandidate(root.Get("metadata.agent_id").String())
				if agentID == "" {
					agentID = normalizedSessionCandidate(root.Get("metadata.subagent_id").String())
				}
				for _, path := range []string{"session_id", "sessionId", "sessionID", "metadata.session_id", "extra_body.session_id"} {
					if sid := normalizedSessionCandidate(root.Get(path).String()); sid != "" {
						info.ClientType = "generic"
						if agentID != "" && agentID != "main" {
							info.SessionID = "session:" + sid + ":agent:" + agentID
							info.ParentSessionID = "session:" + sid
							if parentCandidate != "" && parentCandidate != sid {
								info.ParentSessionID = "session:" + parentCandidate
							}
							info.AgentName = agentID
						} else {
							info.SessionID = "session:" + sid
							if parentCandidate != "" && parentCandidate != sid {
								info.ParentSessionID = "session:" + parentCandidate
								info.AgentName = "subagent"
							} else {
								info.AgentName = "main"
							}
						}
						break
					}
				}
			}

			// Conversation paths
			if info.SessionID == "" {
				for _, convPath := range []string{"conversation_id", "conversationId", "chat_id", "chatId", "metadata.conversation_id", "extra_body.conversation_id"} {
					if cid := normalizedSessionCandidate(root.Get(convPath).String()); cid != "" {
						info.ClientType = "conv"
						info.SessionID = "conv:" + cid
						if parentCandidate != "" && parentCandidate != cid {
							info.ParentSessionID = "conv:" + parentCandidate
							info.AgentName = "subagent"
						} else {
							info.AgentName = "main"
						}
						break
					}
				}
			}
		} else if info.ParentSessionID == "" && parentCandidate != "" {
			info.ParentSessionID = canonicalParentSessionID(info.ClientType, parentCandidate)
			if info.AgentName == "main" || info.AgentName == "" {
				info.AgentName = "subagent"
			}
		}
	}

	if info.SessionID == "" {
		return SessionTreeInfo{}, false
	}
	if info.AgentName == "" {
		info.AgentName = "main"
	}
	if info.ClientType == "" {
		info.ClientType = "generic"
	}
	return info, true
}

func sessionHeaderValue(headers http.Header, name string) string {
	if headers == nil {
		return ""
	}
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, value := range values {
			if normalized := normalizedSessionCandidate(value); normalized != "" {
				return normalized
			}
		}
	}
	return ""
}

func normalizedSessionCandidate(raw string) string {
	return NormalizeExplicitID(raw)
}

func canonicalParentSessionID(clientType, raw string) string {
	raw = normalizedSessionCandidate(raw)
	if raw == "" {
		return ""
	}

	var scheme string
	switch clientType {
	case "agy":
		scheme = "agy"
	case "claude":
		scheme = "claude"
	case "codex":
		scheme = "codex"
	case "conv":
		scheme = "conv"
	case "gemini":
		scheme = "geminicache"
	case "generic":
		scheme = "header"
	case "openai-thread":
		scheme = "thread"
	case "opencode":
		scheme = "affinity"
	case "pi":
		scheme = "slot"
	}
	if scheme != "" {
		if strings.HasPrefix(raw, scheme+":") {
			return raw
		}
		return scheme + ":" + raw
	}
	return raw
}

// SessionTreeStore defines the interface for recording and querying session trees.
type SessionTreeStore interface {
	RecordNode(info SessionTreeInfo) *SessionTreeNode
	GetNode(sessionID string) (*SessionTreeNode, bool)
	GetTree(rootSessionID string) []*SessionTreeNode
	GetSubtree(sessionID string) []*SessionTreeNode
	Ancestors(sessionID string) []string
	UpdateAffinity(sessionID, authID, provider, model string) bool
	Len() int
	Clear()
}

// InMemorySessionTreeStore is a bounded, concurrent-safe in-memory session tree store.
type InMemorySessionTreeStore struct {
	mu          sync.RWMutex
	maxNodes    int
	ttl         time.Duration
	nodes       map[string]*SessionTreeNode
	rootIndex   map[string]map[string]struct{} // root_session_id -> set of session_ids
	parentIndex map[string]map[string]struct{} // parent_session_id -> set of direct child session_ids
	lru         *list.List
	lruElements map[string]*list.Element
}

type lruItem struct {
	sessionID string
	expiresAt time.Time
}

// NewInMemorySessionTreeStore creates a new in-memory session tree store with bounded limits.
func NewInMemorySessionTreeStore(maxNodes int, ttl time.Duration) *InMemorySessionTreeStore {
	if maxNodes <= 0 {
		maxNodes = defaultMaxTreeNodes
	}
	if ttl <= 0 {
		ttl = defaultTreeTTL
	}
	return &InMemorySessionTreeStore{
		maxNodes:    maxNodes,
		ttl:         ttl,
		nodes:       make(map[string]*SessionTreeNode),
		rootIndex:   make(map[string]map[string]struct{}),
		parentIndex: make(map[string]map[string]struct{}),
		lru:         list.New(),
		lruElements: make(map[string]*list.Element),
	}
}

// RecordNode atomically creates or updates a session tree node with materialized paths.
func (s *InMemorySessionTreeStore) RecordNode(info SessionTreeInfo) *SessionTreeNode {
	sessionID := strings.TrimSpace(info.SessionID)
	if sessionID == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.cleanupExpiredLocked(now)
	expiresAt := now.Add(s.ttl)

	// Update existing node
	if existing, ok := s.nodes[sessionID]; ok {
		existing.UpdatedAt = now
		if info.AuthID != "" {
			existing.LastAuthID = info.AuthID
		}
		if info.Provider != "" {
			existing.LastProvider = info.Provider
		}
		if info.Model != "" {
			existing.LastModel = info.Model
		}
		if info.AgentName != "" && existing.AgentName == "main" {
			existing.AgentName = info.AgentName
		}
		if info.ClientType != "" && existing.ClientType == "generic" {
			existing.ClientType = info.ClientType
		}
		if parentID := strings.TrimSpace(info.ParentSessionID); parentID != "" && parentID != existing.ParentSessionID && parentID != existing.SessionID && !s.isDescendantLocked(parentID, existing.SessionID) {
			oldParent := existing.ParentSessionID
			existing.ParentSessionID = parentID
			s.updateParentIndexLocked(existing.SessionID, oldParent, existing.ParentSessionID)
			oldRoot, changed := s.computeNodeLineageLocked(existing)
			if changed {
				s.updateRootIndexLocked(existing.SessionID, oldRoot, existing.RootSessionID)
			}
			s.cascadeDescendantsLineageLocked(existing.SessionID)
		}
		if elem, ok := s.lruElements[sessionID]; ok {
			elem.Value = lruItem{sessionID: sessionID, expiresAt: expiresAt}
			s.lru.MoveToBack(elem)
		}
		return existing.Clone()
	}

	// Create new node
	node := &SessionTreeNode{
		SessionID:       sessionID,
		ParentSessionID: strings.TrimSpace(info.ParentSessionID),
		AgentName:       info.AgentName,
		ClientType:      info.ClientType,
		CallerScope:     info.CallerScope,
		LastAuthID:      info.AuthID,
		LastProvider:    info.Provider,
		LastModel:       info.Model,
		CreatedAt:       now,
		UpdatedAt:       now,
		Metadata:        cloneTreeMetadata(info.Metadata),
	}
	if node.AgentName == "" {
		node.AgentName = "main"
	}
	if node.ClientType == "" {
		node.ClientType = "generic"
	}

	// Compute RootSessionID, TreePath, and TreeDepth
	s.computeNodeLineageLocked(node)

	s.nodes[sessionID] = node
	if node.ParentSessionID != "" {
		s.updateParentIndexLocked(sessionID, "", node.ParentSessionID)
	}

	// Index by root
	s.updateRootIndexLocked(sessionID, "", node.RootSessionID)

	// Cascade lineage in case children were recorded before this node arrived
	s.cascadeDescendantsLineageLocked(sessionID)

	// LRU tracking
	elem := s.lru.PushBack(lruItem{sessionID: sessionID, expiresAt: expiresAt})
	s.lruElements[sessionID] = elem

	// Evict if capacity exceeded
	for len(s.nodes) > s.maxNodes {
		s.evictOldestLocked()
	}

	return node.Clone()
}

// GetNode returns a clone of the specified node if it exists.
func (s *InMemorySessionTreeStore) GetNode(sessionID string) (*SessionTreeNode, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupExpiredLocked(time.Now())
	node, ok := s.nodes[sessionID]
	if !ok {
		return nil, false
	}
	return node.Clone(), true
}

// GetTree returns all nodes belonging to the same root session, ordered by depth and created time.
func (s *InMemorySessionTreeStore) GetTree(rootSessionID string) []*SessionTreeNode {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupExpiredLocked(time.Now())

	set := s.rootIndex[rootSessionID]
	if len(set) == 0 {
		return nil
	}

	res := make([]*SessionTreeNode, 0, len(set))
	for sid := range set {
		if node, ok := s.nodes[sid]; ok {
			res = append(res, node.Clone())
		}
	}

	sort.Slice(res, func(i, j int) bool {
		if res[i].TreeDepth != res[j].TreeDepth {
			return res[i].TreeDepth < res[j].TreeDepth
		}
		return res[i].CreatedAt.Before(res[j].CreatedAt)
	})
	return res
}

// GetSubtree returns the specified node and all of its descendants by parent linkage.
// Parent-link traversal is used instead of TreePath prefix matching because session IDs
// may contain "/". The cost is O(rootSet x depth), which is acceptable for this
// management/debug-oriented accessor.
func (s *InMemorySessionTreeStore) GetSubtree(sessionID string) []*SessionTreeNode {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupExpiredLocked(time.Now())

	target, ok := s.nodes[sessionID]
	if !ok {
		return nil
	}

	res := []*SessionTreeNode{target.Clone()}

	set := s.rootIndex[target.RootSessionID]
	for sid := range set {
		if sid == sessionID {
			continue
		}
		if node, ok := s.nodes[sid]; ok && s.isDescendantLocked(node.SessionID, sessionID) {
			res = append(res, node.Clone())
		}
	}

	sort.Slice(res, func(i, j int) bool {
		if res[i].TreeDepth != res[j].TreeDepth {
			return res[i].TreeDepth < res[j].TreeDepth
		}
		return res[i].CreatedAt.Before(res[j].CreatedAt)
	})
	return res
}

// Ancestors returns the sequence of ancestor session IDs from root to direct parent without
// database lookup. The chain may include IDs of ancestors that already expired or were
// evicted, so callers must not assume every returned ID resolves via GetNode.
func (s *InMemorySessionTreeStore) Ancestors(sessionID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupExpiredLocked(time.Now())

	node, ok := s.nodes[sessionID]
	if !ok || node.ParentSessionID == "" {
		return nil
	}

	ancestors := make([]string, 0, node.TreeDepth)
	visited := make(map[string]struct{}, node.TreeDepth+1)
	visited[sessionID] = struct{}{}
	parentID := node.ParentSessionID
	for steps := 0; parentID != "" && steps <= len(s.nodes); steps++ {
		if _, seen := visited[parentID]; seen {
			break
		}
		visited[parentID] = struct{}{}
		ancestors = append(ancestors, parentID)
		parent, exists := s.nodes[parentID]
		if !exists {
			break
		}
		parentID = parent.ParentSessionID
	}
	for left, right := 0, len(ancestors)-1; left < right; left, right = left+1, right-1 {
		ancestors[left], ancestors[right] = ancestors[right], ancestors[left]
	}
	return ancestors
}

// UpdateAffinity updates the latest successful upstream binding for a session node.
func (s *InMemorySessionTreeStore) UpdateAffinity(sessionID, authID, provider, model string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.cleanupExpiredLocked(now)

	node, ok := s.nodes[sessionID]
	if !ok {
		return false
	}
	node.UpdatedAt = now
	if elem, exists := s.lruElements[sessionID]; exists {
		elem.Value = lruItem{sessionID: sessionID, expiresAt: now.Add(s.ttl)}
		s.lru.MoveToBack(elem)
	}
	if authID != "" {
		node.LastAuthID = authID
	}
	if provider != "" {
		node.LastProvider = provider
	}
	if model != "" {
		node.LastModel = model
	}
	return true
}

func (s *InMemorySessionTreeStore) evictOldestLocked() {
	oldest := s.lru.Front()
	if oldest == nil {
		return
	}
	item, ok := oldest.Value.(lruItem)
	if !ok {
		s.lru.Remove(oldest)
		return
	}
	s.removeNodeLocked(item.sessionID)
}

func (s *InMemorySessionTreeStore) cleanupExpiredLocked(now time.Time) {
	for element := s.lru.Front(); element != nil; {
		item, ok := element.Value.(lruItem)
		if !ok {
			next := element.Next()
			s.lru.Remove(element)
			element = next
			continue
		}
		// Every touch sets expiresAt = touch + the same TTL and moves the element to the
		// LRU tail, so LRU order is exactly expiresAt order and expired nodes form a
		// front prefix. Stop at the first live node to keep this amortized O(expired).
		if now.Before(item.expiresAt) {
			return
		}
		next := element.Next()
		s.removeNodeLocked(item.sessionID)
		element = next
	}
}

func (s *InMemorySessionTreeStore) removeNodeLocked(sessionID string) {
	if element := s.lruElements[sessionID]; element != nil {
		s.lru.Remove(element)
		delete(s.lruElements, sessionID)
	}
	if node, ok := s.nodes[sessionID]; ok {
		delete(s.nodes, sessionID)
		s.updateParentIndexLocked(sessionID, node.ParentSessionID, "")
		if set := s.rootIndex[node.RootSessionID]; set != nil {
			delete(set, sessionID)
			if len(set) == 0 {
				delete(s.rootIndex, node.RootSessionID)
			}
		}
	}
}

func (s *InMemorySessionTreeStore) computeNodeLineageLocked(node *SessionTreeNode) (oldRoot string, changed bool) {
	oldRoot = node.RootSessionID
	var newRoot, newPath string
	var newDepth int

	if node.ParentSessionID != "" && node.ParentSessionID != node.SessionID && !s.isDescendantLocked(node.ParentSessionID, node.SessionID) {
		if parent, ok := s.nodes[node.ParentSessionID]; ok {
			if parent.TreeDepth < maxTreeDepth {
				newRoot = parent.RootSessionID
				newPath = parent.TreePath + "/" + node.SessionID
				newDepth = parent.TreeDepth + 1
			} else {
				node.ParentSessionID = ""
				newRoot = node.SessionID
				newPath = node.SessionID
				newDepth = 0
			}
		} else {
			newRoot = node.ParentSessionID
			newPath = node.ParentSessionID + "/" + node.SessionID
			newDepth = 1
		}
	} else {
		node.ParentSessionID = ""
		newRoot = node.SessionID
		newPath = node.SessionID
		newDepth = 0
	}

	if node.RootSessionID != newRoot || node.TreePath != newPath || node.TreeDepth != newDepth {
		node.RootSessionID = newRoot
		node.TreePath = newPath
		node.TreeDepth = newDepth
		changed = true
	}
	return oldRoot, changed
}

func (s *InMemorySessionTreeStore) updateRootIndexLocked(sessionID, oldRoot, newRoot string) {
	if oldRoot != "" && oldRoot != newRoot {
		if set := s.rootIndex[oldRoot]; set != nil {
			delete(set, sessionID)
			if len(set) == 0 {
				delete(s.rootIndex, oldRoot)
			}
		}
	}
	if newRoot != "" {
		set := s.rootIndex[newRoot]
		if set == nil {
			set = make(map[string]struct{})
			s.rootIndex[newRoot] = set
		}
		set[sessionID] = struct{}{}
	}
}

func (s *InMemorySessionTreeStore) updateParentIndexLocked(sessionID, oldParent, newParent string) {
	if oldParent != "" && oldParent != newParent {
		if set := s.parentIndex[oldParent]; set != nil {
			delete(set, sessionID)
			if len(set) == 0 {
				delete(s.parentIndex, oldParent)
			}
		}
	}
	if newParent != "" {
		set := s.parentIndex[newParent]
		if set == nil {
			set = make(map[string]struct{})
			s.parentIndex[newParent] = set
		}
		set[sessionID] = struct{}{}
	}
}

func (s *InMemorySessionTreeStore) cascadeDescendantsLineageLocked(parentID string) {
	visited := make(map[string]struct{})
	s.cascadeDescendantsHelperLocked(parentID, visited)
}

func (s *InMemorySessionTreeStore) cascadeDescendantsHelperLocked(parentID string, visited map[string]struct{}) {
	if _, seen := visited[parentID]; seen {
		return
	}
	visited[parentID] = struct{}{}

	childrenSet := s.parentIndex[parentID]
	if len(childrenSet) == 0 {
		return
	}
	childIDs := make([]string, 0, len(childrenSet))
	for childID := range childrenSet {
		childIDs = append(childIDs, childID)
	}

	for _, childID := range childIDs {
		child := s.nodes[childID]
		if child != nil && child.ParentSessionID == parentID {
			oldRoot, changed := s.computeNodeLineageLocked(child)
			if changed {
				s.updateRootIndexLocked(child.SessionID, oldRoot, child.RootSessionID)
			}
			s.cascadeDescendantsHelperLocked(child.SessionID, visited)
		}
	}
}

func (s *InMemorySessionTreeStore) isDescendantLocked(sessionID, ancestorID string) bool {
	if sessionID == "" || ancestorID == "" || sessionID == ancestorID {
		return false
	}
	visited := make(map[string]struct{})
	visited[sessionID] = struct{}{}
	currentID := sessionID
	for steps := 0; currentID != "" && steps <= len(s.nodes); steps++ {
		node, ok := s.nodes[currentID]
		if !ok {
			return false
		}
		if node.ParentSessionID == ancestorID {
			return true
		}
		if _, seen := visited[node.ParentSessionID]; seen {
			return false
		}
		visited[node.ParentSessionID] = struct{}{}
		currentID = node.ParentSessionID
	}
	return false
}

// Len returns the current count of tracked session tree nodes.
func (s *InMemorySessionTreeStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupExpiredLocked(time.Now())
	return len(s.nodes)
}

// Clear flushes all tracked nodes.
func (s *InMemorySessionTreeStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes = make(map[string]*SessionTreeNode)
	s.rootIndex = make(map[string]map[string]struct{})
	s.parentIndex = make(map[string]map[string]struct{})
	s.lru = list.New()
	s.lruElements = make(map[string]*list.Element)
}
