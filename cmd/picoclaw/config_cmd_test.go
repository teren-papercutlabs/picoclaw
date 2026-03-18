package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func executePicoclawCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()

	cmd := NewPicoclawCommand()
	var out bytes.Buffer
	var errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)

	err := cmd.Execute()
	output := strings.TrimSpace(out.String())
	if output == "" {
		output = strings.TrimSpace(errBuf.String())
	}
	return output, err
}

func TestParseConfigPath_QuotedKeysAndIndexes(t *testing.T) {
	tokens, err := parseConfigPath(`agents.list[0].model.primary`)
	if err != nil {
		t.Fatalf("parseConfigPath() error = %v", err)
	}
	if len(tokens) != 5 {
		t.Fatalf("tokens = %d, want 5", len(tokens))
	}
	if tokens[0].key == nil || *tokens[0].key != "agents" {
		t.Fatalf("token[0] = %#v, want agents", tokens[0])
	}
	if tokens[2].index == nil || *tokens[2].index != 0 {
		t.Fatalf("token[2] = %#v, want index 0", tokens[2])
	}

	tokens, err = parseConfigPath(`cost_tracking.prices["gpt-5.4.nano"]`)
	if err != nil {
		t.Fatalf("parseConfigPath() quoted error = %v", err)
	}
	if len(tokens) != 3 {
		t.Fatalf("quoted tokens = %d, want 3", len(tokens))
	}
	if tokens[2].key == nil || *tokens[2].key != "gpt-5.4.nano" {
		t.Fatalf("token[2] = %#v, want quoted key", tokens[2])
	}
}

func TestResolveConfigCommandPath_FallsBackToLocalConfig(t *testing.T) {
	tmpDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	defer os.Chdir(oldWD)

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("Chdir() error = %v", err)
	}

	if err := os.WriteFile("config.json", []byte(`{"heartbeat":{"enabled":true}}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	t.Setenv("PICOCLAW_CONFIG", "")
	t.Setenv("PICOCLAW_HOME", filepath.Join(tmpDir, "missing-home"))

	got, err := resolveConfigCommandPath()
	if err != nil {
		t.Fatalf("resolveConfigCommandPath() error = %v", err)
	}
	if got != filepath.Join(".", "config.json") {
		t.Fatalf("path = %q, want ./config.json", got)
	}
}

func TestConfigCommand_SetGetListPreservesFields(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	raw := `{
  "agents": {
    "defaults": {
      "model": "gpt-5.4"
    },
    "list": [
      {
        "id": "main",
        "model": {
          "primary": "gpt-5.4"
        }
      }
    ]
  },
  "heartbeat": {
    "enabled": true,
    "interval": 15
  },
  "placeholder": {
    "enabled": true
  },
  "channels": {
    "onebot": {
      "group_trigger": {
        "prefixes": ["!"],
        "mention_only": true
      }
    }
  },
  "cost_tracking": {
    "prices": {
      "existing-model": {
        "input": 0.1,
        "output": 0.2
      }
    }
  }
}`
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	t.Setenv("PICOCLAW_CONFIG", configPath)

	beforeList, err := executePicoclawCommand(t, "config", "list")
	if err != nil {
		t.Fatalf("config list before error = %v", err)
	}

	setOutput, err := executePicoclawCommand(t, "config", "set", "heartbeat.enabled", "false")
	if err != nil {
		t.Fatalf("config set heartbeat.enabled error = %v", err)
	}
	if !strings.Contains(setOutput, "false") {
		t.Fatalf("set output = %q, want updated value", setOutput)
	}
	if !strings.Contains(setOutput, restartNote) {
		t.Fatalf("set output = %q, want restart note", setOutput)
	}

	got, err := executePicoclawCommand(t, "config", "get", "heartbeat.enabled")
	if err != nil {
		t.Fatalf("config get heartbeat.enabled error = %v", err)
	}
	if got != "false" {
		t.Fatalf("get heartbeat.enabled = %q, want false", got)
	}

	_, err = executePicoclawCommand(
		t,
		"config",
		"set",
		`cost_tracking.prices["gpt-5.4-nano"]`,
		`{"input": 0.20, "output": 1.25}`,
	)
	if err != nil {
		t.Fatalf("config set cost_tracking.prices error = %v", err)
	}

	got, err = executePicoclawCommand(t, "config", "get", `cost_tracking.prices["gpt-5.4-nano"]`)
	if err != nil {
		t.Fatalf("config get cost_tracking.prices error = %v", err)
	}
	if got != `{"input":0.2,"output":1.25}` {
		t.Fatalf("get nested json = %q, want compact JSON", got)
	}

	afterList, err := executePicoclawCommand(t, "config", "list")
	if err != nil {
		t.Fatalf("config list after error = %v", err)
	}
	if beforeList != afterList {
		t.Fatalf("top-level keys changed:\nbefore:\n%s\n\nafter:\n%s", beforeList, afterList)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if _, ok := doc["placeholder"]; !ok {
		t.Fatal("placeholder key was dropped")
	}
	channels, ok := doc["channels"].(map[string]any)
	if !ok {
		t.Fatal("channels missing after set")
	}
	onebot, ok := channels["onebot"].(map[string]any)
	if !ok {
		t.Fatal("channels.onebot missing after set")
	}
	if _, ok := onebot["group_trigger"]; !ok {
		t.Fatal("channels.onebot.group_trigger was dropped")
	}

	listedKeys := strings.Split(afterList, "\n")
	expectedKeys := []string{"agents", "channels", "cost_tracking", "heartbeat", "placeholder"}
	for _, key := range expectedKeys {
		if !slices.Contains(listedKeys, key) {
			t.Fatalf("config list missing key %q in %v", key, listedKeys)
		}
	}
}
