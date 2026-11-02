package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAuthenticateFlow(t *testing.T) {
	for _, failure := range []string{"", "credentials", "captcha", "untrusted_callback", "redirect_post", "unexpected_input", "callback_offsite", "callback_rejected"} {
		t.Run(failure, func(t *testing.T) {
			posts, callbacks, logins := 0, 0, 0
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/boker/Auth/Login":
					// Like Tibi, this endpoint always starts a new sign-in.
					logins++
					http.SetCookie(w, &http.Cookie{Name: "correlation", Value: "private-correlation", Path: "/"})
					http.Redirect(w, r, "/authentication/oidc/tibi/login?state=private-state", http.StatusFound)
				case "/authentication/oidc/tibi/login":
					if r.Method == http.MethodGet {
						s := `{"component":"Selector","csrfToken":"private-csrf","props":{"id":"private-selector","authenticators":[{"id":"Lnr_PIN"}]}}`
						fmt.Fprintf(w, `<div data-initial-state="%s"></div>`, html.EscapeString(s))
						return
					}
					if r.URL.RawQuery != "" || r.Header.Get("X-Csrf-Token") != "private-csrf" || r.Header.Get("Accept") != "application/json" {
						t.Error("invalid auth headers or query")
					}
					if _, err := r.Cookie("correlation"); err != nil {
						t.Error("missing cookie")
					}
					r.ParseForm()
					w.Header().Set("Content-Type", "application/json")
					switch r.Form.Get("component") {
					case "Selector":
						if r.Form.Get("selectorId") != "private-selector" || r.Form.Get("selectedAuthenticatorId") != "Lnr_PIN" {
							t.Error("bad selector")
						}
						inputs := []map[string]string{{"name": "username", "type": "Text"}, {"name": "password", "type": "Password"}}
						if failure == "unexpected_input" {
							inputs = append(inputs, map[string]string{"name": "otp", "type": "Text"})
						}
						json.NewEncoder(w).Encode(map[string]any{"component": "DynamicInputs", "props": map[string]any{"inputs": inputs, "useRecaptcha": failure == "captcha"}})
					case "DynamicInputs":
						posts++
						if r.Form.Get("username") != "private-card" || r.Form.Get("password") != "private-pin" || r.Form.Get("action") != "submit" {
							t.Error("bad credentials payload")
						}
						if failure == "credentials" {
							fmt.Fprint(w, `{"component":"DynamicInputs","props":{"errors":["private-error"]}}`)
							return
						}
						if failure == "redirect_post" {
							w.Header().Set("Location", server.URL+"/leak")
							w.WriteHeader(http.StatusTemporaryRedirect)
							return
						}
						callback := server.URL + "/boker/auth/callback"
						if failure == "untrusted_callback" {
							callback = "https://example.invalid/steal"
						}
						json.NewEncoder(w).Encode(map[string]any{"component": "Post", "props": map[string]any{"url": callback, "form": map[string]string{"code": "private-code", "state": "private-state"}}})
					default:
						t.Error("unexpected component")
					}
				case "/boker/auth/callback":
					callbacks++
					r.ParseForm()
					if r.Form.Get("code") != "private-code" || r.Form.Get("state") != "private-state" || r.Header.Get("X-Csrf-Token") != "" {
						t.Error("bad callback")
					}
					switch failure {
					case "callback_offsite":
						http.Redirect(w, r, "https://example.invalid/private-secret", http.StatusFound)
					case "callback_rejected":
						w.WriteHeader(http.StatusBadRequest)
					default:
						http.SetCookie(w, &http.Cookie{Name: "session", Value: "private-session", Path: "/"})
						http.Redirect(w, r, "/boker/Auth/Login?returnUrl=%2Fboker%2F", http.StatusFound)
					}
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			client := server.Client()
			client.Jar, _ = cookiejar.New(nil)
			err := authenticate(context.Background(), client, server.URL, "private-card", "private-pin")
			if failure == "" {
				if err != nil || posts != 1 || callbacks != 1 || logins != 1 {
					t.Fatalf("flow failed: err=%v posts=%d callbacks=%d logins=%d", err, posts, callbacks, logins)
				}
				u, _ := url.Parse(server.URL + "/boker/")
				if len(client.Jar.Cookies(u)) == 0 {
					t.Fatal("missing session cookie")
				}
			} else {
				if err == nil || strings.Contains(err.Error(), "private-") {
					t.Fatal("expected sanitized failure")
				}
				if callbacks != map[bool]int{true: 1, false: 0}[strings.HasPrefix(failure, "callback_")] {
					t.Fatal("unexpected callback count")
				}
				if (failure == "captcha" || failure == "unexpected_input") && posts != 0 {
					t.Fatal("must not submit credentials")
				}
			}
		})
	}
}

func TestAuthenticationRejectsMissingCredentialsAndUntrustedRedirect(t *testing.T) {
	client := &http.Client{}
	if authenticate(context.Background(), client, "https://tibi.no", "", "") == nil {
		t.Fatal("missing credentials accepted")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.invalid/private-secret", http.StatusFound)
	}))
	defer server.Close()
	client = server.Client()
	client.Jar, _ = cookiejar.New(nil)
	err := authenticate(context.Background(), client, server.URL, "private-card", "private-pin")
	if err == nil || strings.Contains(err.Error(), "private-") {
		t.Fatal("unsafe redirect error")
	}
}
