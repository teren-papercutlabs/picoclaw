// PicoClaw PCL downstream — cost tracking tests.
// Downstream: permanent

package telemetry

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCostTrackerRecord(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "subdir", "cost.jsonl")

	prices := map[string]ModelPrice{
		"gemini-2.5-flash": {Input: 0.15, Output: 0.60},
	}

	ct := NewCostTracker(logPath, prices)

	entry := CostEntry{
		AgentID:          "test-agent",
		Model:            "gemini-2.5-flash",
		SessionKey:       "session:abc",
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
		ToolResults: []ToolResult{
			{Name: "web_search", Success: true},
		},
		Iterations: 2,
		DurationMs: 1234,
	}

	if err := ct.Record(entry); err != nil {
		t.Fatalf("Record failed: %v", err)
	}

	// Read back the JSONL line.
	f, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		t.Fatal("expected one JSONL line, got none")
	}

	var got CostEntry
	if err := json.Unmarshal(scanner.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Verify cost: (1000 * 0.15 / 1_000_000) + (500 * 0.60 / 1_000_000) = 0.00015 + 0.0003 = 0.00045
	want := 0.00045
	if got.CostUSD != want {
		t.Errorf("CostUSD = %v, want %v", got.CostUSD, want)
	}
	if got.AgentID != "test-agent" {
		t.Errorf("AgentID = %q, want %q", got.AgentID, "test-agent")
	}
	if got.Ts == "" {
		t.Error("Ts should be set")
	}
	if len(got.ToolResults) != 1 || got.ToolResults[0].Name != "web_search" {
		t.Errorf("ToolResults = %v, unexpected", got.ToolResults)
	}

	// Verify no second line.
	if scanner.Scan() {
		t.Error("expected only one JSONL line")
	}
}

func TestCostTrackerUnknownModel(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "cost.jsonl")

	ct := NewCostTracker(logPath, map[string]ModelPrice{})

	entry := CostEntry{
		AgentID:          "agent",
		Model:            "unknown-model",
		PromptTokens:     100,
		CompletionTokens: 50,
	}

	if err := ct.Record(entry); err != nil {
		t.Fatalf("Record failed: %v", err)
	}

	f, _ := os.Open(logPath)
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Scan()

	var got CostEntry
	json.Unmarshal(scanner.Bytes(), &got)
	if got.CostUSD != 0 {
		t.Errorf("CostUSD for unknown model = %v, want 0", got.CostUSD)
	}
}

func TestComputeCost(t *testing.T) {
	ct := NewCostTracker("/dev/null", map[string]ModelPrice{
		"m": {Input: 1.25, Output: 10.00},
	})
	// 2000 prompt @ $1.25/M + 500 completion @ $10.00/M = 0.0025 + 0.005 = 0.0075
	got := ct.computeCost("m", 2000, 500)
	want := 0.0075
	if got != want {
		t.Errorf("computeCost = %v, want %v", got, want)
	}
}
