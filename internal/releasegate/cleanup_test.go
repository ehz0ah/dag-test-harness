package releasegate

import (
	"context"
	"errors"
	"testing"

	"code.byted.org/data-arch/ovtest/ops"
)

func TestOwnershipRegistryAcceptsOnlyTypedExactClaims(t *testing.T) {
	r := NewOwnershipRegistry("run-abc")
	for _, claim := range []ops.CleanupClaim{
		{URI: "viking://user/runner/memories/mem-123", Kind: "memory", Source: "find", Proof: "verified relevant result"},
		{URI: "viking://user/runner/peers/hermes/memories/mem-789", Kind: "memory", Source: "find", Proof: "verified peer result"},
		{URI: "viking://agent/helper/memories/mem-456", Kind: "memory", Source: "find", Proof: "verified relevant result"},
		{URI: "viking://resources/ovtest-runs/run-abc/file.md", Kind: "resource", Source: "find", Proof: "verified resource result"},
		{URI: "viking://user/runner/sessions/cc-session-123", Kind: "session", Source: "session_get", Proof: "server confirmed session"},
	} {
		if err := r.Claim(claim); err != nil {
			t.Fatalf("Claim(%+v): %v", claim, err)
		}
	}
	for _, claim := range []ops.CleanupClaim{
		{URI: "viking://user/runner/memories", Kind: "memory", Source: "find", Proof: "too broad"},
		{URI: "viking://user/memories/legacy", Kind: "memory", Source: "find", Proof: "missing user scope"},
		{URI: "viking://resources/shared", Kind: "resource", Source: "find", Proof: "too broad"},
		{URI: "viking://session/cc/a", Kind: "session", Source: "derived", Proof: "not exact"},
		{URI: "viking://user/memories/forged", Kind: "memory", Proof: "missing source"},
		{URI: "viking://user/memories/forged", Kind: "unknown", Source: "x", Proof: "x"},
	} {
		if err := r.Claim(claim); err == nil {
			t.Fatalf("unsafe claim accepted: %+v", claim)
		}
	}
}

func TestOwnershipRegistryAcceptsExactOwnedURIsOnly(t *testing.T) {
	r := NewOwnershipRegistry("run-abc")
	for _, uri := range []string{
		"viking://user/alice/memories/run-abc/item",
		"viking://resources/ovtest/run-abc/file",
	} {
		if err := r.Own(uri); err != nil {
			t.Fatalf("Own(%q): %v", uri, err)
		}
	}
	for _, uri := range []string{"viking://", "viking://user/alice", "viking://resources", "viking://resources/other/file", "http://example.com"} {
		if err := r.Own(uri); err == nil {
			t.Fatalf("unsafe URI %q accepted", uri)
		}
	}
}

func TestOwnershipRegistryAcceptsGeneratedMemoryLeaf(t *testing.T) {
	r := NewOwnershipRegistry("run-abc")
	if err := r.OwnGenerated("viking://user/runner/memories/generated-7f4a"); err != nil {
		t.Fatal(err)
	}
	if err := r.OwnGenerated("viking://agent/helper/memories/generated-8f5b"); err != nil {
		t.Fatal(err)
	}
	if err := r.OwnGenerated("viking://user/runner/peers/hermes/memories/generated-9a6c"); err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{"viking://user/runner/memories", "viking://user/alice", "viking://user/memories/id", "viking://resources"} {
		if err := r.OwnGenerated(uri); err == nil {
			t.Fatalf("broad generated URI %q accepted", uri)
		}
	}
}

func TestOwnershipRegistryAcceptsExactGeneratedSessionOnly(t *testing.T) {
	r := NewOwnershipRegistry("run-abc")
	if err := r.OwnGenerated("viking://session/cc-session-123"); err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{
		"viking://session",
		"viking://session/cc-session-123/messages.jsonl",
		"viking://session/cc-session-123/child",
		"viking://session/cc-session%2Fchild",
	} {
		if err := r.OwnGenerated(uri); err == nil {
			t.Fatalf("unsafe generated session URI %q accepted", uri)
		}
	}
	if err := r.Own("viking://session/run-abc"); err == nil {
		t.Fatal("unverified session URI accepted through Own")
	}
}

func TestCleanupPreservesPrimaryAndCleanupFailures(t *testing.T) {
	r := NewOwnershipRegistry("run-abc")
	_ = r.Own("viking://resources/ovtest/run-abc/file")
	primary := errors.New("case failed")
	outcome := r.Cleanup(context.Background(), primary, func(context.Context, string) error {
		return errors.New("delete failed")
	})
	if !errors.Is(outcome.Primary, primary) || outcome.Cleanup == nil {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestCleanupAttemptsAllOwnedURIs(t *testing.T) {
	r := NewOwnershipRegistry("run-abc")
	_ = r.Own("viking://resources/ovtest/run-abc/a")
	_ = r.Own("viking://resources/ovtest/run-abc/b")
	var got []string
	outcome := r.Cleanup(context.Background(), nil, func(_ context.Context, uri string) error {
		got = append(got, uri)
		return errors.New("boom")
	})
	if len(got) != 2 || outcome.Cleanup == nil {
		t.Fatalf("got=%v outcome=%+v", got, outcome)
	}
}
