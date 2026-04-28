package httpapi

import (
	"encoding/json"
	"strings"
)

func normalizeStoredToolCall(tc ToolCall) (string, map[string]any, string) {
	name := tc.Name
	args := tc.Arguments
	thoughtSignature := ""

	if name == "" && tc.Function != nil {
		name = tc.Function.Name
		thoughtSignature = tc.Function.ThoughtSignature
	} else if tc.Function != nil {
		thoughtSignature = tc.Function.ThoughtSignature
	}

	if args == nil {
		args = map[string]any{}
	}

	if len(args) == 0 && tc.Function != nil && tc.Function.Arguments != "" {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &parsed); err == nil && parsed != nil {
			args = parsed
		}
	}

	return name, args, thoughtSignature
}

func resolveToolResponseName(toolCallID string, toolCallNames map[string]string) string {
	if toolCallID == "" {
		return ""
	}

	if name, ok := toolCallNames[toolCallID]; ok && name != "" {
		return name
	}

	return inferToolNameFromCallID(toolCallID)
}

func inferToolNameFromCallID(toolCallID string) string {
	if !strings.HasPrefix(toolCallID, "call_") {
		return toolCallID
	}

	rest := strings.TrimPrefix(toolCallID, "call_")
	if idx := strings.LastIndex(rest, "_"); idx > 0 {
		candidate := rest[:idx]
		if candidate != "" {
			return candidate
		}
	}

	return toolCallID
}

func extractPartThoughtSignature(thoughtSignature string, thoughtSignatureSnake string) string {
	if thoughtSignature != "" {
		return thoughtSignature
	}
	if thoughtSignatureSnake != "" {
		return thoughtSignatureSnake
	}
	return ""
}

var geminiUnsupportedKeywords = map[string]bool{
	"patternProperties":    true,
	"additionalProperties": true,
	"$schema":              true,
	"$id":                  true,
	"$ref":                 true,
	"$defs":                true,
	"definitions":          true,
	"examples":             true,
	"minLength":            true,
	"maxLength":            true,
	"minimum":              true,
	"maximum":              true,
	"multipleOf":           true,
	"pattern":              true,
	"format":               true,
	"minItems":             true,
	"maxItems":             true,
	"uniqueItems":          true,
	"minProperties":        true,
	"maxProperties":        true,
}

func sanitizeSchemaForGemini(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}

	result := make(map[string]any)
	for k, v := range schema {
		if geminiUnsupportedKeywords[k] {
			continue
		}
		switch val := v.(type) {
		case map[string]any:
			result[k] = sanitizeSchemaForGemini(val)
		case []any:
			// Special-case: type union like ["integer","null"] is JSON Schema
			// 2020 syntax that Gemini does NOT support. Convert to single type
			// + nullable: true. Anything else (e.g. enum arrays) passes through.
			if k == "type" {
				primary, hasNull := flattenTypeUnion(val)
				if primary != "" {
					result["type"] = primary
					if hasNull {
						result["nullable"] = true
					}
					continue
				}
			}
			sanitized := make([]any, len(val))
			for i, item := range val {
				if m, ok := item.(map[string]any); ok {
					sanitized[i] = sanitizeSchemaForGemini(m)
				} else {
					sanitized[i] = item
				}
			}
			result[k] = sanitized
		case []string:
			// Same as []any case but for typed-string slices coming from Go literals.
			if k == "type" {
				primary, hasNull := flattenTypeUnionStrings(val)
				if primary != "" {
					result["type"] = primary
					if hasNull {
						result["nullable"] = true
					}
					continue
				}
			}
			out := make([]any, len(val))
			for i, s := range val {
				out[i] = s
			}
			result[k] = out
		default:
			result[k] = v
		}
	}

	if _, hasProps := result["properties"]; hasProps {
		if _, hasType := result["type"]; !hasType {
			result["type"] = "object"
		}
	}

	return result
}

// flattenTypeUnion reduces a JSON Schema type-union like ["integer","null"]
// down to a single primary type plus a nullable boolean. Returns ("", false)
// if the slice is not a recognised union shape.
func flattenTypeUnion(items []any) (primary string, hasNull bool) {
	for _, it := range items {
		s, ok := it.(string)
		if !ok {
			return "", false
		}
		if s == "null" {
			hasNull = true
			continue
		}
		if primary != "" {
			// Two non-null types — Gemini cannot express; bail out, leave
			// the array in place so the caller surfaces the schema error.
			return "", false
		}
		primary = s
	}
	return primary, hasNull
}

// flattenTypeUnionStrings is the []string-typed variant of flattenTypeUnion.
func flattenTypeUnionStrings(items []string) (primary string, hasNull bool) {
	for _, s := range items {
		if s == "null" {
			hasNull = true
			continue
		}
		if primary != "" {
			return "", false
		}
		primary = s
	}
	return primary, hasNull
}

func extractProtocol(model string) (protocol, modelID string) {
	model = strings.TrimSpace(model)
	protocol, modelID, found := strings.Cut(model, "/")
	if !found {
		return "openai", model
	}
	return protocol, modelID
}
