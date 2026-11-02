package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func clearTibiEnv(t *testing.T) {
	t.Helper()
	for _, k := range envNames {
		t.Setenv(k, "")
	}
}

func testApp(t *testing.T) (*app, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := newApp(st)
	if err != nil {
		t.Fatal(err)
	}
	return a, dir
}

func call(t *testing.T, h http.Handler, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if strings.Contains(rec.Body.String(), "private-pin") {
		t.Fatal("PIN returned")
	}
	return rec.Code, out
}

func field(doc map[string]any, key string) map[string]any {
	for _, f := range doc["fields"].([]any) {
		if m := f.(map[string]any); m["key"] == key {
			return m
		}
	}
	return nil
}

func toolNames(t *testing.T, s *mcp.Server) []string {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := s.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

func TestConfigAPI(t *testing.T) {
	clearTibiEnv(t)
	a, dir := testApp(t)
	h := newHTTPHandler(a.server, "mcp-token", func(mux *http.ServeMux) { a.registerConfigAPI(mux, "config-token") })

	if code, _ := call(t, h, "GET", "/healthz", "", ""); code != 200 {
		t.Fatalf("plain healthz %d", code)
	}
	for _, path := range []string{"/config", "/healthz?check=1"} {
		if code, _ := call(t, h, "GET", path, "", ""); code != 401 {
			t.Fatalf("%s without token: %d", path, code)
		}
		if code, _ := call(t, h, "GET", path, "mcp-token", ""); code != 401 {
			t.Fatalf("%s accepted the MCP token instead of the config token: %d", path, code)
		}
	}
	code, doc := call(t, h, "GET", "/config", "config-token", "")
	if code != 200 || doc["name"] != "tibi-mcp" || doc["configured"] != false || field(doc, "MCP_READ_ONLY")["value"] != "true" {
		t.Fatalf("initial config %d %v", code, doc)
	}
	if code, hd := call(t, h, "GET", "/healthz?check=1", "config-token", ""); code != 503 || hd["status"] != "not configured" {
		t.Fatalf("unconfigured check %d %v", code, hd)
	}
	if got := strings.Join(toolNames(t, a.server), ","); strings.Contains(got, "borrow_ebook") {
		t.Fatalf("read-only by default, got %s", got)
	}

	code, doc = call(t, h, "POST", "/config", "config-token", `{"values":{"TIBI_CARD_NUMBER":"123456","TIBI_PIN":"private-pin","MCP_READ_ONLY":"false"}}`)
	if code != 200 || doc["configured"] != true || field(doc, "TIBI_PIN")["set"] != true || field(doc, "TIBI_PIN")["value"] != "" || field(doc, "TIBI_CARD_NUMBER")["value"] != "123456" {
		t.Fatalf("after save %d %v", code, doc)
	}
	if got := strings.Join(toolNames(t, a.server), ","); !strings.Contains(got, "borrow_ebook") || !strings.Contains(got, "set_kindle_email") {
		t.Fatalf("write tools not added: %s", got)
	}
	info, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode %v err %v", info, err)
	}

	// An empty PIN keeps the stored one; clearing read-only restores the default.
	if code, doc = call(t, h, "POST", "/config", "config-token", `{"values":{"TIBI_PIN":""},"clear":["MCP_READ_ONLY"]}`); code != 200 || field(doc, "TIBI_PIN")["set"] != true {
		t.Fatalf("empty PIN %d %v", code, doc)
	}
	if got := strings.Join(toolNames(t, a.server), ","); strings.Contains(got, "borrow_ebook") {
		t.Fatalf("write tools not removed: %s", got)
	}

	for _, body := range []string{`{"values":{"TIBI_CARD_NUMBER":"12ab"}}`, `{"values":{"NOPE":"x"}}`, `{"values":{"MCP_READ_ONLY":"maybe"}}`, `not json`} {
		if code, _ := call(t, h, "POST", "/config", "config-token", body); code != 400 {
			t.Fatalf("bad update %s accepted: %d", body, code)
		}
	}

	// Settings survive a restart.
	st, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := newApp(st)
	if err != nil || !b.current().ready() || b.current().CardNumber != "123456" {
		t.Fatalf("settings not reloaded: %v", err)
	}
}

func TestConfigEnvironmentIsLocked(t *testing.T) {
	clearTibiEnv(t)
	t.Setenv("TIBI_CARD_NUMBER", "654321")
	t.Setenv("TIBI_PIN", "private-pin")
	a, _ := testApp(t)
	h := newHTTPHandler(a.server, "token", func(mux *http.ServeMux) { a.registerConfigAPI(mux, "token") })
	code, doc := call(t, h, "GET", "/config", "token", "")
	if code != 200 || doc["configured"] != true || field(doc, "TIBI_PIN")["locked"] != true || field(doc, "MCP_READ_ONLY")["locked"] != false {
		t.Fatalf("locked fields %d %v", code, doc)
	}
	for _, body := range []string{`{"values":{"TIBI_PIN":"other"}}`, `{"clear":["TIBI_CARD_NUMBER"]}`} {
		if code, _ := call(t, h, "POST", "/config", "token", body); code != 400 {
			t.Fatalf("locked setting changed by %s: %d", body, code)
		}
	}
}

func TestHealthCheckLogsInWithoutLeakingTheKindleAddress(t *testing.T) {
	clearTibiEnv(t)
	t.Setenv("TIBI_CARD_NUMBER", "654321")
	t.Setenv("TIBI_PIN", "private-pin")
	for _, tc := range []struct {
		name, kindle string
		auth         bool
		code         int
		detail       string
	}{
		{"kindle set", "private-reader@kindle.com", true, 200, "set"},
		{"kindle missing", "", true, 200, "not set"},
		{"login fails", "", false, 503, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/boker/book/GetBookPageByEdition":
					json.NewEncoder(w).Encode(fakePage(tc.auth, false, false, 4))
				case "/boker/patron/GetPatronData":
					json.NewEncoder(w).Encode(map[string]any{"kindleEmail": tc.kindle, "email": "private-mail@example.com"})
				default:
					// Login is attempted when the page is not authenticated.
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			st, _ := openStore(t.TempDir())
			a := &app{store: st, newClient: func(card, pin string) *client {
				c := newClient(card, pin)
				c.base = srv.URL
				return c
			}}
			a.env, a.envSet, _ = fromEnv()
			a.server = newServer(a, true)
			a.reload()
			h := newHTTPHandler(a.server, "token", func(mux *http.ServeMux) { a.registerConfigAPI(mux, "token") })
			req := httptest.NewRequest("GET", "/healthz?check=1", nil)
			req.Header.Set("Authorization", "Bearer token")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.detail) {
				t.Fatalf("check %d %s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "private-") {
				t.Fatal("health check leaked private data")
			}
		})
	}
}

func TestAccountToolsExplainMissingSettings(t *testing.T) {
	clearTibiEnv(t)
	a, _ := testApp(t)
	ct, st := mcp.NewInMemoryTransports()
	ss, err := a.server.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_kindle_email", Arguments: map[string]any{}})
	if err != nil || !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "not set up") {
		t.Fatalf("missing settings not explained: %v", err)
	}
}
