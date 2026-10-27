package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/store"
)

func pickFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func TestRunWebServer_Endpoints(t *testing.T) {
	tmp := t.TempDir()
	st, err := store.Open(tmp + "/uta.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// Seed a session with a secret in the goal to verify redaction.
	if err := st.CreateSession(store.Session{
		ID:        "12345678-1111-2222-3333-444444444444",
		Goal:      "deploy with token sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789ABCDEF",
		Worker:    "claude",
		Status:    "completed",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	app := &App{
		StateDir:    tmp,
		ProjectRoot: tmp,
		ProjectName: `test-proj<script>alert(1)</script>`,
		Store:       st,
		Registry:    provider.NewRegistry(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var errOut bytes.Buffer
	port := pickFreePort(t)

	go func() {
		_ = runWebServer(ctx, app, port, &errOut)
	}()

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	var resp *http.Response
	for i := 0; i < 30; i++ {
		resp, err = http.Get(baseURL + "/")
		if err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET / status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	htmlStr := string(body)
	if !strings.Contains(htmlStr, "uta dashboard") {
		t.Errorf("HTML body missing 'uta dashboard': %s", htmlStr)
	}
	if strings.Contains(htmlStr, "<script>alert(1)</script>") {
		t.Errorf("HTML body did not escape ProjectName XSS payload")
	}
	if !strings.Contains(htmlStr, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("HTML body missing escaped ProjectName")
	}

	// 2. Test API Dash Endpoint (summary + panes)
	respDash, err := http.Get(baseURL + "/api/dash")
	if err != nil {
		t.Fatalf("GET /api/dash: %v", err)
	}
	defer respDash.Body.Close()
	if respDash.StatusCode != http.StatusOK {
		t.Errorf("GET /api/dash status = %d, want 200", respDash.StatusCode)
	}
	var dashPayload map[string]any
	if err := json.NewDecoder(respDash.Body).Decode(&dashPayload); err != nil {
		t.Fatalf("decode /api/dash: %v", err)
	}
	summary, ok := dashPayload["summary"].(map[string]any)
	if !ok {
		t.Fatalf("/api/dash missing summary object: %v", dashPayload)
	}
	if got, _ := summary["sessions_total"].(float64); got != 1 {
		t.Errorf("summary.sessions_total = %v, want 1", got)
	}

	// 3. Test API Sessions Endpoint (redaction)
	respSess, err := http.Get(baseURL + "/api/sessions")
	if err != nil {
		t.Fatalf("GET /api/sessions: %v", err)
	}
	defer respSess.Body.Close()
	if respSess.StatusCode != http.StatusOK {
		t.Errorf("GET /api/sessions status = %d, want 200", respSess.StatusCode)
	}
	sessBody, _ := io.ReadAll(respSess.Body)
	if strings.Contains(string(sessBody), "sk-ant-api03-") {
		t.Errorf("/api/sessions leaked unredacted secret: %s", string(sessBody))
	}

	// 4. Test API Providers Endpoint (returns JSON array)
	respProv, err := http.Get(baseURL + "/api/providers")
	if err != nil {
		t.Fatalf("GET /api/providers: %v", err)
	}
	defer respProv.Body.Close()
	if respProv.StatusCode != http.StatusOK {
		t.Errorf("GET /api/providers status = %d, want 200", respProv.StatusCode)
	}
	var provList []map[string]any
	if err := json.NewDecoder(respProv.Body).Decode(&provList); err != nil {
		t.Fatalf("/api/providers did not return JSON array: %v", err)
	}

	// 5. Test API HITL Action security (path traversal + cross-origin)
	travReq, _ := http.NewRequest(http.MethodPost, baseURL+"/api/hitl/action",
		strings.NewReader(`{"session_id":"../etc","action_id":"act1","deny":false}`))
	travResp, err := http.DefaultClient.Do(travReq)
	if err != nil {
		t.Fatalf("POST /api/hitl/action traversal: %v", err)
	}
	_ = travResp.Body.Close()
	if travResp.StatusCode != http.StatusBadRequest {
		t.Errorf("traversal status = %d, want 400", travResp.StatusCode)
	}

	csrfReq, _ := http.NewRequest(http.MethodPost, baseURL+"/api/hitl/action",
		strings.NewReader(`{"session_id":"sess1","action_id":"act1","deny":false}`))
	csrfReq.Header.Set("Origin", "https://evil.example.com")
	csrfResp, err := http.DefaultClient.Do(csrfReq)
	if err != nil {
		t.Fatalf("POST /api/hitl/action csrf: %v", err)
	}
	_ = csrfResp.Body.Close()
	if csrfResp.StatusCode != http.StatusForbidden {
		t.Errorf("csrf status = %d, want 403", csrfResp.StatusCode)
	}

	// 6. Test API Replay Endpoint
	respReplay, err := http.Get(baseURL + "/api/replay?session=12345678")
	if err != nil {
		t.Fatalf("GET /api/replay: %v", err)
	}
	defer respReplay.Body.Close()
	if respReplay.StatusCode != http.StatusOK {
		t.Errorf("GET /api/replay status = %d, want 200", respReplay.StatusCode)
	}

	cancel() // shutdown web server cleanly
}
