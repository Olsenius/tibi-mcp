package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func fakePage(auth, on, kindle bool, typ int) bookPage {
	p := bookPage{Authenticated: auth}
	p.Book.Title = "Synthetic book"
	e := edition{ID: "123", Format: format{Type: typ}, Available: true, Copies: true, LoanButton: true, Kindle: kindle}
	e.Shelf.On = on
	p.Editions = []edition{e}
	return p
}
func TestClientActions(t *testing.T) {
	for _, tc := range []struct {
		name             string
		on, kindle, send bool
		typ              int
		reply            string
		wantCalls        int
		wantError        bool
	}{
		{"borrow", false, false, false, 4, "true", 1, false},
		{"already borrowed", true, false, false, 4, "true", 0, false},
		{"audio rejected", false, false, false, 2, "true", 0, true},
		{"send", true, true, true, 4, "true", 1, false},
		{"unborrowed send rejected", false, true, true, 4, "true", 0, true},
		{"unavailable kindle", true, false, true, 4, "true", 0, true},
		{"upstream false", false, false, false, 4, "false", 1, true},
		{"unknown result no retry", true, true, true, 4, "private-upstream-error", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("editionId") != "123" {
					t.Error("wrong edition")
				}
				if r.URL.Path == "/boker/book/GetBookPageByEdition" {
					json.NewEncoder(w).Encode(fakePage(true, tc.on, tc.kindle, tc.typ))
					return
				}
				calls++
				wantPath, wantMethod := "/boker/patron/addBookToShelf", "POST"
				if tc.send {
					wantPath, wantMethod = "/boker/patron/SendToKindle", "GET"
				}
				if r.URL.Path != wantPath || r.Method != wantMethod {
					t.Error("wrong mutation route/method")
				}
				w.Write([]byte(tc.reply))
			}))
			defer srv.Close()
			c := newClient("", "")
			c.base = srv.URL
			_, err := c.action(context.Background(), "123", tc.send)
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v", err)
			}
			if err != nil && strings.Contains(err.Error(), "private-") {
				t.Fatal("private data in error")
			}
			if calls != tc.wantCalls {
				t.Fatalf("mutation calls %d, want %d", calls, tc.wantCalls)
			}
		})
	}
}
func TestSearchFiltersAndResolvesEbooks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") != "synthetic & book" || r.URL.Query().Get("Format") != "EBook" || r.URL.Query().Get("page") != "2" {
			t.Error("query encoding/filter")
		}
		w.Write([]byte(`{"presentationCards":[{"title":"Synthetic ebook","bookUrl":"/boker/bok?utgave=123","formatTags":[{"type":4}]},{"title":"Audio only","bookUrl":"/boker/bok?utgave=456","formatTags":[{"type":2}]}],"paging":{"start":2,"limit":20,"total":21},"private":"private-patron"}`))
	}))
	defer srv.Close()
	c := newClient("", "")
	c.base = srv.URL
	v, err := c.Search(context.Background(), "synthetic & book", 2)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), "private-") || strings.Contains(string(b), "Audio only") || !strings.Contains(string(b), `"catalog_edition_id":"123"`) {
		t.Fatalf("unexpected result: %s", b)
	}
}
func TestBookOutputAllowlist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"isAuthenticated":false,"book":{"title":"Synthetic"},"editions":[{"editionId":"123","format":{"type":4},"editionLinks":{"webPlayerUrl":"private-token"}},{"editionId":"456","format":{"type":2}}],"patron":"private-patron"}`))
	}))
	defer srv.Close()
	c := newClient("", "")
	c.base = srv.URL
	v, err := c.Book(context.Background(), "123")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), "private-") || strings.Contains(string(b), "456") {
		t.Fatal("unexpected book metadata")
	}
}
func TestMutationRedirectIsNotFollowed(t *testing.T) {
	leaked := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked++ }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/boker/book/GetBookPageByEdition" {
			json.NewEncoder(w).Encode(fakePage(true, true, true, 4))
			return
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c := newClient("", "")
	c.base = srv.URL
	if _, err := c.SendKindle(context.Background(), "123"); err == nil {
		t.Fatal("accepted redirect")
	}
	if leaked != 0 {
		t.Fatal("followed mutation redirect")
	}
}

func TestKindleConnectionFailureIsNotReplayed(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/boker/book/GetBookPageByEdition" {
			json.NewEncoder(w).Encode(fakePage(true, true, true, 4))
			return
		}
		if calls.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		w.Write([]byte("true"))
	}))
	defer srv.Close()
	c := newClient("", "")
	c.base = srv.URL
	_, err := c.SendKindle(context.Background(), "123")
	if err == nil || calls.Load() != 1 {
		t.Fatalf("uncertain write replayed: calls=%d error=%v", calls.Load(), err)
	}
}

func TestEPUBLink(t *testing.T) {
	for _, tc := range []struct {
		name          string
		on, epub      bool
		link          string
		wantLinkCalls int
		wantError     bool
	}{
		{"borrowed", true, true, "", 1, false},
		{"not borrowed", false, true, "", 0, true},
		{"no epub button", true, false, "", 0, true},
		{"audio rejected", true, true, "", 0, true},
		{"untrusted host", true, true, "https://example.invalid/private-card/123.epub", 1, true},
		{"plain http", true, true, "http://HOST/content/private-card/123.epub", 1, true},
		{"not epub", true, true, "https://HOST/content/private-card/123.html", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			linkCalls, heads := 0, 0
			var srv *httptest.Server
			srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/boker/book/GetBookPageByEdition":
					p := fakePage(true, tc.on, false, 4)
					if tc.name == "audio rejected" {
						p = fakePage(true, tc.on, false, 2)
					}
					p.Editions[0].EPUB = tc.epub
					json.NewEncoder(w).Encode(p)
				case "/boker/patron/getDownloadUrl":
					linkCalls++
					if r.URL.Query().Get("editionId") != "123" || r.URL.Query().Get("downloadFormat") != "2" || r.Method != "GET" {
						t.Error("wrong download request")
					}
					link := tc.link
					if link == "" {
						link = srv.URL + "/content/private-card/123/x/123.epub"
					}
					json.NewEncoder(w).Encode(strings.ReplaceAll(link, "HOST", strings.TrimPrefix(srv.URL, "https://")))
				case "/content/private-card/123/x/123.epub":
					heads++
					if len(r.Cookies()) != 0 {
						t.Error("size check must not send cookies")
					}
					if r.Method == http.MethodHead {
						// Like dod.nlb.no, give no usable HEAD answer.
						w.WriteHeader(http.StatusMethodNotAllowed)
						return
					}
					if r.Header.Get("Range") != "bytes=0-0" {
						t.Error("size fallback must request one byte")
					}
					w.Header().Set("Content-Range", "bytes 0-0/825157")
					w.WriteHeader(http.StatusPartialContent)
					w.Write([]byte("P"))
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			c := newClient("", "")
			c.base = srv.URL
			c.http.Transport = srv.Client().Transport
			c.downloadHost = strings.TrimPrefix(srv.URL, "https://")
			v, err := c.EPUBLink(context.Background(), "123")
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v", err)
			}
			if linkCalls != tc.wantLinkCalls {
				t.Fatalf("link calls %d, want %d", linkCalls, tc.wantLinkCalls)
			}
			if err != nil {
				if strings.Contains(err.Error(), "private-") {
					t.Fatal("private data in error")
				}
				return
			}
			m := v.(map[string]any)
			if m["download_url"] != srv.URL+"/content/private-card/123/x/123.epub" || m["filename"] != "123.epub" || m["mime_type"] != "application/epub+zip" || m["size_bytes"] != int64(825157) || heads != 2 {
				t.Fatalf("unexpected result %v heads=%d", m, heads)
			}
		})
	}
}

func TestKindleEmail(t *testing.T) {
	for _, tc := range []struct {
		name, current, set string
		success, persist   bool
		wantPosts          int
		wantStatus         string
		wantError          bool
	}{
		{"update", "old@kindle.com", "New@Kindle.com", true, true, 1, "updated", false},
		{"clear", "old@kindle.com", "", true, true, 1, "updated", false},
		{"pocketbook", "", "reader@pbsync.com", true, true, 1, "updated", false},
		{"unchanged", "same@kindle.com", "same@kindle.com", true, true, 0, "unchanged", false},
		{"arbitrary domain refused", "old@kindle.com", "someone@example.com", true, true, 0, "", true},
		{"two addresses refused", "old@kindle.com", "a@kindle.com,b@kindle.com", true, true, 0, "", true},
		{"upstream refused", "old@kindle.com", "new@kindle.com", false, false, 1, "", true},
		{"not persisted", "old@kindle.com", "new@kindle.com", true, false, 1, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored, posts := tc.current, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/boker/book/GetBookPageByEdition":
					json.NewEncoder(w).Encode(fakePage(true, false, false, 4))
				case "/boker/patron/GetPatronData":
					json.NewEncoder(w).Encode(map[string]any{"phoneNumber": "", "email": "private-mail@example.com", "kindleEmail": stored, "addressLine1": "private-address", "firstName": "private-name"})
				case "/boker/patron/UpdatePatronProfile":
					posts++
					var body map[string]any
					if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&body) != nil {
						t.Error("profile update must be a JSON POST")
					}
					if len(body) != 3 || body["phoneNumber"] != "" || body["email"] != "private-mail@example.com" {
						t.Errorf("other profile fields must be sent back unchanged: %v", body)
					}
					if tc.persist {
						stored, _ = body["kindleEmail"].(string)
					}
					json.NewEncoder(w).Encode(map[string]any{"success": tc.success, "friendlyErrorMessage": "private-upstream-error"})
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			c := newClient("", "")
			c.base = srv.URL
			v, err := c.SetKindleEmail(context.Background(), tc.set)
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v", err)
			}
			if posts != tc.wantPosts {
				t.Fatalf("posts %d, want %d", posts, tc.wantPosts)
			}
			if err != nil {
				if strings.Contains(err.Error(), "private-") {
					t.Fatal("private data in error")
				}
				return
			}
			want := strings.ToLower(tc.set[strings.LastIndex(tc.set, "@")+1:])
			if m := v.(map[string]any); m["status"] != tc.wantStatus || !strings.HasSuffix(m["kindle_email"].(string), want) {
				t.Fatalf("unexpected result %v", m)
			}
			got, err := c.KindleEmail(context.Background())
			if err != nil || got.(map[string]any)["kindle_email"] != stored {
				t.Fatalf("get_kindle_email=%v err=%v", got, err)
			}
			if b, _ := json.Marshal(got); strings.Contains(string(b), "private-") {
				t.Fatal("other profile fields leaked")
			}
		})
	}
}
