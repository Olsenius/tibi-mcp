package main

// The gateway contract (github.com/Olsenius/mcp-gateway): endpoints a gateway
// uses to show a settings form and a Test button for this server.
//
//   GET  /healthz            liveness, no auth
//   GET  /healthz?check=1    logs in to Tibi and checks the Kindle address
//   GET  /config             the settings: schema, values (never secrets), lock state
//   POST /config             {"values": {"ENV_NAME": "value"}, "clear": ["ENV_NAME"]}
//
// All but plain /healthz need "Authorization: Bearer <CONFIG_TOKEN>", which
// defaults to MCP_AUTH_TOKEN.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type configField struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Type     string `json:"type"` // text, url, secret, bool, select
	Required bool   `json:"required,omitempty"`
	Help     string `json:"help,omitempty"`
	Value    string `json:"value"`
	Set      bool   `json:"set"`
	Locked   bool   `json:"locked"` // set by an environment variable
	Default  string `json:"default,omitempty"`
}

type configDoc struct {
	Name       string        `json:"name"`
	Version    string        `json:"version"`
	Configured bool          `json:"configured"`
	Fields     []configField `json:"fields"`
}

type checkResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type healthDoc struct {
	OK     bool          `json:"ok"`
	Status string        `json:"status"`
	Checks []checkResult `json:"checks,omitempty"`
}

type configUpdate struct {
	Values map[string]string `json:"values"`
	Clear  []string          `json:"clear"`
}

var fieldDefs = []struct {
	key, label, typ, help, def string
	required                   bool
}{
	{"card_number", "Borrower number", "text", "The Tibi borrower number (lånenummer) you log in with.", "", true},
	{"pin", "PIN", "secret", "Stored on the server and never shown again.", "", true},
	{"read_only", "Read-only", "bool", "Hides the tools that borrow, send to Kindle and change the Kindle address. On until you turn it off.", "true", false},
}

func keyForEnv(env string) string {
	for k, e := range envNames {
		if strings.EqualFold(e, env) {
			return k
		}
	}
	return ""
}

func setSetting(s *Settings, key, v string) error {
	v = strings.TrimSpace(v)
	switch key {
	case "card_number":
		if v != "" && !editionID.MatchString(v) {
			return fmt.Errorf("%s must be digits", envNames[key])
		}
		s.CardNumber = v
	case "pin":
		s.PIN = v
	case "read_only":
		if v == "" {
			s.ReadOnly = nil
			return nil
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%s must be true or false", envNames[key])
		}
		s.ReadOnly = &b
	default:
		return fmt.Errorf("unknown setting %q", key)
	}
	return nil
}

func (a *app) configDoc() configDoc {
	s := a.current()
	stored := a.store.settings()
	doc := configDoc{Name: "tibi-mcp", Version: version, Configured: s.ready()}
	for _, d := range fieldDefs {
		f := configField{Key: envNames[d.key], Label: d.label, Type: d.typ, Help: d.help, Required: d.required, Default: d.def, Locked: a.envSet[d.key]}
		switch d.key {
		case "card_number":
			f.Value, f.Set = s.CardNumber, s.CardNumber != ""
		case "pin":
			f.Set = s.PIN != ""
		case "read_only":
			f.Value = strconv.FormatBool(s.readOnly())
			f.Set = a.envSet[d.key] || stored.ReadOnly != nil
		}
		doc.Fields = append(doc.Fields, f)
	}
	return doc
}

// applyConfig validates everything first, then stores and reloads.
func (a *app) applyConfig(u configUpdate) error {
	next := a.store.settings()
	locked := func(env string) (string, error) {
		key := keyForEnv(env)
		if key == "" {
			return "", fmt.Errorf("unknown setting %q", env)
		}
		if a.envSet[key] {
			return "", fmt.Errorf("%s is set by an environment variable and cannot be changed here", env)
		}
		return key, nil
	}
	for env, v := range u.Values {
		key, err := locked(env)
		if err != nil {
			return err
		}
		if key == "pin" && v == "" {
			continue // an empty secret keeps the current one
		}
		if err := setSetting(&next, key, v); err != nil {
			return err
		}
	}
	for _, env := range u.Clear {
		key, err := locked(env)
		if err != nil {
			return err
		}
		_ = setSetting(&next, key, "")
	}
	if err := a.store.update(func(st *state) { st.Settings = next }); err != nil {
		return err
	}
	a.reload()
	return nil
}

func (a *app) runChecks(ctx context.Context) healthDoc {
	h := healthDoc{OK: true, Status: "ok"}
	c, err := a.client()
	if err != nil {
		return healthDoc{Status: "not configured", Checks: []checkResult{{Name: "Settings", Detail: err.Error()}}}
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	v, err := c.KindleEmail(ctx)
	if err != nil {
		h.OK, h.Status = false, "failing"
		h.Checks = append(h.Checks, checkResult{Name: "Login to Tibi", Detail: "login failed; check the borrower number and PIN"})
		return h
	}
	h.Checks = append(h.Checks, checkResult{Name: "Login to Tibi", OK: true, Detail: "logged in"})
	detail := "not set; send_to_kindle needs one (set_kindle_email)"
	if m, _ := v.(map[string]any); m["configured"] == true {
		detail = "set"
	}
	h.Checks = append(h.Checks, checkResult{Name: "Kindle address", OK: true, Detail: detail})
	return h
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func authorized(token string, r *http.Request) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) == 1
}

func (a *app) registerConfigAPI(mux *http.ServeMux, token string) {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("check") == "" {
			_, _ = w.Write([]byte("ok"))
			return
		}
		if !authorized(token, r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		h := a.runChecks(r.Context())
		code := http.StatusOK
		if !h.OK {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, h)
	})
	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(token, r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, a.configDoc())
		case http.MethodPost:
			var u configUpdate
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&u); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
				return
			}
			if err := a.applyConfig(u); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, a.configDoc())
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}
