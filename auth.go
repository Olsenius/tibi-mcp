package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel"
	"golang.org/x/net/html"
)

var errAuthentication = errors.New("Tibi authentication failed; check account settings or sign in on tibi.no")

type authState struct {
	Component string `json:"component"`
	CSRFToken string `json:"csrfToken"`
	Props     struct {
		ID             string `json:"id"`
		Authenticators []struct {
			ID string `json:"id"`
		} `json:"authenticators"`
		Inputs []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"inputs"`
		UseRecaptcha bool              `json:"useRecaptcha"`
		Errors       []json.RawMessage `json:"errors"`
		URL          string            `json:"url"`
		Form         map[string]string `json:"form"`
	} `json:"props"`
}

// authenticate drives the same Selector/DynamicInputs flow as auth.nlb.no's
// public web client. OIDC state, correlation cookies and CSRF values are obtained
// afresh. They are never persisted, logged or returned to MCP clients.
func authenticate(ctx context.Context, hc *http.Client, baseURL, card, pin string) error {
	if card == "" || pin == "" || hc == nil || hc.Jar == nil {
		return errAuthentication
	}
	base, err := url.Parse(baseURL)
	if err != nil || base.Host == "" || base.User != nil || (base.Scheme != "https" && base.Scheme != "http") {
		return errAuthentication
	}
	client := *hc
	previousRedirect := hc.CheckRedirect
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !authDestination(base, r.URL) {
			return errAuthentication
		}
		// A redirect must never forward a PIN or authorization-code POST body.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			return errAuthentication
		}
		// These headers describe a single auth form submission, not a redirect.
		r.Header.Del("X-Csrf-Token")
		r.Header.Del("Content-Type")
		if previousRedirect != nil {
			return previousRedirect(r, via)
		}
		return nil
	}
	target := base.ResolveReference(&url.URL{Path: "/boker/Auth/Login", RawQuery: "returnUrl=%2Fboker%2F"})
	body, current, err := authRequest(ctx, &client, target, nil, "", false)
	if err != nil {
		return errAuthentication
	}
	var csrf string
	selected, submitted := false, false
	for step := 0; step < 8; step++ {
		state, err := parseAuthState(body)
		if err != nil {
			return errAuthentication
		}
		if state.CSRFToken != "" {
			csrf = state.CSRFToken
		}
		if len(state.Props.Errors) > 0 {
			return errAuthentication
		}
		var values url.Values
		jsonResponse := true
		switch state.Component {
		case "Selector":
			if selected || submitted || csrf == "" || !isAuthOrigin(base, current) {
				return errAuthentication
			}
			found := false
			for _, a := range state.Props.Authenticators {
				if a.ID == "Lnr_PIN" {
					found = true
				}
			}
			if !found || state.Props.ID == "" {
				return errAuthentication
			}
			values = url.Values{"component": {"Selector"}, "selectorId": {state.Props.ID}, "selectedAuthenticatorId": {"Lnr_PIN"}}
			selected = true
			target = authPath(current)
		case "DynamicInputs":
			if submitted || csrf == "" || state.Props.UseRecaptcha || !isAuthOrigin(base, current) {
				return errAuthentication
			}
			user, pass := false, false
			for _, input := range state.Props.Inputs {
				switch {
				case input.Name == "username" && input.Type == "Text":
					user = true
				case input.Name == "password" && input.Type == "Password":
					pass = true
				default:
					return errAuthentication
				}
			}
			if !user || !pass {
				return errAuthentication
			}
			values = url.Values{"component": {"DynamicInputs"}, "username": {card}, "password": {pin}, "action": {"submit"}}
			submitted = true
			target = authPath(current)
		case "Post":
			if !submitted {
				return errAuthentication
			}
			target, err = current.Parse(state.Props.URL)
			if err != nil || !sameOrigin(base, target) || target.Path != "/boker/auth/callback" || target.RawQuery != "" {
				return errAuthentication
			}
			if state.Props.Form["code"] == "" || state.Props.Form["state"] == "" || state.Props.Form["error"] != "" {
				return errAuthentication
			}
			values = url.Values{}
			for k, v := range state.Props.Form {
				values.Set(k, v)
			}
			return completeCallback(ctx, &client, base, target, values)
		case "Redirect":
			if !submitted {
				return errAuthentication
			}
			target, err = current.Parse(state.Props.URL)
			if err != nil || !authDestination(base, target) {
				return errAuthentication
			}
			jsonResponse = false
		default:
			return errAuthentication
		}
		token := csrf
		if !jsonResponse {
			token = ""
		}
		body, current, err = authRequest(ctx, &client, target, values, token, jsonResponse)
		if err != nil {
			return errAuthentication
		}
	}
	return errAuthentication
}

// completeCallback posts the authorization code once and stops there. Tibi sets
// its session cookie on the callback response and then redirects to
// /boker/Auth/Login, which always starts a new sign-in, so the redirect is not
// followed. Callers confirm the session with an isAuthenticated book lookup.
func completeCallback(ctx context.Context, hc *http.Client, base, target *url.URL, values url.Values) error {
	client := *hc
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ctx, span := otel.Tracer("tibi-mcp").Start(ctx, "tibi authentication")
	defer span.End()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), strings.NewReader(values.Encode()))
	if err != nil {
		return errAuthentication
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return errAuthentication
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusFound, http.StatusSeeOther:
		next, err := resp.Location()
		if err != nil || !sameOrigin(base, next) {
			return errAuthentication
		}
		return nil
	}
	return errAuthentication
}

func sameOrigin(a, b *url.URL) bool {
	return a != nil && b != nil && b.User == nil && a.Scheme == b.Scheme && a.Host == b.Host
}
func isAuthOrigin(base, u *url.URL) bool {
	return u != nil && u.User == nil && ((u.Scheme == "https" && u.Host == "auth.nlb.no") || (base.Host != "tibi.no" && sameOrigin(base, u)))
}
func authDestination(base, u *url.URL) bool { return sameOrigin(base, u) || isAuthOrigin(base, u) }
func authPath(u *url.URL) *url.URL          { v := *u; v.RawQuery = ""; v.Fragment = ""; return &v }

func authRequest(ctx context.Context, client *http.Client, target *url.URL, values url.Values, csrf string, jsonResponse bool) ([]byte, *url.URL, error) {
	ctx, span := otel.Tracer("tibi-mcp").Start(ctx, "tibi authentication")
	defer span.End()
	method := http.MethodGet
	var reader io.Reader
	if values != nil {
		method = http.MethodPost
		reader = strings.NewReader(values.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return nil, nil, errAuthentication
	}
	if values != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if csrf != "" {
		req.Header.Set("X-Csrf-Token", csrf)
	}
	if jsonResponse {
		req.Header.Set("Accept", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, errAuthentication
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, errAuthentication
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
	if err != nil || len(body) > 2*1024*1024 {
		return nil, nil, errAuthentication
	}
	return body, resp.Request.URL, nil
}

func parseAuthState(body []byte) (authState, error) {
	var state authState
	if json.Unmarshal(body, &state) == nil && state.Component != "" {
		return state, nil
	}
	root, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return state, errAuthentication
	}
	var encoded string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				if a.Key == "data-initial-state" {
					encoded = a.Val
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	if encoded == "" || json.Unmarshal([]byte(encoded), &state) != nil || state.Component == "" {
		return authState{}, errAuthentication
	}
	return state, nil
}
