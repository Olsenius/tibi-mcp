package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var errUpstream = errors.New("Tibi request failed; check account and availability")
var errUncertain = errors.New("Tibi action failed or outcome is unknown; inspect the book before retrying")
var editionID = regexp.MustCompile(`^[0-9]{1,20}$`)

type client struct {
	mu              sync.Mutex
	http            *http.Client
	base, card, pin string
	downloadHost    string
}

func newClient(card, pin string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{http: &http.Client{Jar: jar, Timeout: 30 * time.Second}, base: "https://tibi.no", card: card, pin: pin, downloadHost: "dod.nlb.no"}
}

// Only fixed route names and response status are recorded. No URLs, queries,
// bodies, upstream error text, cookies or account data enter spans.
func (c *client) json(ctx context.Context, method, route string, q url.Values, out any) error {
	ctx, span := otel.Tracer("tibi-mcp").Start(ctx, method+" "+route, trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/boker"+route+"?"+q.Encode(), nil)
	if err != nil {
		return errUpstream
	}
	req.Header.Set("Accept", "application/json")
	hc := *c.http
	if route == "/patron/SendToKindle" {
		// Transport may replay GET on a stale reused connection. A fresh
		// transport ensures this side-effecting GET never uses such a connection.
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.DisableKeepAlives = true
		hc.Transport = transport
		defer transport.CloseIdleConnections()
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return errUpstream
	}
	defer resp.Body.Close()
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	if resp.StatusCode != 200 {
		return errUpstream
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20+1))
	if err != nil || len(b) > 4<<20 {
		return errUpstream
	}
	if json.Unmarshal(b, out) != nil {
		return errUpstream
	}
	return nil
}

type format struct {
	Type int `json:"type"`
}
type edition struct {
	ID         string `json:"editionId"`
	Format     format `json:"format"`
	Available  bool   `json:"isAvailable"`
	Copies     bool   `json:"isCopiesAvailableForLoan"`
	LoanButton bool   `json:"showLoanBookButton"`
	Kindle     bool   `json:"showKindleButton"`
	EPUB       bool   `json:"showDownloadEpubButton"`
	Shelf      struct {
		On bool `json:"isOnPatronShelf"`
	} `json:"editionOnPatronShelf"`
}
type bookPage struct {
	Authenticated bool `json:"isAuthenticated"`
	Book          struct {
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Languages   []string `json:"languages"`
	} `json:"book"`
	Editions []edition `json:"editions"`
}
type ebook struct {
	EditionID     string `json:"edition_id"`
	Available     bool   `json:"available"`
	Borrowed      bool   `json:"borrowed"`
	CanBorrow     bool   `json:"can_borrow"`
	CanSendKindle bool   `json:"can_send_to_kindle"`
}
type bookResult struct {
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	Languages     []string `json:"languages"`
	Authenticated bool     `json:"authenticated"`
	Ebooks        []ebook  `json:"ebooks"`
}

func (p bookPage) result() bookResult {
	r := bookResult{Title: p.Book.Title, Description: p.Book.Description, Languages: p.Book.Languages, Authenticated: p.Authenticated, Ebooks: []ebook{}}
	for _, e := range p.Editions {
		if e.Format.Type == 4 || e.Format.Type == 6 {
			r.Ebooks = append(r.Ebooks, ebook{e.ID, e.Available, e.Shelf.On, e.Available && e.Copies && e.LoanButton && !e.Shelf.On, e.Kindle && e.Shelf.On})
		}
	}
	return r
}
func (c *client) page(ctx context.Context, id string, requireAuth bool) (bookPage, error) {
	var p bookPage
	if !editionID.MatchString(id) {
		return p, errors.New("invalid edition ID")
	}
	q := url.Values{"editionId": {id}}
	if err := c.json(ctx, "GET", "/book/GetBookPageByEdition", q, &p); err != nil {
		return p, err
	}
	if !p.Authenticated && (requireAuth || c.card != "" && c.pin != "") {
		if c.card == "" || c.pin == "" {
			return p, errors.New("configure TIBI_CARD_NUMBER and TIBI_PIN")
		}
		if err := authenticate(ctx, c.http, c.base, c.card, c.pin); err != nil {
			return p, errUpstream
		}
		p = bookPage{}
		if err := c.json(ctx, "GET", "/book/GetBookPageByEdition", q, &p); err != nil {
			return p, err
		}
		if !p.Authenticated {
			return p, errUpstream
		}
	}
	return p, nil
}
func (c *client) Book(ctx context.Context, id string) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, err := c.page(ctx, id, false)
	if err != nil {
		return nil, err
	}
	return p.result(), nil
}

type searchCard struct {
	Title    string   `json:"title"`
	BookURL  string   `json:"bookUrl"`
	Language string   `json:"language"`
	Year     string   `json:"publishedYear"`
	Formats  []format `json:"formatTags"`
}
type searchHit struct {
	Title            string `json:"title"`
	CatalogEditionID string `json:"catalog_edition_id"`
	Language         string `json:"language"`
	Year             string `json:"year"`
}
type paging struct {
	Start int `json:"start"`
	Limit int `json:"limit"`
	Total int `json:"total"`
}

func (c *client) Search(ctx context.Context, query string, page int) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(query) == "" || len(query) > 500 || page < 1 || page > 10000 {
		return nil, errors.New("invalid query or page")
	}
	var raw struct {
		Cards  []searchCard `json:"presentationCards"`
		Paging paging       `json:"paging"`
	}
	err := c.json(ctx, "GET", "/search/search", url.Values{"query": {query}, "Format": {"EBook"}, "page": {strconv.Itoa(page)}}, &raw)
	if err != nil {
		return nil, err
	}
	hits := []searchHit{}
	for _, x := range raw.Cards {
		ok := false
		for _, f := range x.Formats {
			if f.Type == 4 || f.Type == 6 {
				ok = true
			}
		}
		if !ok {
			continue
		}
		u, e := url.Parse(x.BookURL)
		if e != nil {
			continue
		}
		id := u.Query().Get("utgave")
		if !editionID.MatchString(id) {
			continue
		}
		hits = append(hits, searchHit{x.Title, id, x.Language, x.Year})
	}
	return struct {
		Books  []searchHit `json:"books"`
		Paging paging      `json:"paging"`
		Note   string      `json:"note"`
	}{hits, raw.Paging, "Use get_ebook with catalog_edition_id to find the e-book edition_id before borrowing."}, nil
}
func (c *client) action(ctx context.Context, id string, kindle bool) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, err := c.page(ctx, id, true)
	if err != nil {
		return nil, err
	}
	var selected *edition
	for i := range p.Editions {
		e := &p.Editions[i]
		if e.ID == id && (e.Format.Type == 4 || e.Format.Type == 6) {
			selected = e
			break
		}
	}
	if selected == nil {
		return nil, errors.New("select an e-book edition_id from get_ebook")
	}
	e := selected
	if !kindle && e.Shelf.On {
		return map[string]any{"status": "already_borrowed", "edition_id": id}, nil
	}
	if kindle && (!e.Shelf.On || !e.Kindle) {
		return nil, errors.New("Kindle unavailable: borrow first, configure Kindle in Tibi profile and check size limit")
	}
	if !kindle && (!e.Available || !e.Copies || !e.LoanButton) {
		return nil, errors.New("e-book is not available to borrow")
	}
	route, method, status := "/patron/addBookToShelf", "POST", "borrowed"
	if kindle {
		route, method, status = "/patron/SendToKindle", "GET", "send_requested"
	}
	var accepted bool
	// Never retry mutations, including the misleading GET used by SendToKindle.
	if c.json(ctx, method, route, url.Values{"editionId": {id}}, &accepted) != nil || !accepted {
		return nil, errUncertain
	}
	return map[string]any{"status": status, "edition_id": id}, nil
}

// downloadFormatEPUB is DownloadFormat.Epub in Tibi's public client.
const downloadFormatEPUB = "2"

// EPUBLink returns Tibi's direct EPUB link for a borrowed e-book. The link works
// without cookies and contains the borrower number, so it is only returned to
// the caller and never logged or put in spans.
func (c *client) EPUBLink(ctx context.Context, id string) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, err := c.page(ctx, id, true)
	if err != nil {
		return nil, err
	}
	var selected *edition
	for i := range p.Editions {
		if e := &p.Editions[i]; e.ID == id && (e.Format.Type == 4 || e.Format.Type == 6) {
			selected = e
			break
		}
	}
	if selected == nil {
		return nil, errors.New("select an e-book edition_id from get_ebook")
	}
	if !selected.Shelf.On || !selected.EPUB {
		return nil, errors.New("EPUB unavailable: borrow the e-book first")
	}
	var link string
	if err := c.json(ctx, "GET", "/patron/getDownloadUrl", url.Values{"editionId": {id}, "downloadFormat": {downloadFormatEPUB}}, &link); err != nil {
		return nil, err
	}
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || u.Host != c.downloadHost || u.User != nil || !strings.HasSuffix(strings.ToLower(u.Path), ".epub") {
		return nil, errUpstream
	}
	result := map[string]any{
		"edition_id":   id,
		"download_url": u.String(),
		"filename":     id + ".epub",
		"mime_type":    "application/epub+zip",
		"note":         "The link needs no login, contains the borrower number and grants access to the file. Pass it only to the service that should fetch the book.",
	}
	if size := c.contentLength(ctx, u.String()); size > 0 {
		result["size_bytes"] = size
	}
	return result, nil
}

// contentLength asks for the file size without cookies or redirects: first a
// HEAD, then a one-byte range request. A failure only omits the size.
func (c *client) contentLength(ctx context.Context, link string) int64 {
	ctx, span := otel.Tracer("tibi-mcp").Start(ctx, "epub size", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	hc := &http.Client{Transport: c.http.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		req, err := http.NewRequestWithContext(ctx, method, link, nil)
		if err != nil {
			return 0
		}
		if method == http.MethodGet {
			req.Header.Set("Range", "bytes=0-0")
		}
		resp, err := hc.Do(req)
		if err != nil {
			continue
		}
		resp.Body.Close()
		switch {
		case method == http.MethodHead && resp.StatusCode == http.StatusOK && resp.ContentLength > 0:
			return resp.ContentLength
		case resp.StatusCode == http.StatusPartialContent:
			// Content-Range: bytes 0-0/<total>
			cr := resp.Header.Get("Content-Range")
			if i := strings.LastIndex(cr, "/"); i >= 0 {
				if n, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil && n > 0 {
					return n
				}
			}
		}
	}
	return 0
}

func (c *client) Borrow(ctx context.Context, id string) (any, error) { return c.action(ctx, id, false) }
func (c *client) SendKindle(ctx context.Context, id string) (any, error) {
	return c.action(ctx, id, true)
}
