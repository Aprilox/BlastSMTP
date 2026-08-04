// Package store persists SMTP profiles and the current draft between runs.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/aprilox/blastsmtp/internal/mailer"
)

// Profile is a named SMTP configuration.
type Profile struct {
	ID   string        `json:"id"`
	Name string        `json:"name"`
	SMTP mailer.Config `json:"smtp"`
}

// Draft is the message being composed, kept so a restart does not lose it.
type Draft struct {
	Subject string            `json:"subject"`
	HTML    string            `json:"html"`
	Text    string            `json:"text"`
	Headers map[string]string `json:"headers"`
}

// Sending holds the pacing preferences of a campaign.
type Sending struct {
	Workers           int  `json:"workers"`
	RatePerMinute     int  `json:"ratePerMinute"`
	BatchSize         int  `json:"batchSize"`
	BatchPauseSeconds int  `json:"batchPauseSeconds"`
	MaxRetries        int  `json:"maxRetries"`
	ReconnectEvery    int  `json:"reconnectEvery"`
	IndexStart        int  `json:"indexStart"`
	StopOnError       bool `json:"stopOnError"`
}

// Data is the whole persisted document.
type Data struct {
	Version int `json:"version"`
	// SavePasswords stores SMTP passwords in clear text inside the config
	// file. Opt-in, and documented as such in the UI.
	SavePasswords bool      `json:"savePasswords"`
	Profiles      []Profile `json:"profiles"`
	ActiveProfile string    `json:"activeProfile"`
	Draft         Draft     `json:"draft"`
	Sending       Sending   `json:"sending"`
}

// Defaults returns a usable configuration for a first run.
func Defaults() Data {
	return Data{
		Version: 1,
		Sending: Sending{
			Workers:           2,
			RatePerMinute:     60,
			BatchSize:         50,
			BatchPauseSeconds: 30,
			MaxRetries:        1,
			ReconnectEvery:    100,
			IndexStart:        1,
		},
		Draft: Draft{Headers: map[string]string{}},
	}
}

// Store reads and writes the configuration file.
type Store struct {
	mu   sync.Mutex
	path string
}

// New returns a store backed by path. An empty path resolves to the per-user
// configuration directory.
func New(path string) (*Store, error) {
	if path == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return nil, fmt.Errorf("cannot resolve the configuration directory: %w", err)
		}
		path = filepath.Join(dir, "BlastSMTP", "config.json")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("cannot create %s: %w", filepath.Dir(path), err)
	}
	return &Store{path: path}, nil
}

// Path is the location of the configuration file.
func (s *Store) Path() string { return s.path }

// Load reads the configuration, returning defaults when the file is absent.
func (s *Store) Load() (Data, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return Defaults(), nil
	}
	if err != nil {
		return Defaults(), err
	}

	data := Defaults()
	if err := json.Unmarshal(raw, &data); err != nil {
		return Defaults(), fmt.Errorf("%s is corrupt: %w", s.path, err)
	}
	if data.Draft.Headers == nil {
		data.Draft.Headers = map[string]string{}
	}
	return data, nil
}

// Save writes the configuration atomically with owner-only permissions.
func (s *Store) Save(data Data) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data.Version = 1
	if !data.SavePasswords {
		for i := range data.Profiles {
			data.Profiles[i].SMTP.Password = ""
		}
	}

	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
