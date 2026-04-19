package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	cliinternal "github.com/sipeed/picoclaw/cmd/picoclaw/internal"
	"github.com/sipeed/picoclaw/pkg/fileutil"
)

type configPathToken struct {
	key   *string
	index *int
}

const restartNote = "Restart picoclaw for model/provider changes to take effect."

func NewConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and update config.json atomically",
		Args:  cobra.NoArgs,
	}

	cmd.AddCommand(
		newConfigGetCommand(),
		newConfigSetCommand(),
		newConfigListCommand(),
	)

	return cmd
}

func newConfigGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "get <path>",
		Short: "Get a config value by dot-path",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			configPath, doc, err := loadConfigDocument()
			if err != nil {
				return err
			}

			value, err := getConfigValue(doc, args[0])
			if err != nil {
				return fmt.Errorf("%s: %w", configPath, err)
			}

			return writeConfigValue(cmd.OutOrStdout(), value)
		},
	}
}

func newConfigSetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "set <path> <value>",
		Short: "Set a config value by dot-path",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			configPath, doc, err := loadConfigDocument()
			if err != nil {
				return err
			}

			value, err := parseConfigCLIValue(args[1])
			if err != nil {
				return err
			}

			updated, err := setConfigValue(doc, args[0], value)
			if err != nil {
				return fmt.Errorf("%s: %w", configPath, err)
			}

			data, err := json.MarshalIndent(updated, "", "  ")
			if err != nil {
				return fmt.Errorf("marshal config: %w", err)
			}
			data = append(data, '\n')

			if err := fileutil.WriteFileAtomic(configPath, data, 0o600); err != nil {
				return fmt.Errorf("write config: %w", err)
			}

			if err := writeConfigValue(cmd.OutOrStdout(), value); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), restartNote)
			return nil
		},
	}
}

func newConfigListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List top-level config keys",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			configPath, doc, err := loadConfigDocument()
			if err != nil {
				return err
			}

			root, ok := doc.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: root config is not an object", configPath)
			}

			keys := make([]string, 0, len(root))
			for key := range root {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), key)
			}
			return nil
		},
	}
}

func loadConfigDocument() (string, any, error) {
	configPath, err := resolveConfigCommandPath()
	if err != nil {
		return "", nil, err
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", nil, fmt.Errorf("read config: %w", err)
	}

	var doc any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&doc); err != nil {
		return "", nil, fmt.Errorf("decode config: %w", err)
	}
	return configPath, doc, nil
}

func resolveConfigCommandPath() (string, error) {
	configPath := cliinternal.GetConfigPath()
	if os.Getenv("PICOCLAW_CONFIG") != "" {
		return configPath, nil
	}

	if _, err := os.Stat(configPath); err == nil {
		return configPath, nil
	}

	localPath := filepath.Join(".", "config.json")
	if _, err := os.Stat(localPath); err == nil {
		return localPath, nil
	}

	return configPath, nil
}

func writeConfigValue(w io.Writer, value any) error {
	switch v := value.(type) {
	case string:
		_, err := fmt.Fprintln(w, v)
		return err
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("marshal value: %w", err)
		}
		_, err = fmt.Fprintln(w, string(data))
		return err
	}
}

func parseConfigCLIValue(raw string) (any, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}

	if json.Valid([]byte(trimmed)) {
		var jsonValue any
		if err := json.Unmarshal([]byte(trimmed), &jsonValue); err == nil {
			return jsonValue, nil
		}
	}

	if trimmed == "true" {
		return true, nil
	}
	if trimmed == "false" {
		return false, nil
	}
	if i, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		return json.Number(strconv.FormatInt(i, 10)), nil
	}
	if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
		return json.Number(strconv.FormatFloat(f, 'f', -1, 64)), nil
	}
	return raw, nil
}

func getConfigValue(root any, path string) (any, error) {
	tokens, err := parseConfigPath(path)
	if err != nil {
		return nil, err
	}

	current := root
	for _, token := range tokens {
		switch {
		case token.key != nil:
			obj, ok := current.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("path %q does not point to an object", path)
			}
			next, ok := obj[*token.key]
			if !ok {
				return nil, fmt.Errorf("path %q not found", path)
			}
			current = next
		case token.index != nil:
			arr, ok := current.([]any)
			if !ok {
				return nil, fmt.Errorf("path %q does not point to an array", path)
			}
			if *token.index < 0 || *token.index >= len(arr) {
				return nil, fmt.Errorf("path %q index %d out of range", path, *token.index)
			}
			current = arr[*token.index]
		}
	}

	return current, nil
}

func setConfigValue(root any, path string, value any) (any, error) {
	tokens, err := parseConfigPath(path)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("path is empty")
	}

	return setConfigNode(root, tokens, value)
}

func setConfigNode(current any, tokens []configPathToken, value any) (any, error) {
	if len(tokens) == 0 {
		return value, nil
	}

	token := tokens[0]
	if token.key != nil {
		var obj map[string]any
		switch typed := current.(type) {
		case nil:
			obj = make(map[string]any)
		case map[string]any:
			obj = typed
		default:
			return nil, fmt.Errorf("expected object at %q", *token.key)
		}

		child, _ := obj[*token.key]
		updated, err := setConfigNode(child, tokens[1:], value)
		if err != nil {
			return nil, err
		}
		obj[*token.key] = updated
		return obj, nil
	}

	var arr []any
	switch typed := current.(type) {
	case nil:
		arr = make([]any, 0, *token.index+1)
	case []any:
		arr = typed
	default:
		return nil, fmt.Errorf("expected array at index %d", *token.index)
	}

	for len(arr) <= *token.index {
		arr = append(arr, nil)
	}

	updated, err := setConfigNode(arr[*token.index], tokens[1:], value)
	if err != nil {
		return nil, err
	}
	arr[*token.index] = updated
	return arr, nil
}

func parseConfigPath(path string) ([]configPathToken, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("path is empty")
	}

	var tokens []configPathToken
	for i := 0; i < len(path); {
		switch path[i] {
		case '.':
			i++
		case '[':
			token, next, err := parseBracketToken(path, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, token)
			i = next
		default:
			token, next, err := parseObjectToken(path, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, token)
			i = next
		}
	}

	return tokens, nil
}

func parseObjectToken(path string, start int) (configPathToken, int, error) {
	var builder strings.Builder
	i := start
	for i < len(path) {
		switch path[i] {
		case '\\':
			i++
			if i >= len(path) {
				return configPathToken{}, 0, fmt.Errorf("unterminated escape in %q", path)
			}
			builder.WriteByte(path[i])
			i++
		case '.', '[':
			key := builder.String()
			if key == "" {
				return configPathToken{}, 0, fmt.Errorf("empty path segment in %q", path)
			}
			return configPathToken{key: &key}, i, nil
		default:
			builder.WriteByte(path[i])
			i++
		}
	}

	key := builder.String()
	if key == "" {
		return configPathToken{}, 0, fmt.Errorf("empty path segment in %q", path)
	}
	return configPathToken{key: &key}, i, nil
}

func parseBracketToken(path string, start int) (configPathToken, int, error) {
	i := start + 1
	if i >= len(path) {
		return configPathToken{}, 0, fmt.Errorf("unterminated bracket in %q", path)
	}

	if path[i] == '"' {
		i++
		var builder strings.Builder
		for i < len(path) {
			switch path[i] {
			case '\\':
				i++
				if i >= len(path) {
					return configPathToken{}, 0, fmt.Errorf("unterminated string in %q", path)
				}
				builder.WriteByte(path[i])
				i++
			case '"':
				i++
				if i >= len(path) || path[i] != ']' {
					return configPathToken{}, 0, fmt.Errorf("missing closing ] in %q", path)
				}
				i++
				key := builder.String()
				return configPathToken{key: &key}, i, nil
			default:
				builder.WriteByte(path[i])
				i++
			}
		}
		return configPathToken{}, 0, fmt.Errorf("unterminated string in %q", path)
	}

	end := strings.IndexByte(path[i:], ']')
	if end == -1 {
		return configPathToken{}, 0, fmt.Errorf("missing closing ] in %q", path)
	}
	content := strings.TrimSpace(path[i : i+end])
	index, err := strconv.Atoi(content)
	if err != nil {
		return configPathToken{}, 0, fmt.Errorf("invalid array index %q in %q", content, path)
	}
	i += end + 1
	return configPathToken{index: &index}, i, nil
}
