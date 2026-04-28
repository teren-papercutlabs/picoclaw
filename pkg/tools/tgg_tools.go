// PCL-DOWNSTREAM (fix/tgg-tool-execution): TGG inversion tools.
//
// Five tools the agent calls instead of being prompted to curl /api/ingest/wa-message.
// Each tool validates its own JSON arguments against the contract schema, POSTs to
// the corresponding tgg-api /api/tools/<name> endpoint, and returns the response
// body to the LLM as the tool result so the next inference iteration can craft
// the user reply.
//
// Contract: ~/pcl-biz/_agents/edna/specs/2026-04-27-7f6833c5-tgg-attention-mock/inversion-tool-contract.md
//
// All five tools share the same Execute() body — only Name/Description/Parameters
// differ. We build them via newTGGTool() so adding/changing one is mechanical.

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/teren-papercutlabs/pclaw/pkg/logger"
)

// TGGAPIBaseURL is resolved at tool-init time; falls back to localhost:3501.
// Override with env var TGG_API_URL.
func tggAPIBaseURL() string {
	if v := strings.TrimSpace(os.Getenv("TGG_API_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://127.0.0.1:3501"
}

// httpClientForTGG is a single shared client with a sensible timeout.
// 15s is generous for localhost POSTs but short enough that a wedged tgg-api
// surfaces fast to the LLM (which can then escalate or retry).
var httpClientForTGG = &http.Client{Timeout: 15 * time.Second}

// TGGTool is one of the 5 inversion tools (case.create, case.update, etc.).
// All instances share the same Execute() — only the schema and endpoint vary.
type TGGTool struct {
	name        string                 // dotted tool name, e.g. "case.create"
	description string                 // sent to LLM as part of the tool definition
	parameters  map[string]any         // JSON schema for arguments
	method      string                 // "POST" or "GET"
	endpoint    string                 // path: "/api/tools/case.create"
	required    []string               // field names that must be present + non-empty in args
}

// Name implements toolshared.Tool.
func (t *TGGTool) Name() string { return t.name }

// Description implements toolshared.Tool.
func (t *TGGTool) Description() string { return t.description }

// Parameters implements toolshared.Tool.
func (t *TGGTool) Parameters() map[string]any { return t.parameters }

// Execute implements toolshared.Tool.
//
// Flow:
//  1. Validate required fields are present + non-empty (cheap pre-check; the registry
//     already validates against the JSON schema, but we additionally enforce that
//     "required" fields are not empty strings — schema validation accepts "" for
//     a required string).
//  2. Marshal args to JSON.
//  3. POST to tgg-api endpoint.
//  4. Read response body, return to LLM verbatim. The body is a JSON envelope
//     (`{"ok": true, "case": {...}, "reply": "..."}` or `{"ok": false, "error": ...}`)
//     and Gemini handles natural-language synthesis from there.
func (t *TGGTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	// 1. Required-field check.
	for _, key := range t.required {
		v, ok := args[key]
		if !ok {
			return ErrorResult(fmt.Sprintf(
				"tgg tool %q: missing required field %q. provide it in the arguments and retry.",
				t.name, key,
			))
		}
		if s, isStr := v.(string); isStr && strings.TrimSpace(s) == "" {
			return ErrorResult(fmt.Sprintf(
				"tgg tool %q: required field %q is empty. provide a non-empty value and retry.",
				t.name, key,
			))
		}
	}

	// 2. Marshal args.
	body, err := json.Marshal(args)
	if err != nil {
		return ErrorResult(fmt.Sprintf(
			"tgg tool %q: failed to marshal arguments to JSON: %v",
			t.name, err,
		)).WithError(err)
	}

	// 3. Build HTTP request.
	endpointURL := tggAPIBaseURL() + t.endpoint

	var req *http.Request
	if t.method == http.MethodGet {
		// case.resolve supports GET. Append args as query string. Skip non-string values
		// for GET — agents should use POST for structured queries per the contract.
		q := url.Values{}
		for k, v := range args {
			if s, ok := v.(string); ok {
				q.Set(k, s)
			}
		}
		full := endpointURL
		if len(q) > 0 {
			full += "?" + q.Encode()
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(body))
		if req != nil {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	if err != nil {
		return ErrorResult(fmt.Sprintf(
			"tgg tool %q: failed to build HTTP request: %v",
			t.name, err,
		)).WithError(err)
	}

	logger.InfoCF("tgg-tool", "tgg tool POST",
		map[string]any{
			"tool":     t.name,
			"endpoint": t.endpoint,
			"method":   req.Method,
		})

	// 4. POST and read response.
	resp, err := httpClientForTGG.Do(req)
	if err != nil {
		return ErrorResult(fmt.Sprintf(
			"tgg tool %q: HTTP request to %s failed: %v. tgg-api may be down. let the principal know if this persists.",
			t.name, endpointURL, err,
		)).WithError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return ErrorResult(fmt.Sprintf(
			"tgg tool %q: failed to read response body: %v",
			t.name, err,
		)).WithError(err)
	}

	respText := string(respBytes)
	logger.InfoCF("tgg-tool", "tgg tool response",
		map[string]any{
			"tool":   t.name,
			"status": resp.StatusCode,
			"bytes":  len(respBytes),
		})

	// Contract Section 9: error envelope is `{"ok": false, "error": {...}}`.
	// We do NOT mark non-2xx as IsError on the ToolResult — we want the LLM to
	// SEE the error envelope so it can craft a natural-language clarification
	// to the user (e.g. "Case AM/JOB/2604/0411 already exists, want me to update it?").
	// Marking IsError=true would short-circuit the agent loop. The body itself
	// carries enough signal via `ok: false` + `error.code` + `error.message`.
	//
	// Hard-fail (5xx, no JSON) is still surfaced as an error result so the LLM
	// knows it was infrastructure, not user-input.
	if resp.StatusCode >= 500 {
		return ErrorResult(fmt.Sprintf(
			"tgg tool %q: server returned %d. body: %s",
			t.name, resp.StatusCode, truncateForLLM(respText, 1000),
		))
	}

	// Sanity-check that we got JSON. If not, surface as error so the LLM doesn't
	// try to parse a 404 HTML page as a tool result.
	respText = strings.TrimSpace(respText)
	if !strings.HasPrefix(respText, "{") && !strings.HasPrefix(respText, "[") {
		return ErrorResult(fmt.Sprintf(
			"tgg tool %q: server returned non-JSON (status %d). first 500 chars: %s",
			t.name, resp.StatusCode, truncateForLLM(respText, 500),
		))
	}

	return NewToolResult(respText)
}

func truncateForLLM(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...[truncated]"
}

// --- Tool factory ---

// NewTGGTools returns the 5 inversion tools, ready to register on an agent.
// Returned in stable order for deterministic registration.
func NewTGGTools() []Tool {
	return []Tool{
		newCaseCreateTool(),
		newCaseUpdateTool(),
		newCaseAttachPhotoTool(),
		newWorkerReportTool(),
		newCaseResolveTool(),
	}
}

// --- Individual tool definitions ---
//
// Each function returns a *TGGTool with the schema baked in. Schemas mirror the
// contract (~/pcl-biz/_agents/edna/specs/2026-04-27-7f6833c5-tgg-attention-mock/
// inversion-tool-contract.md) verbatim. If the contract changes, update here.

func newCaseCreateTool() *TGGTool {
	return &TGGTool{
		name: "case_create",
		description: "Create a new TGG case from an officer's structured post. " +
			"Use when an officer (sender role 'officer') posts a NEW case announcement " +
			"with a job_no like AM/JOB/2604/0411 and an address. " +
			"Returns the created case row. " +
			"If the case already exists you'll get error code 'case_already_exists' with " +
			"the existing case_id — fall back to case_update with that id.",
		method:   http.MethodPost,
		endpoint: "/api/tools/case.create",
		required: []string{"job_no", "address", "source_msg_id", "officer_name"},
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"job_no": map[string]any{
					"type":        "string",
					"description": "Job number, format XX/JOB/YYMM/NNNN (e.g. AM/JOB/2604/0411). REQUIRED.",
				},
				"address": map[string]any{
					"type":        "string",
					"description": "Full address as posted by officer. REQUIRED.",
				},
				"unit": map[string]any{
					"type":        "string",
					"description": "Unit number, e.g. '#06-1334'. Optional.",
				},
				"block": map[string]any{
					"type":        "string",
					"description": "Block, e.g. 'Blk 215'. Optional.",
				},
				"zone": map[string]any{
					"type":        "string",
					"description": "Zone code, e.g. 'AM'. Optional — server derives from job_no prefix if omitted.",
				},
				"tenant_name": map[string]any{
					"type":        "string",
					"description": "Tenant display name, e.g. 'Mdm Goh'. Optional but expected.",
				},
				"contact_phone": map[string]any{
					"type":        "string",
					"description": "Tenant contact phone, E.164-ish. Optional.",
				},
				"problem": map[string]any{
					"type":        "string",
					"description": "Problem description. Plain text, optional but expected.",
				},
				"due_at": map[string]any{
					"type":        []string{"integer", "null"},
					"description": "Unix seconds for due date. If null/omitted, server defaults to now+7days.",
				},
				"source_msg_id": map[string]any{
					"type":        "string",
					"description": "Telegram message ID for traceability. REQUIRED.",
				},
				"officer_name": map[string]any{
					"type":        "string",
					"description": "Officer display name from header prefix. REQUIRED.",
				},
			},
			"required": []string{"job_no", "address", "source_msg_id", "officer_name"},
		},
	}
}

func newCaseUpdateTool() *TGGTool {
	return &TGGTool{
		name: "case_update",
		description: "Update fields on an existing case (officer correction, added tenant, due-date change, etc). " +
			"DO NOT use this to change state (use worker_report) or job_no (immutable). " +
			"Use after case_create returned 'case_already_exists' for a known case_id, " +
			"or when an officer posts a correction.",
		method:   http.MethodPost,
		endpoint: "/api/tools/case.update",
		required: []string{"case_id", "source_msg_id"},
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"case_id": map[string]any{
					"type":        "integer",
					"description": "Existing case id. REQUIRED.",
				},
				"fields": map[string]any{
					"type":        "object",
					"description": "Map of column updates. Whitelisted: tenant_name, contact_phone, problem, unit, block, zone, address, due_at. Server rejects 'state' and 'job_no'.",
					"properties": map[string]any{
						"tenant_name":   map[string]any{"type": "string"},
						"contact_phone": map[string]any{"type": "string"},
						"problem":       map[string]any{"type": "string"},
						"unit":          map[string]any{"type": "string"},
						"block":         map[string]any{"type": "string"},
						"zone":          map[string]any{"type": "string"},
						"address":       map[string]any{"type": "string"},
						"due_at":        map[string]any{"type": "integer"},
					},
				},
				"source_msg_id": map[string]any{
					"type":        "string",
					"description": "Telegram message ID. REQUIRED.",
				},
			},
			"required": []string{"case_id", "fields", "source_msg_id"},
		},
	}
}

func newCaseAttachPhotoTool() *TGGTool {
	return &TGGTool{
		name: "case_attach_photo",
		description: "Attach photo file paths (already downloaded by picoclaw to /tmp/picoclaw_media/) to a case. " +
			"Use when a worker posts a photo without a status update — pure photo attachment. " +
			"For photos accompanying a worker status update, prefer worker_report with photo_paths inside.",
		method:   http.MethodPost,
		endpoint: "/api/tools/case.attach_photo",
		required: []string{"case_id", "source_msg_id", "worker_name"},
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"case_id": map[string]any{
					"type":        "integer",
					"description": "Existing case id. REQUIRED.",
				},
				"photo_paths": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Array of /tmp/picoclaw_media/ paths extracted from [image: <path>] markers. REQUIRED.",
				},
				"source_msg_id": map[string]any{
					"type":        "string",
					"description": "Telegram message ID. REQUIRED.",
				},
				"worker_name": map[string]any{
					"type":        "string",
					"description": "Worker display name from header prefix. REQUIRED.",
				},
			},
			"required": []string{"case_id", "photo_paths", "source_msg_id", "worker_name"},
		},
	}
}

func newWorkerReportTool() *TGGTool {
	return &TGGTool{
		name: "worker_report",
		description: "Worker status update — drives the case state machine. " +
			"Use when a worker (sender role 'worker') reports completion, partial work, in-progress, or blocked. " +
			"You must first identify the case_id (use case_resolve if the worker referenced fuzzily). " +
			"If the worker also attached photos, include their paths in photo_paths so the server attaches " +
			"them atomically with the status transition.",
		method:   http.MethodPost,
		endpoint: "/api/tools/worker.report",
		required: []string{"case_id", "status", "source_msg_id", "worker_name"},
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"case_id": map[string]any{
					"type":        "integer",
					"description": "Existing case id. REQUIRED.",
				},
				"status": map[string]any{
					"type":        "string",
					"enum":        []string{"in_progress", "completed", "partial_complete", "blocked"},
					"description": "Status enum. REQUIRED.",
				},
				"partial_complete": map[string]any{
					"type":        "boolean",
					"description": "If true with status=in_progress, transitions to partial_complete. Default false.",
				},
				"observation": map[string]any{
					"type":        "string",
					"description": "Worker's plain-text observation (what they did or saw). Optional but recommended for completed/blocked.",
				},
				"photo_paths": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional /tmp/picoclaw_media/ paths to attach atomically with the report.",
				},
				"source_msg_id": map[string]any{
					"type":        "string",
					"description": "Telegram message ID. REQUIRED.",
				},
				"worker_name": map[string]any{
					"type":        "string",
					"description": "Worker display name from header prefix. REQUIRED.",
				},
			},
			"required": []string{"case_id", "status", "source_msg_id", "worker_name"},
		},
	}
}

func newCaseResolveTool() *TGGTool {
	return &TGGTool{
		name: "case_resolve",
		description: "Find a case_id from a fuzzy reference (e.g. '0301', 'the AMK case', 'Justin's last one', " +
			"'Blk 410 #08-1234'). Use FIRST when a worker reports without a clean job_no, before worker_report. " +
			"Returns ranked matches with confidence. " +
			"If 0 matches, ask the user to clarify (no auto-merge — false matches poison the case set).",
		method:   http.MethodPost,
		endpoint: "/api/tools/case.resolve",
		required: []string{},
		parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Free-text query — full job_no, last 4 digits, or any text the user said.",
				},
				"worker_name": map[string]any{
					"type":        "string",
					"description": "Worker display name to scope a recent text-link match.",
				},
				"block": map[string]any{
					"type":        "string",
					"description": "Block, e.g. 'Blk 410'. Combine with unit for a structured lookup.",
				},
				"unit": map[string]any{
					"type":        "string",
					"description": "Unit, e.g. '#08-1234'. Combine with block.",
				},
				"recent_window_min": map[string]any{
					"type":        "integer",
					"description": "How recent (minutes) for worker text-link match. Default 90.",
				},
			},
		},
	}
}
