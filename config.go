package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Settings come from the gateway's settings page (stored in DATA_DIR) or from
// environment variables. Environment variables always win.
type Settings struct {
	CardNumber string `json:"card_number,omitempty"`
	PIN        string `json:"pin,omitempty"`
	// ReadOnly is nil when never chosen; Tibi then stays read-only.
	ReadOnly *bool `json:"read_only,omitempty"`
}

// envNames maps each setting to its environment variable.
var envNames = map[string]string{
	"card_number": "TIBI_CARD_NUMBER",
	"pin":         "TIBI_PIN",
	"read_only":   "MCP_READ_ONLY",
}

func (s Settings) ready() bool { return s.CardNumber != "" && s.PIN != "" }

func (s Settings) readOnly() bool { return s.ReadOnly == nil || *s.ReadOnly }

// fromEnv returns the settings given as environment variables and which of
// them were set.
func fromEnv() (Settings, map[string]bool, error) {
	set := map[string]bool{}
	get := func(key string) string {
		v := strings.TrimSpace(os.Getenv(envNames[key]))
		if v != "" {
			set[key] = true
		}
		return v
	}
	s := Settings{CardNumber: get("card_number"), PIN: get("pin")}
	if v := get("read_only"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return s, set, errors.New("MCP_READ_ONLY must be a boolean")
		}
		s.ReadOnly = &b
	}
	return s, set, nil
}

// merge lays the environment on top of the stored settings.
func merge(stored, env Settings, set map[string]bool) Settings {
	out := stored
	if set["card_number"] {
		out.CardNumber = env.CardNumber
	}
	if set["pin"] {
		out.PIN = env.PIN
	}
	if set["read_only"] {
		out.ReadOnly = env.ReadOnly
	}
	return out
}

type state struct {
	Settings Settings `json:"settings"`
}

// store keeps the settings in DATA_DIR/state.json (mode 0600). Without a
// usable directory they live in memory until restart. Login cookies are never
// stored.
type store struct {
	mu   sync.Mutex
	path string
	st   state
}

func openStore(dir string) (*store, error) {
	s := &store{}
	if dir == "" {
		return s, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return s, fmt.Errorf("DATA_DIR is not usable; settings are kept in memory only")
	}
	s.path = filepath.Join(dir, "state.json")
	data, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return s, fmt.Errorf("could not read stored settings")
	default:
		if json.Unmarshal(data, &s.st) != nil {
			s.st = state{}
			return s, fmt.Errorf("stored settings are corrupt; starting empty")
		}
	}
	return s, nil
}

// update changes the state under lock and writes it to disk.
func (s *store) update(fn func(*state)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.st
	fn(&next)
	if s.path != "" {
		data, err := json.MarshalIndent(next, "", "  ")
		if err != nil {
			return err
		}
		tmp := s.path + ".tmp"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return errors.New("could not save settings")
		}
		if err := os.Rename(tmp, s.path); err != nil {
			return errors.New("could not save settings")
		}
	}
	s.st = next
	return nil
}

func (s *store) settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Settings
}
