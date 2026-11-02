package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type tibiAPI interface {
	Search(context.Context, string, int) (any, error)
	Book(context.Context, string) (any, error)
	Borrow(context.Context, string) (any, error)
	SendKindle(context.Context, string) (any, error)
	EPUBLink(context.Context, string) (any, error)
	KindleEmail(context.Context) (any, error)
	SetKindleEmail(context.Context, string) (any, error)
}

type kindleEmailInput struct {
	KindleEmail string `json:"kindle_email" jsonschema:"Kindle or PocketBook address (@kindle.com, @free.kindle.com or @pbsync.com); empty string removes it"`
}

type searchInput struct {
	Query string `json:"query" jsonschema:"Search words for e-books"`
	Page  int    `json:"page,omitempty" jsonschema:"One-based page number; default one"`
}

type editionInput struct {
	EditionID string `json:"edition_id" jsonschema:"Catalog edition ID from search_ebooks, or an exact e-book edition_id from get_ebook; writes require an exact e-book edition_id"`
}

func toolError(message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: message}}}
}

func toolResult(value any, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		return toolError("Tibi request failed. Check account setup and book availability. For a write, check the book status before retrying; the outcome may be unknown."), nil, nil
	}
	b, err := json.Marshal(value)
	if err != nil {
		return toolError("Could not encode the Tibi result."), nil, nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
}

func newServer(api tibiAPI, readOnly bool) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "tibi", Title: "Tibi e-books", Version: version}, &mcp.ServerOptions{Instructions: "Search Tibi e-books, inspect an edition, borrow it, send it to the Kindle address already configured on the Tibi profile, get an EPUB download link for a borrowed book, or show and change the Kindle address on the profile. Only borrow or send books requested by the user. Kindle delivery requires profile setup and Tibi's file-size eligibility. Do not retry an uncertain write automatically."})
	s.AddReceivingMiddleware(traceMCP)
	yes, no := true, false
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &yes}
	write := &mcp.ToolAnnotations{DestructiveHint: &no, OpenWorldHint: &yes}
	mcp.AddTool(s, &mcp.Tool{Name: "search_ebooks", Description: "Search the Tibi catalog restricted to e-books.", Annotations: ro}, func(ctx context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, any, error) {
		in.Query = strings.TrimSpace(in.Query)
		if in.Page == 0 {
			in.Page = 1
		}
		if in.Query == "" || len(in.Query) > 500 || in.Page < 1 || in.Page > 10000 {
			return toolError("Provide query (1–500 bytes) and page (1–10000)."), nil, nil
		}
		v, err := api.Search(ctx, in.Query, in.Page)
		return toolResult(v, err)
	})
	addEdition := func(name, description string, annotations *mcp.ToolAnnotations, fn func(context.Context, string) (any, error)) {
		mcp.AddTool(s, &mcp.Tool{Name: name, Description: description, Annotations: annotations}, func(ctx context.Context, _ *mcp.CallToolRequest, in editionInput) (*mcp.CallToolResult, any, error) {
			if strings.TrimSpace(in.EditionID) == "" || len(in.EditionID) > 100 {
				return toolError("Provide edition_id from a Tibi e-book result."), nil, nil
			}
			v, err := fn(ctx, in.EditionID)
			return toolResult(v, err)
		})
	}
	addEdition("get_ebook", "Resolve a catalog_edition_id or edition_id to e-book editions and their borrowing and Kindle eligibility. Pass it as edition_id.", ro, api.Book)
	addEdition("get_epub_link", "Get a direct EPUB download link for an already borrowed e-book, so another tool can fetch the file. The link needs no login and contains the borrower number: pass it only to the service that should fetch the book, and do not show it unless asked.", ro, api.EPUBLink)
	mcp.AddTool(s, &mcp.Tool{Name: "get_kindle_email", Description: "Show the Kindle or PocketBook address stored on the Tibi profile, used by send_to_kindle.", Annotations: ro}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		v, err := api.KindleEmail(ctx)
		return toolResult(v, err)
	})
	if !readOnly {
		mcp.AddTool(s, &mcp.Tool{Name: "set_kindle_email", Description: "Change only the Kindle or PocketBook address on the Tibi profile. Other profile fields are kept. Remind the user to approve TL-administrator@nb.no as a sender at Amazon. Do not retry an uncertain result automatically.", Annotations: &mcp.ToolAnnotations{DestructiveHint: &no, IdempotentHint: true, OpenWorldHint: &yes}}, func(ctx context.Context, _ *mcp.CallToolRequest, in kindleEmailInput) (*mcp.CallToolResult, any, error) {
			if _, err := normalizeDeliveryEmail(in.KindleEmail); err != nil {
				return toolError(err.Error()), nil, nil
			}
			v, err := api.SetKindleEmail(ctx, in.KindleEmail)
			return toolResult(v, err)
		})
		addEdition("borrow_ebook", "Borrow the requested e-book edition. Does not send to Kindle.", write, api.Borrow)
		addEdition("send_to_kindle", "Send an already borrowed, eligible e-book to the Kindle address configured on the Tibi profile. Do not retry an uncertain result automatically.", write, api.SendKindle)
	}
	return s
}

func requireToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="tibi-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func newHTTPHandler(s *mcp.Server, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("/mcp", requireToken(token, mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true})))
	return traceHTTP(mux)
}
