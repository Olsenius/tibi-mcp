package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type fakeAPI struct {
	calls []string
	err   error
}

func (f *fakeAPI) result(name string) (any, error) {
	f.calls = append(f.calls, name)
	return map[string]string{"status": "ok"}, f.err
}
func (f *fakeAPI) Search(context.Context, string, int) (any, error) { return f.result("search") }
func (f *fakeAPI) Book(context.Context, string) (any, error)        { return f.result("book") }
func (f *fakeAPI) Borrow(context.Context, string) (any, error)      { return f.result("borrow") }
func (f *fakeAPI) SendKindle(context.Context, string) (any, error)  { return f.result("kindle") }
func (f *fakeAPI) EPUBLink(context.Context, string) (any, error)    { return f.result("epub") }
func (f *fakeAPI) KindleEmail(context.Context) (any, error)         { return f.result("kindle_email") }
func (f *fakeAPI) SetKindleEmail(context.Context, string) (any, error) {
	return f.result("set_kindle_email")
}

func session(t *testing.T, api tibiAPI, readOnly bool) *mcp.ClientSession {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := newServer(api, readOnly).Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestReadOnlyDoesNotExposeWrites(t *testing.T) {
	f := &fakeAPI{}
	cs := session(t, f, true)
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 4 {
		t.Fatalf("got %d tools", len(list.Tools))
	}
	for _, name := range []string{"borrow_ebook", "send_to_kindle", "set_kindle_email"} {
		_, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{"edition_id": "123"}})
		if err == nil {
			t.Fatalf("write %s accepted", name)
		}
	}
	if len(f.calls) != 0 {
		t.Fatal("read-only called upstream write")
	}
}

func TestToolDispatchAndValidation(t *testing.T) {
	f := &fakeAPI{}
	cs := session(t, f, false)
	for _, name := range []string{"get_ebook", "borrow_ebook", "send_to_kindle", "get_epub_link"} {
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{"edition_id": "123"}})
		if err != nil || res.IsError {
			t.Fatalf("%s failed: %v", name, err)
		}
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "search_ebooks", Arguments: map[string]any{"query": "synthetic book", "page": 0}})
	if err != nil || res.IsError {
		t.Fatalf("search failed: %v", err)
	}
	if strings.Join(f.calls, ",") != "book,borrow,kindle,epub,search" {
		t.Fatal(f.calls)
	}
	for _, args := range []map[string]any{{"query": " "}, {"query": "book", "page": -1}, {"query": "book", "page": 10001}} {
		res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "search_ebooks", Arguments: args})
		if err != nil || !res.IsError {
			t.Fatalf("invalid search accepted: %v", err)
		}
	}
	if len(f.calls) != 5 {
		t.Fatal("invalid input reached upstream")
	}
}

func TestErrorsDoNotLeakUpstreamDetails(t *testing.T) {
	f := &fakeAPI{err: errors.New("private-token private-card private-pin")}
	cs := session(t, f, false)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_to_kindle", Arguments: map[string]any{"edition_id": "123"}})
	if err != nil || !res.IsError {
		t.Fatalf("expected tool error: %v", err)
	}
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), "private-") {
		t.Fatal("upstream error leaked")
	}
}

func TestHTTPAuthentication(t *testing.T) {
	h := newHTTPHandler(newServer(&fakeAPI{}, true), "synthetic-token")
	for _, token := range []string{"", "Bearer wrong"} {
		r := httptest.NewRequest("POST", "/mcp", nil)
		r.Header.Set("Authorization", token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatal(w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	called := false
	secured := requireToken("synthetic-token", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	r := httptest.NewRequest("POST", "/mcp", nil)
	r.Header.Set("Authorization", "Bearer synthetic-token")
	secured.ServeHTTP(httptest.NewRecorder(), r)
	if !called {
		t.Fatal("valid token denied")
	}
	called = false
	requireToken("", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).ServeHTTP(httptest.NewRecorder(), r)
	if called {
		t.Fatal("empty token bypassed authentication")
	}
}

func TestTraceNamesRejectUntrustedInput(t *testing.T) {
	if safeTool("private-token") != "unknown" || safeMethod("private-card") != "unknown" {
		t.Fatal("untrusted trace name")
	}
	if safeTool("search_ebooks") != "search_ebooks" || safeMethod("tools/call") != "tools/call" {
		t.Fatal("known trace names lost")
	}
}

func TestHTTPInitializeIsStateless(t *testing.T) {
	h := newHTTPHandler(newServer(&fakeAPI{}, true), "synthetic-token")
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"synthetic","version":"1"}}}`))
	r.Header.Set("Authorization", "Bearer synthetic-token")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("initialize status %d", w.Code)
	}
	if w.Header().Get("Mcp-Session-Id") != "" {
		t.Fatal("stateless server issued a session ID")
	}
	if !strings.Contains(w.Body.String(), `"serverInfo"`) {
		t.Fatal("missing initialize result")
	}
}

func TestSpansNeverContainPrivateInputsOrErrors(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()); otel.SetTracerProvider(previous) })
	cs := session(t, &fakeAPI{err: errors.New("private-upstream-error")}, false)
	_, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "search_ebooks", Arguments: map[string]any{"query": "private-query"}})
	_, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "private-tool", Arguments: map[string]any{"query": "private-query"}})
	// Also exercise unrecognized methods through the middleware, independent of
	// whether the SDK chooses to reject those before invoking middleware.
	_, _ = traceMCP(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return nil, errors.New("private-error")
	})(context.Background(), "private-method", nil)
	h := newHTTPHandler(newServer(&fakeAPI{}, true), "private-auth-token")
	r := httptest.NewRequest("POST", "/mcp?private-query", strings.NewReader("private-body"))
	r.Header.Set("Authorization", "Bearer private-auth-token")
	r.Header.Set("Baggage", "secret=private-baggage")
	h.ServeHTTP(httptest.NewRecorder(), r)
	spans := exporter.GetSpans()
	if len(spans) == 0 {
		t.Fatal("no spans")
	}
	foundTool, foundHTTP := false, false
	for _, s := range spans {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "private-") {
			t.Fatalf("private data in span %q", s.Name)
		}
		if s.Name == "tools/call search_ebooks" {
			foundTool = true
		}
		if s.Name == "POST /mcp" {
			foundHTTP = true
		}
	}
	if !foundTool || !foundHTTP {
		t.Fatalf("missing tool or HTTP span: %v %v", foundTool, foundHTTP)
	}
}

func TestSetKindleEmailRejectsArbitraryRecipients(t *testing.T) {
	f := &fakeAPI{}
	cs := session(t, f, false)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "set_kindle_email", Arguments: map[string]any{"kindle_email": "someone@example.com"}})
	if err != nil || !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "kindle.com") {
		t.Fatalf("arbitrary recipient accepted: %v", err)
	}
	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_kindle_email", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("get_kindle_email failed: %v", err)
	}
	if strings.Join(f.calls, ",") != "kindle_email" {
		t.Fatal(f.calls)
	}
}
