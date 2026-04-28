// PCL-DOWNSTREAM (fix/tgg-tool-execution): unit tests for the 5 TGG inversion tools.
//
// Stage 1 from the C-implementer brief: validate that parser + HTTP POST shape
// is correct against a mock tgg-api. Mock returns the contract's success and
// error envelopes; tool surface returns them verbatim to the LLM.

package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// startMockTGGAPI returns a test server that captures the inbound request and
// responds with the supplied JSON body + status. The captured request is exposed
// via the returned channel for assertions.
func startMockTGGAPI(
	t *testing.T,
	respStatus int,
	respBody string,
) (server *httptest.Server, capturedURL chan string, capturedBody chan string) {
	t.Helper()
	capturedURL = make(chan string, 5)
	capturedBody = make(chan string, 5)

	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL <- r.Method + " " + r.URL.RequestURI()
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			capturedBody <- string(b)
		} else {
			capturedBody <- ""
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(respStatus)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(server.Close)
	t.Setenv("TGG_API_URL", server.URL)
	return server, capturedURL, capturedBody
}

func TestTGGTool_CaseCreate_HappyPath(t *testing.T) {
	respJSON := `{"ok":true,"case":{"id":167,"job_no":"AM/JOB/2604/0411","state":"new"},"reply":"Got it — case logged."}`
	_, capturedURL, capturedBody := startMockTGGAPI(t, 201, respJSON)

	tool := newCaseCreateTool()
	args := map[string]any{
		"job_no":        "AM/JOB/2604/0411",
		"address":       "Blk 215 AMK Ave 4 #06-1334",
		"unit":          "#06-1334",
		"tenant_name":   "Mdm Goh",
		"contact_phone": "92223334",
		"problem":       "water seeping under flush valve",
		"source_msg_id": "tg-msg-12345",
		"officer_name":  "Sharon Chia",
	}

	result := tool.Execute(context.Background(), args)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.ContentForLLM())
	}
	if !strings.Contains(result.ContentForLLM(), "AM/JOB/2604/0411") {
		t.Fatalf("expected response body returned to LLM, got: %s", result.ContentForLLM())
	}

	gotURL := <-capturedURL
	if gotURL != "POST /api/tools/case.create" {
		t.Errorf("expected POST /api/tools/case.create, got %s", gotURL)
	}

	gotBody := <-capturedBody
	var parsed map[string]any
	if err := json.Unmarshal([]byte(gotBody), &parsed); err != nil {
		t.Fatalf("body not valid JSON: %v\nbody: %s", err, gotBody)
	}
	if parsed["job_no"] != "AM/JOB/2604/0411" {
		t.Errorf("body missing job_no: %+v", parsed)
	}
	if parsed["officer_name"] != "Sharon Chia" {
		t.Errorf("body missing officer_name: %+v", parsed)
	}
}

func TestTGGTool_CaseCreate_MissingRequired(t *testing.T) {
	// No mock — execution should short-circuit before the HTTP call.
	t.Setenv("TGG_API_URL", "http://127.0.0.1:1") // hard-fail if reached

	tool := newCaseCreateTool()
	args := map[string]any{
		"job_no": "AM/JOB/2604/0411",
		// address, source_msg_id, officer_name missing
	}
	result := tool.Execute(context.Background(), args)
	if result == nil || !result.IsError {
		t.Fatalf("expected IsError=true for missing required, got: %+v", result)
	}
	if !strings.Contains(result.ForLLM, "address") {
		t.Errorf("expected error to name missing field 'address', got: %s", result.ForLLM)
	}
}

func TestTGGTool_CaseCreate_EmptyRequiredString(t *testing.T) {
	t.Setenv("TGG_API_URL", "http://127.0.0.1:1")

	tool := newCaseCreateTool()
	args := map[string]any{
		"job_no":        "AM/JOB/2604/0411",
		"address":       "   ", // whitespace-only counts as empty
		"source_msg_id": "tg-1",
		"officer_name":  "Sharon",
	}
	result := tool.Execute(context.Background(), args)
	if !result.IsError {
		t.Fatalf("expected error for whitespace address, got: %+v", result)
	}
	if !strings.Contains(result.ForLLM, "address") {
		t.Errorf("expected error to name field 'address', got: %s", result.ForLLM)
	}
}

func TestTGGTool_CaseCreate_ServerErrorEnvelope_NotMarkedAsError(t *testing.T) {
	// 4xx with proper JSON envelope: the tool returns the body verbatim
	// (NOT IsError=true) so the LLM can craft a natural-language reply.
	respJSON := `{"ok":false,"error":{"code":"case_already_exists","message":"Case AM/JOB/2604/0411 already exists","case_id":167}}`
	_, _, _ = startMockTGGAPI(t, 409, respJSON)

	tool := newCaseCreateTool()
	args := map[string]any{
		"job_no":        "AM/JOB/2604/0411",
		"address":       "Blk 215",
		"source_msg_id": "tg-1",
		"officer_name":  "Sharon",
	}
	result := tool.Execute(context.Background(), args)
	if result.IsError {
		t.Fatalf("expected IsError=false (LLM should see body), got error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ContentForLLM(), "case_already_exists") {
		t.Errorf("expected envelope to reach LLM, got: %s", result.ContentForLLM())
	}
}

func TestTGGTool_CaseCreate_5xxIsError(t *testing.T) {
	// 5xx surfaces as IsError so the LLM knows it was infrastructure failure.
	_, _, _ = startMockTGGAPI(t, 502, `{"error":"upstream"}`)

	tool := newCaseCreateTool()
	args := map[string]any{
		"job_no":        "AM/JOB/2604/0411",
		"address":       "Blk 215",
		"source_msg_id": "tg-1",
		"officer_name":  "Sharon",
	}
	result := tool.Execute(context.Background(), args)
	if !result.IsError {
		t.Fatal("expected 5xx to surface as IsError")
	}
}

func TestTGGTool_CaseCreate_NonJSONResponseIsError(t *testing.T) {
	_, _, _ = startMockTGGAPI(t, 200, `<html><body>404 not found</body></html>`)

	tool := newCaseCreateTool()
	args := map[string]any{
		"job_no":        "AM/JOB/2604/0411",
		"address":       "Blk 215",
		"source_msg_id": "tg-1",
		"officer_name":  "Sharon",
	}
	result := tool.Execute(context.Background(), args)
	if !result.IsError {
		t.Fatal("expected non-JSON to surface as IsError")
	}
	if !strings.Contains(result.ForLLM, "non-JSON") {
		t.Errorf("expected non-JSON error message, got: %s", result.ForLLM)
	}
}

func TestTGGTool_WorkerReport_HappyPath(t *testing.T) {
	respJSON := `{"ok":true,"case":{"id":167,"state":"completed"},"transition":{"from":"in_progress","to":"completed"}}`
	_, capturedURL, capturedBody := startMockTGGAPI(t, 200, respJSON)

	tool := newWorkerReportTool()
	args := map[string]any{
		"case_id":       float64(167), // JSON numbers come in as float64
		"status":        "completed",
		"observation":   "replaced flush valve, no leak",
		"source_msg_id": "tg-msg-9999",
		"worker_name":   "Muthu",
	}
	result := tool.Execute(context.Background(), args)
	if result.IsError {
		t.Fatalf("expected success, got: %s", result.ForLLM)
	}

	gotURL := <-capturedURL
	if gotURL != "POST /api/tools/worker.report" {
		t.Errorf("expected POST /api/tools/worker.report, got %s", gotURL)
	}

	gotBody := <-capturedBody
	if !strings.Contains(gotBody, `"status":"completed"`) {
		t.Errorf("expected status in body, got %s", gotBody)
	}
	if !strings.Contains(gotBody, `"worker_name":"Muthu"`) {
		t.Errorf("expected worker_name in body, got %s", gotBody)
	}
}

func TestTGGTool_CaseAttachPhoto_HappyPath(t *testing.T) {
	respJSON := `{"ok":true,"attached":[{"url":"/media/incoming/abc/photo1.jpg","size":12345,"contentType":"image/jpeg"}]}`
	_, capturedURL, capturedBody := startMockTGGAPI(t, 200, respJSON)

	tool := newCaseAttachPhotoTool()
	args := map[string]any{
		"case_id":       float64(167),
		"photo_paths":   []any{"/tmp/picoclaw_media/abc12345_photo.jpg", "/tmp/picoclaw_media/def67890_photo.jpg"},
		"source_msg_id": "tg-msg-9998",
		"worker_name":   "Justin",
	}
	result := tool.Execute(context.Background(), args)
	if result.IsError {
		t.Fatalf("expected success, got: %s", result.ForLLM)
	}

	gotURL := <-capturedURL
	if gotURL != "POST /api/tools/case.attach_photo" {
		t.Errorf("expected POST /api/tools/case.attach_photo, got %s", gotURL)
	}

	gotBody := <-capturedBody
	if !strings.Contains(gotBody, "abc12345_photo.jpg") || !strings.Contains(gotBody, "def67890_photo.jpg") {
		t.Errorf("expected both photo paths in body, got %s", gotBody)
	}
}

func TestTGGTool_CaseUpdate_NestedFieldsObject(t *testing.T) {
	respJSON := `{"ok":true,"case":{"id":167,"unit":"#06-1335"}}`
	_, capturedURL, capturedBody := startMockTGGAPI(t, 200, respJSON)

	tool := newCaseUpdateTool()
	args := map[string]any{
		"case_id": float64(167),
		"fields": map[string]any{
			"unit":          "#06-1335",
			"contact_phone": "92223334",
		},
		"source_msg_id": "tg-msg-9997",
	}
	result := tool.Execute(context.Background(), args)
	if result.IsError {
		t.Fatalf("expected success, got: %s", result.ForLLM)
	}

	gotURL := <-capturedURL
	if gotURL != "POST /api/tools/case.update" {
		t.Errorf("expected POST /api/tools/case.update, got %s", gotURL)
	}

	gotBody := <-capturedBody
	if !strings.Contains(gotBody, `"unit":"#06-1335"`) {
		t.Errorf("expected nested fields.unit in body, got %s", gotBody)
	}
}

func TestTGGTool_CaseResolve_ZeroMatches(t *testing.T) {
	respJSON := `{"ok":true,"matches":[]}`
	_, capturedURL, _ := startMockTGGAPI(t, 200, respJSON)

	tool := newCaseResolveTool()
	args := map[string]any{
		"query":       "block 410 update",
		"worker_name": "Muthu",
	}
	result := tool.Execute(context.Background(), args)
	if result.IsError {
		t.Fatalf("expected success, got: %s", result.ForLLM)
	}
	if !strings.Contains(result.ContentForLLM(), `"matches":[]`) {
		t.Errorf("expected empty matches in result, got: %s", result.ContentForLLM())
	}

	gotURL := <-capturedURL
	if gotURL != "POST /api/tools/case.resolve" {
		t.Errorf("expected POST /api/tools/case.resolve, got %s", gotURL)
	}
}

func TestNewTGGTools_AllFiveRegistered(t *testing.T) {
	tools := NewTGGTools()
	if len(tools) != 5 {
		t.Fatalf("expected 5 tools, got %d", len(tools))
	}
	expected := map[string]bool{
		"case_create":       false,
		"case_update":       false,
		"case_attach_photo": false,
		"worker_report":     false,
		"case_resolve":      false,
	}
	for _, tt := range tools {
		if _, ok := expected[tt.Name()]; !ok {
			t.Errorf("unexpected tool: %s", tt.Name())
		}
		expected[tt.Name()] = true
	}
	for name, found := range expected {
		if !found {
			t.Errorf("missing tool: %s", name)
		}
	}
}
