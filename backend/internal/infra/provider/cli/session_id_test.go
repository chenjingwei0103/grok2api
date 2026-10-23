package cli

import "testing"

func TestGrokSessionIDEmptyWhenNoKey(t *testing.T) {
	got, err := grokSessionID("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("empty key must not invent session id, got %q", got)
	}
	// 两次调用仍为空，证明无无随机漂移
	again, err := grokSessionID("   ")
	if err != nil || again != "" {
		t.Fatalf("blank key = %q err=%v", again, err)
	}
}

func TestGrokSessionIDStableForSameKey(t *testing.T) {
	first, err := grokSessionID("client-session")
	if err != nil || first == "" {
		t.Fatalf("first = %q err=%v", first, err)
	}
	second, err := grokSessionID("client-session")
	if err != nil || second != first {
		t.Fatalf("unstable: first=%q second=%q err=%v", first, second, err)
	}
}

func TestGrokConversationGroupIDMatchesGrokBuildDerivation(t *testing.T) {
	// Reference vector: uuid5(NAMESPACE_OID, "xai:grok-build:conversation-group:" + root_session_id),
	// identical to xai-grok-shell `derive_conversation_group_id`.
	const root = "0f4c7a1e-6c1b-5a0e-8e6c-3b0f6a6a2a1d"
	got := grokConversationGroupID(root)
	if got != "d5de345a-3509-59bc-b73c-7d33029d840b" {
		t.Fatalf("conversation group id = %q", got)
	}
	if again := grokConversationGroupID(root); again != got {
		t.Fatalf("unstable: first=%q second=%q", got, again)
	}
	if other := grokConversationGroupID("another-root"); other == got {
		t.Fatalf("different roots must not share a group id: %q", other)
	}
}
