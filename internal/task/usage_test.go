package task

import (
	"encoding/json"
	"testing"
)

func TestTokenUsageAvailabilityCountersRoundTripAndAdd(t *testing.T) {
	var empty TokenUsage
	if !empty.Empty() {
		t.Fatal("zero usage should be empty")
	}

	first := TokenUsage{Requests: 2, PromptBytes: 11, UnreportedRequests: 1, InputTokens: 5}
	second := TokenUsage{Requests: 3, PromptBytes: 13, IncompleteRequests: 2, OutputTokens: 7}
	first.Add(second)
	want := TokenUsage{Requests: 5, PromptBytes: 24, UnreportedRequests: 1, IncompleteRequests: 2, InputTokens: 5, OutputTokens: 7}
	if first != want {
		t.Fatalf("usage=%#v, want %#v", first, want)
	}
	if first.Empty() {
		t.Fatal("availability counters should make usage non-empty")
	}

	raw, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	var got TokenUsage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got != first {
		t.Fatalf("round trip=%#v, want %#v", got, first)
	}
}

func TestTokenUsageBackwardCompatibleDecoding(t *testing.T) {
	var got TokenUsage
	if err := json.Unmarshal([]byte(`{"requests":4,"prompt_bytes":9,"output_tokens":3}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Requests != 4 || got.PromptBytes != 9 || got.OutputTokens != 3 || got.UnreportedRequests != 0 || got.IncompleteRequests != 0 {
		t.Fatalf("legacy usage=%#v", got)
	}
}
