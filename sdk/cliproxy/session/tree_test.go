package session

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestSessionTreeStoreRootAndChildHierarchy(t *testing.T) {
	t.Parallel()

	store := NewInMemorySessionTreeStore(1000, time.Hour)

	// 1. Root Node
	rootInfo := SessionTreeInfo{
		SessionID:  "session-root",
		AgentName:  "main",
		ClientType: "pi",
		AuthID:     "auth-1",
		Provider:   "claude",
		Model:      "claude-3-7-sonnet",
	}
	rootNode := store.RecordNode(rootInfo)
	if rootNode == nil {
		t.Fatalf("expected rootNode to be non-nil")
	}
	if rootNode.RootSessionID != "session-root" {
		t.Errorf("rootNode.RootSessionID = %q, want %q", rootNode.RootSessionID, "session-root")
	}
	if rootNode.TreePath != "session-root" {
		t.Errorf("rootNode.TreePath = %q, want %q", rootNode.TreePath, "session-root")
	}
	if rootNode.TreeDepth != 0 {
		t.Errorf("rootNode.TreeDepth = %d, want 0", rootNode.TreeDepth)
	}

	// 2. Child 1 (Subagent under root)
	child1Info := SessionTreeInfo{
		SessionID:       "session-child-1",
		ParentSessionID: "session-root",
		AgentName:       "code-reviewer",
		ClientType:      "pi",
	}
	child1Node := store.RecordNode(child1Info)
	if child1Node == nil {
		t.Fatalf("expected child1Node to be non-nil")
	}
	if child1Node.RootSessionID != "session-root" {
		t.Errorf("child1Node.RootSessionID = %q, want %q", child1Node.RootSessionID, "session-root")
	}
	if child1Node.TreePath != "session-root/session-child-1" {
		t.Errorf("child1Node.TreePath = %q, want %q", child1Node.TreePath, "session-root/session-child-1")
	}
	if child1Node.TreeDepth != 1 {
		t.Errorf("child1Node.TreeDepth = %d, want 1", child1Node.TreeDepth)
	}

	// 3. Child 2 (Grandchild, subagent under Child 1)
	child2Info := SessionTreeInfo{
		SessionID:       "session-child-2",
		ParentSessionID: "session-child-1",
		AgentName:       "test-runner",
		ClientType:      "pi",
	}
	child2Node := store.RecordNode(child2Info)
	if child2Node == nil {
		t.Fatalf("expected child2Node to be non-nil")
	}
	if child2Node.RootSessionID != "session-root" {
		t.Errorf("child2Node.RootSessionID = %q, want %q", child2Node.RootSessionID, "session-root")
	}
	if child2Node.TreePath != "session-root/session-child-1/session-child-2" {
		t.Errorf("child2Node.TreePath = %q, want %q", child2Node.TreePath, "session-root/session-child-1/session-child-2")
	}
	if child2Node.TreeDepth != 2 {
		t.Errorf("child2Node.TreeDepth = %d, want 2", child2Node.TreeDepth)
	}

	// 4. Child 3 (Sibling under Root)
	child3Info := SessionTreeInfo{
		SessionID:       "session-child-3",
		ParentSessionID: "session-root",
		AgentName:       "explorer",
		ClientType:      "pi",
	}
	child3Node := store.RecordNode(child3Info)
	if child3Node.TreePath != "session-root/session-child-3" {
		t.Errorf("child3Node.TreePath = %q, want %q", child3Node.TreePath, "session-root/session-child-3")
	}
	if child3Node.TreeDepth != 1 {
		t.Errorf("child3Node.TreeDepth = %d, want 1", child3Node.TreeDepth)
	}
}

func TestSessionTreeStoreGetTreeAndSubtree(t *testing.T) {
	t.Parallel()

	store := NewInMemorySessionTreeStore(1000, time.Hour)

	store.RecordNode(SessionTreeInfo{SessionID: "root"})
	store.RecordNode(SessionTreeInfo{SessionID: "branch-a", ParentSessionID: "root", AgentName: "agent-a"})
	store.RecordNode(SessionTreeInfo{SessionID: "branch-a-sub", ParentSessionID: "branch-a", AgentName: "agent-a-sub"})
	store.RecordNode(SessionTreeInfo{SessionID: "branch-b", ParentSessionID: "root", AgentName: "agent-b"})

	// 1. GetTree for root
	fullTree := store.GetTree("root")
	if len(fullTree) != 4 {
		t.Fatalf("GetTree(root) returned %d nodes, want 4", len(fullTree))
	}
	if fullTree[0].SessionID != "root" || fullTree[0].TreeDepth != 0 {
		t.Errorf("first node should be root (depth 0), got %+v", fullTree[0])
	}
	if fullTree[3].SessionID != "branch-a-sub" || fullTree[3].TreeDepth != 2 {
		t.Errorf("last node should be branch-a-sub (depth 2), got %+v", fullTree[3])
	}

	// 2. GetSubtree for branch-a
	subTree := store.GetSubtree("branch-a")
	if len(subTree) != 2 {
		t.Fatalf("GetSubtree(branch-a) returned %d nodes, want 2", len(subTree))
	}
	if subTree[0].SessionID != "branch-a" || subTree[1].SessionID != "branch-a-sub" {
		t.Errorf("subTree nodes mismatch: %+v", subTree)
	}

	// 3. Ancestors check
	ancestors := store.Ancestors("branch-a-sub")
	if len(ancestors) != 2 || ancestors[0] != "root" || ancestors[1] != "branch-a" {
		t.Fatalf("Ancestors(branch-a-sub) = %v, want [root, branch-a]", ancestors)
	}
	if got := store.Ancestors("root"); len(got) != 0 {
		t.Fatalf("Ancestors(root) = %v, want empty", got)
	}

	// 4. UpdateAffinity
	ok := store.UpdateAffinity("branch-a-sub", "auth-updated", "openai", "gpt-4o")
	if !ok {
		t.Fatalf("UpdateAffinity returned false")
	}
	node, _ := store.GetNode("branch-a-sub")
	if node.LastAuthID != "auth-updated" || node.LastProvider != "openai" {
		t.Errorf("node affinity not updated: %+v", node)
	}
}

func TestSessionTreeStoreExpiresNodesLazily(t *testing.T) {
	t.Parallel()

	store := NewInMemorySessionTreeStore(1000, time.Millisecond)
	store.RecordNode(SessionTreeInfo{SessionID: "expiring"})
	time.Sleep(5 * time.Millisecond)

	if node, ok := store.GetNode("expiring"); ok || node != nil {
		t.Fatalf("GetNode() returned expired node: %#v", node)
	}
	if got := store.Len(); got != 0 {
		t.Fatalf("Len() = %d after lazy expiry, want 0", got)
	}
}

func TestSessionTreeStoreLazyCleanupKeepsTouchedNodes(t *testing.T) {
	t.Parallel()

	store := NewInMemorySessionTreeStore(1000, 30*time.Millisecond)
	store.RecordNode(SessionTreeInfo{SessionID: "touched"})
	store.RecordNode(SessionTreeInfo{SessionID: "untouched"})
	time.Sleep(10 * time.Millisecond)
	if !store.UpdateAffinity("touched", "auth-1", "openai", "gpt-5") {
		t.Fatal("UpdateAffinity() failed for existing node")
	}
	time.Sleep(22 * time.Millisecond)

	if _, ok := store.GetNode("untouched"); ok {
		t.Fatal("GetNode() returned a node that should have expired")
	}
	if _, ok := store.GetNode("touched"); !ok {
		t.Fatal("GetNode() expired a node whose TTL was refreshed by UpdateAffinity")
	}
	if got := store.Len(); got != 1 {
		t.Fatalf("Len() = %d after mixed expiry, want 1", got)
	}
}

func TestSessionTreeStoreHierarchySupportsSlashIDs(t *testing.T) {
	t.Parallel()

	store := NewInMemorySessionTreeStore(1000, time.Hour)
	store.RecordNode(SessionTreeInfo{SessionID: "root/with/slash"})
	store.RecordNode(SessionTreeInfo{SessionID: "child/with/slash", ParentSessionID: "root/with/slash"})
	store.RecordNode(SessionTreeInfo{SessionID: "leaf", ParentSessionID: "child/with/slash"})

	ancestors := store.Ancestors("leaf")
	if len(ancestors) != 2 || ancestors[0] != "root/with/slash" || ancestors[1] != "child/with/slash" {
		t.Fatalf("Ancestors(leaf) = %v, want exact slash-bearing IDs", ancestors)
	}
	subtree := store.GetSubtree("child/with/slash")
	if len(subtree) != 2 || subtree[0].SessionID != "child/with/slash" || subtree[1].SessionID != "leaf" {
		t.Fatalf("GetSubtree() = %#v, want child and leaf", subtree)
	}
}

func TestExtractTreeInfoAllClients(t *testing.T) {
	t.Parallel()

	// 1. Claude Code with Subagent
	claudeHeaders := http.Header{
		"X-Claude-Code-Session-Id": []string{"claude-root-123"},
		"X-Claude-Code-Agent-Id":   []string{"subagent-checker"},
	}
	info, ok := ExtractTreeInfo(claudeHeaders, nil, nil)
	if !ok {
		t.Fatalf("ExtractTreeInfo failed for Claude Code")
	}
	if info.ClientType != "claude" {
		t.Errorf("ClientType = %q, want claude", info.ClientType)
	}
	if info.SessionID != "claude:claude-root-123:agent:subagent-checker" {
		t.Errorf("SessionID = %q", info.SessionID)
	}
	if info.ParentSessionID != "claude:claude-root-123" {
		t.Errorf("ParentSessionID = %q", info.ParentSessionID)
	}
	if info.AgentName != "subagent-checker" {
		t.Errorf("AgentName = %q", info.AgentName)
	}

	// 2. Codex CLI with parent thread
	codexHeaders := http.Header{
		"Session-Id":               []string{"codex-child-555"},
		"x-codex-parent-thread-id": []string{"codex-parent-111"},
	}
	info, ok = ExtractTreeInfo(codexHeaders, nil, nil)
	if !ok || info.ClientType != "codex" {
		t.Fatalf("ExtractTreeInfo failed for Codex CLI")
	}
	if info.SessionID != "codex:codex-child-555" || info.ParentSessionID != "codex:codex-parent-111" {
		t.Errorf("Codex session ids mismatch: %+v", info)
	}

	// 3. Pi Slot Session
	piHeaders := http.Header{
		"X-Slot-Session-Id": []string{"pi-slot-777"},
	}
	info, ok = ExtractTreeInfo(piHeaders, nil, nil)
	if !ok || info.ClientType != "pi" || info.SessionID != "slot:pi-slot-777" {
		t.Errorf("Pi slot mismatch: %+v", info)
	}

	// 4. OpenCode Session Affinity & Parent
	opencodeHeaders := http.Header{
		"X-Session-Affinity":        []string{"oc-child-999"},
		"X-Parent-Session-Affinity": []string{"oc-parent-333"},
	}
	info, ok = ExtractTreeInfo(opencodeHeaders, nil, nil)
	if !ok || info.ClientType != "opencode" || info.SessionID != "affinity:oc-child-999" || info.ParentSessionID != "affinity:oc-parent-333" {
		t.Errorf("OpenCode mismatch: %+v", info)
	}

	// 5. Antigravity X-Http-Session-Id
	agyHeaders := http.Header{
		"X-Http-Session-Id": []string{"agy-sess-888"},
	}
	info, ok = ExtractTreeInfo(agyHeaders, nil, nil)
	if !ok || info.ClientType != "agy" || info.SessionID != "agy:agy-sess-888" {
		t.Errorf("Antigravity mismatch: %+v", info)
	}

	// 6. Payload with metadata.agent_id and parent_session_id
	payload := []byte(`{
		"session_id": "payload-child-10",
		"parent_session_id": "payload-parent-01",
		"metadata": {
			"agent_id": "analyzer"
		}
	}`)
	metadata := map[string]any{
		cliproxyexecutor.CallerScopeMetadataKey: "test-scope",
	}
	info, ok = ExtractTreeInfo(nil, payload, metadata)
	if !ok || info.ClientType != "generic" {
		t.Fatalf("ExtractTreeInfo failed for payload")
	}
	if info.SessionID != "session:payload-child-10:agent:analyzer" {
		t.Errorf("SessionID = %q", info.SessionID)
	}
	if info.ParentSessionID != "session:payload-parent-01" {
		t.Errorf("ParentSessionID = %q", info.ParentSessionID)
	}
	if info.CallerScope != "test-scope" {
		t.Errorf("CallerScope = %q", info.CallerScope)
	}
}

func TestExtractTreeInfoCanonicalizesPayloadParentForHeaderSessions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		headers    http.Header
		wantParent string
	}{
		{
			name:       "generic header",
			headers:    http.Header{"X-Session-ID": []string{"child"}},
			wantParent: "header:parent",
		},
		{
			name:       "codex header",
			headers:    http.Header{"Session-Id": []string{"child"}},
			wantParent: "codex:parent",
		},
		{
			name:       "claude header",
			headers:    http.Header{"X-Claude-Code-Session-Id": []string{"child"}},
			wantParent: "claude:parent",
		},
	}

	payload := []byte(`{"parent_session_id":"parent"}`)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			info, ok := ExtractTreeInfo(test.headers, payload, nil)
			if !ok {
				t.Fatal("ExtractTreeInfo() returned no session")
			}
			if info.ParentSessionID != test.wantParent {
				t.Fatalf("ParentSessionID = %q, want %q", info.ParentSessionID, test.wantParent)
			}
		})
	}
}

func TestSessionTreeStoreConcurrencyAndLRUEviction(t *testing.T) {
	t.Parallel()

	// Max 50 nodes
	store := NewInMemorySessionTreeStore(50, time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rootID := fmt.Sprintf("root-%d", idx%10)
			childID := fmt.Sprintf("child-%d", idx)
			store.RecordNode(SessionTreeInfo{SessionID: rootID})
			store.RecordNode(SessionTreeInfo{SessionID: childID, ParentSessionID: rootID})
			_ = store.GetTree(rootID)
			_ = store.GetSubtree(childID)
		}(i)
	}
	wg.Wait()

	if store.Len() > 50 {
		t.Fatalf("store length %d exceeded maxNodes 50", store.Len())
	}
}

func TestSessionTreeNodeDeepCloneEmptyMetadata(t *testing.T) {
	t.Parallel()

	store := NewInMemorySessionTreeStore(10, time.Hour)
	meta := map[string]any{}
	store.RecordNode(SessionTreeInfo{SessionID: "session-meta", Metadata: meta})

	node1, ok1 := store.GetNode("session-meta")
	if !ok1 || node1 == nil {
		t.Fatal("GetNode() returned false")
	}
	node1.Metadata["mutated"] = true

	node2, ok2 := store.GetNode("session-meta")
	if !ok2 || node2 == nil {
		t.Fatal("GetNode() returned false on second read")
	}
	if val, exists := node2.Metadata["mutated"]; exists || val != nil {
		t.Fatalf("mutation of clone leaked into tree store: %#v", node2.Metadata)
	}
}

func TestSessionTreeStoreCycleRejection(t *testing.T) {
	t.Parallel()

	store := NewInMemorySessionTreeStore(10, time.Hour)

	// Record A -> B (A parent is B)
	store.RecordNode(SessionTreeInfo{SessionID: "node-a", ParentSessionID: "node-b"})
	// Record B -> A (B parent is A). This should be detected as a cycle and rejected, making B a root node.
	nodeB := store.RecordNode(SessionTreeInfo{SessionID: "node-b", ParentSessionID: "node-a"})
	if nodeB.ParentSessionID != "" {
		t.Fatalf("node-b parent = %q, want empty (cycle rejected)", nodeB.ParentSessionID)
	}
	if nodeB.RootSessionID != "node-b" {
		t.Fatalf("node-b root = %q, want node-b", nodeB.RootSessionID)
	}
	if nodeB.TreeDepth != 0 {
		t.Fatalf("node-b depth = %d, want 0", nodeB.TreeDepth)
	}

	ancestorsA := store.Ancestors("node-a")
	if len(ancestorsA) != 1 || ancestorsA[0] != "node-b" {
		t.Fatalf("Ancestors(node-a) = %v, want [node-b]", ancestorsA)
	}
	ancestorsB := store.Ancestors("node-b")
	if len(ancestorsB) != 0 {
		t.Fatalf("Ancestors(node-b) = %v, want empty", ancestorsB)
	}
}

func TestExtractTreeInfoRejectsControlCharacters(t *testing.T) {
	t.Parallel()

	payloadWithNewline := []byte(`{"session_id":"bad\nsession"}`)
	if _, ok := ExtractTreeInfo(nil, payloadWithNewline, nil); ok {
		t.Fatal("ExtractTreeInfo accepted session_id with newline")
	}

	payloadWithNull := []byte(`{"session_id":"bad\u0000session"}`)
	if _, ok := ExtractTreeInfo(nil, payloadWithNull, nil); ok {
		t.Fatal("ExtractTreeInfo accepted session_id with null byte")
	}
}

func TestSessionTreeStoreLateParentAndReparenting(t *testing.T) {
	t.Parallel()

	store := NewInMemorySessionTreeStore(100, time.Hour)

	// 1. Child arrives before parent
	child := store.RecordNode(SessionTreeInfo{
		SessionID:       "child-1",
		ParentSessionID: "parent-1",
	})
	if child.RootSessionID != "parent-1" || child.TreeDepth != 1 || child.TreePath != "parent-1/child-1" {
		t.Fatalf("unexpected initial child state: %+v", child)
	}

	// 2. Grandchild arrives before root and parent
	grandchild := store.RecordNode(SessionTreeInfo{
		SessionID:       "grandchild-1",
		ParentSessionID: "child-1",
	})
	if grandchild.RootSessionID != "parent-1" || grandchild.TreeDepth != 2 || grandchild.TreePath != "parent-1/child-1/grandchild-1" {
		t.Fatalf("unexpected initial grandchild state: %+v", grandchild)
	}

	// 3. Parent arrives, rooted at root-1 (which is already known or self-rooted)
	parent := store.RecordNode(SessionTreeInfo{
		SessionID:       "parent-1",
		ParentSessionID: "root-1",
	})
	if parent.RootSessionID != "root-1" || parent.TreeDepth != 1 || parent.TreePath != "root-1/parent-1" {
		t.Fatalf("unexpected parent state: %+v", parent)
	}

	// Verify child and grandchild were cascaded to root-1
	updatedChild, ok := store.GetNode("child-1")
	if !ok || updatedChild.RootSessionID != "root-1" || updatedChild.TreeDepth != 2 || updatedChild.TreePath != "root-1/parent-1/child-1" {
		t.Fatalf("child not cascaded to root-1: %+v", updatedChild)
	}

	updatedGrandchild, ok := store.GetNode("grandchild-1")
	if !ok || updatedGrandchild.RootSessionID != "root-1" || updatedGrandchild.TreeDepth != 3 || updatedGrandchild.TreePath != "root-1/parent-1/child-1/grandchild-1" {
		t.Fatalf("grandchild not cascaded to root-1: %+v", updatedGrandchild)
	}

	// Check GetTree on root-1
	tree := store.GetTree("root-1")
	if len(tree) != 3 {
		t.Fatalf("GetTree(root-1) returned %d nodes, want 3", len(tree))
	}

	// 4. Reparent child-1 to another parent (parent-2 under root-2)
	_ = store.RecordNode(SessionTreeInfo{
		SessionID: "root-2",
	})
	_ = store.RecordNode(SessionTreeInfo{
		SessionID:       "parent-2",
		ParentSessionID: "root-2",
	})
	_ = store.RecordNode(SessionTreeInfo{
		SessionID:       "child-1",
		ParentSessionID: "parent-2",
	})

	reparentedChild, _ := store.GetNode("child-1")
	if reparentedChild.RootSessionID != "root-2" || reparentedChild.TreeDepth != 2 || reparentedChild.TreePath != "root-2/parent-2/child-1" {
		t.Fatalf("reparented child state incorrect: %+v", reparentedChild)
	}

	reparentedGrandchild, _ := store.GetNode("grandchild-1")
	if reparentedGrandchild.RootSessionID != "root-2" || reparentedGrandchild.TreeDepth != 3 || reparentedGrandchild.TreePath != "root-2/parent-2/child-1/grandchild-1" {
		t.Fatalf("reparented grandchild state incorrect: %+v", reparentedGrandchild)
	}

	tree1 := store.GetTree("root-1")
	if len(tree1) != 1 { // only parent-1 left
		t.Fatalf("GetTree(root-1) returned %d nodes, want 1", len(tree1))
	}
	tree2 := store.GetTree("root-2")
	if len(tree2) != 4 { // root-2, parent-2, child-1, grandchild-1
		t.Fatalf("GetTree(root-2) returned %d nodes, want 4", len(tree2))
	}
}

func TestSessionTreeNodeCloneDeepCopiesTypedMapsAndSlices(t *testing.T) {
	original := &SessionTreeNode{
		SessionID: "test-session",
		Metadata: map[string]any{
			"stringMap": map[string]string{
				"key1": "val1",
			},
			"stringSlice": []string{"a", "b"},
			"intSlice":    []int{1, 2, 3},
			"byteSlice":   []byte("hello"),
		},
	}

	clone := original.Clone()
	if clone == nil {
		t.Fatal("clone returned nil")
	}

	// Mutate clone
	if sm, ok := clone.Metadata["stringMap"].(map[string]string); ok {
		sm["key1"] = "mutated"
		sm["key2"] = "val2"
	} else {
		t.Fatal("expected stringMap in clone metadata")
	}

	if origSM, ok := original.Metadata["stringMap"].(map[string]string); ok {
		if origSM["key1"] != "val1" || len(origSM) != 1 {
			t.Fatalf("original stringMap was mutated: %v", origSM)
		}
	}
}

func TestExtractTreeInfoClaudeNestedParent(t *testing.T) {
	// Nested metadata.user_id containing session_id and parent_session_id
	payload := []byte(`{
		"metadata": {
			"user_id": "{\"session_id\":\"child-session-123\",\"parent_session_id\":\"parent-session-456\",\"agent_id\":\"subagent-worker\"}"
		}
	}`)
	info, ok := ExtractTreeInfo(nil, payload, nil)
	if !ok {
		t.Fatal("ExtractTreeInfo failed on nested Claude user_id")
	}
	if info.SessionID != "claude:child-session-123:agent:subagent-worker" {
		t.Fatalf("expected child session with agent, got %q", info.SessionID)
	}
	if info.ParentSessionID != "claude:parent-session-456" {
		t.Fatalf("expected parent claude:parent-session-456, got %q", info.ParentSessionID)
	}

	// Explicit header + payload body parent_session_id
	headerPayload := []byte(`{"parent_session_id":"parent-session-789"}`)
	headers := http.Header{}
	headers.Set("X-Claude-Code-Session-Id", "header-child-123")
	info2, ok := ExtractTreeInfo(headers, headerPayload, nil)
	if !ok {
		t.Fatal("ExtractTreeInfo failed on header + body parent")
	}
	if info2.SessionID != "claude:header-child-123" {
		t.Fatalf("expected session claude:header-child-123, got %q", info2.SessionID)
	}
	if info2.ParentSessionID != "claude:parent-session-789" {
		t.Fatalf("expected parent claude:parent-session-789, got %q", info2.ParentSessionID)
	}
}

func TestExtractTreeInfoGeminiAndAntigravityHierarchy(t *testing.T) {
	// Gemini cachedContent with parent_session_id
	geminiPayload := []byte(`{"cachedContent":"cache-child-1","parent_session_id":"cache-parent-1"}`)
	info, ok := ExtractTreeInfo(nil, geminiPayload, nil)
	if !ok {
		t.Fatal("ExtractTreeInfo failed on Gemini cachedContent")
	}
	if info.SessionID != "geminicache:cache-child-1" {
		t.Fatalf("expected session geminicache:cache-child-1, got %q", info.SessionID)
	}
	if info.ParentSessionID != "geminicache:cache-parent-1" {
		t.Fatalf("expected parent geminicache:cache-parent-1, got %q", info.ParentSessionID)
	}
	if info.AgentName != "subagent" {
		t.Fatalf("expected subagent, got %q", info.AgentName)
	}

	// Antigravity headers with parent
	headers := http.Header{}
	headers.Set("X-Http-Session-Id", "agy-child-2")
	headers.Set("X-Parent-Session-ID", "agy-parent-2")
	info2, ok := ExtractTreeInfo(headers, nil, nil)
	if !ok {
		t.Fatal("ExtractTreeInfo failed on Antigravity headers")
	}
	if info2.SessionID != "agy:agy-child-2" {
		t.Fatalf("expected session agy:agy-child-2, got %q", info2.SessionID)
	}
	if info2.ParentSessionID != "agy:agy-parent-2" {
		t.Fatalf("expected parent agy:agy-parent-2, got %q", info2.ParentSessionID)
	}
	if info2.AgentName != "subagent" {
		t.Fatalf("expected subagent, got %q", info2.AgentName)
	}
}

func TestSessionTreeStoreMaxDepthCapping(t *testing.T) {
	t.Parallel()

	store := NewInMemorySessionTreeStore(500, time.Hour)
	current := "node-0"
	store.RecordNode(SessionTreeInfo{SessionID: current})

	// Create a chain exceeding maxTreeDepth (128)
	for i := 1; i <= 150; i++ {
		next := fmt.Sprintf("node-%d", i)
		node := store.RecordNode(SessionTreeInfo{
			SessionID:       next,
			ParentSessionID: current,
		})
		if i <= 128 {
			if node.TreeDepth != i {
				t.Fatalf("expected depth %d, got %d", i, node.TreeDepth)
			}
		} else {
			// Once exceeding maxTreeDepth, the chain is capped and the node becomes its own root
			if node.TreeDepth > maxTreeDepth {
				t.Fatalf("depth %d exceeded maxTreeDepth %d", node.TreeDepth, maxTreeDepth)
			}
		}
		current = next
	}

	// Verify that the capped node's parent index was cleaned up
	cappedParent := fmt.Sprintf("node-%d", 128)
	cappedNode := fmt.Sprintf("node-%d", 129)
	store.mu.Lock()
	if children, ok := store.parentIndex[cappedParent]; ok {
		if _, exists := children[cappedNode]; exists {
			store.mu.Unlock()
			t.Fatalf("capped node %s still found in parentIndex[%s]", cappedNode, cappedParent)
		}
	}
	store.mu.Unlock()
}

func TestSessionTreeStoreMetadataDeepRecursionProtection(t *testing.T) {
	t.Parallel()

	store := NewInMemorySessionTreeStore(100, time.Hour)

	// Create deeply nested map
	nested := map[string]any{"level": 0}
	curr := nested
	for i := 1; i <= 30; i++ {
		next := map[string]any{"level": i}
		curr["child"] = next
		curr = next
	}

	// Recording node should succeed without stack overflow
	node := store.RecordNode(SessionTreeInfo{
		SessionID: "sess-deep-meta",
		Metadata:  nested,
	})
	if node.SessionID != "sess-deep-meta" {
		t.Fatalf("expected sess-deep-meta, got %s", node.SessionID)
	}
}

func TestExtractTreeInfoNestedAntigravityRequest(t *testing.T) {
	t.Parallel()

	payload := []byte(`{
		"project_id": "proj-123",
		"request": {
			"parentSessionId": "parent-sess-456",
			"sessionId": "child-sess-789"
		}
	}`)
	info, ok := ExtractTreeInfo(nil, payload, nil)
	if !ok {
		t.Fatal("ExtractTreeInfo failed on nested Antigravity payload")
	}
	if info.SessionID != "session:child-sess-789" {
		t.Fatalf("SessionID = %q, want session:child-sess-789", info.SessionID)
	}
	if info.ParentSessionID != "session:parent-sess-456" {
		t.Fatalf("ParentSessionID = %q, want session:parent-sess-456", info.ParentSessionID)
	}
}

func BenchmarkSessionTreeRecordAndCascade(b *testing.B) {
	store := NewInMemorySessionTreeStore(10000, time.Hour)
	// Pre-populate root and parent
	store.RecordNode(SessionTreeInfo{SessionID: "root-node"})
	for i := 0; i < 50; i++ {
		store.RecordNode(SessionTreeInfo{
			SessionID:       fmt.Sprintf("parent-%d", i),
			ParentSessionID: "root-node",
		})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		store.RecordNode(SessionTreeInfo{
			SessionID:       fmt.Sprintf("child-%d", i%1000),
			ParentSessionID: fmt.Sprintf("parent-%d", i%50),
		})
	}
}

func BenchmarkSessionTreeGetSubtree(b *testing.B) {
	store := NewInMemorySessionTreeStore(10000, time.Hour)
	store.RecordNode(SessionTreeInfo{SessionID: "root-node"})
	for i := 0; i < 20; i++ {
		p := fmt.Sprintf("parent-%d", i)
		store.RecordNode(SessionTreeInfo{SessionID: p, ParentSessionID: "root-node"})
		for j := 0; j < 10; j++ {
			c := fmt.Sprintf("child-%d-%d", i, j)
			store.RecordNode(SessionTreeInfo{SessionID: c, ParentSessionID: p})
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = store.GetSubtree("root-node")
	}
}
