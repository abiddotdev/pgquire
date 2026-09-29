package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgconn"
)

// Profile is a named connection. The DSN (and any password in it) never leaves the server.
type Profile struct {
	Name     string `json:"name"`
	DSN      string `json:"dsn"`
	Tag      string `json:"tag,omitempty"` // "prod" gets read-only by default and a warning colour
	ReadOnly bool   `json:"readOnly,omitempty"`
	// User and Password, when set, replace the login in DSN — e.g. a read-only login pgquire created.
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	saved    bool   // in the connections file (false = this run only)
}

// publicProfile is what the browser sees.
type publicProfile struct {
	Name     string `json:"name"`
	Tag      string `json:"tag,omitempty"`
	ReadOnly bool   `json:"readOnly"`
	Saved    bool   `json:"saved"`
	Host     string `json:"host,omitempty"`
	Port     uint16 `json:"port,omitempty"`
	Database string `json:"database,omitempty"`
	User     string `json:"user,omitempty"`
}

func (p *Profile) public() publicProfile {
	out := publicProfile{Name: p.Name, Tag: p.Tag, ReadOnly: p.ReadOnly, Saved: p.saved}
	if c, err := pgconn.ParseConfig(p.DSN); err == nil {
		out.Host, out.Port, out.Database, out.User = c.Host, c.Port, c.Database, c.User
	}
	if p.User != "" {
		out.User = p.User
	}
	return out
}

type profileStore struct {
	mu   sync.RWMutex
	path string
	list []*Profile
}

type profileFile struct {
	Connections []*Profile `json:"connections"`
}

func loadProfiles(path string) (*profileStore, error) {
	s := &profileStore{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f profileFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	for _, p := range f.Connections {
		if p == nil || strings.TrimSpace(p.Name) == "" || p.DSN == "" {
			continue
		}
		p.saved = true
		s.list = append(s.list, p)
	}
	return s, nil
}

func (s *profileStore) count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.list)
}

func (s *profileStore) get(name string) *Profile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.list {
		if p.Name == name {
			return p
		}
	}
	return nil
}

func (s *profileStore) all() []publicProfile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]publicProfile, 0, len(s.list))
	for _, p := range s.list {
		out = append(out, p.public())
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// put adds or replaces a profile, writing the file when it is (or was) saved.
func (s *profileStore) put(p *Profile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	wasSaved := false
	for i, x := range s.list {
		if x.Name == p.Name {
			wasSaved = x.saved
			s.list = append(s.list[:i], s.list[i+1:]...)
			break
		}
	}
	s.list = append(s.list, p)
	if p.saved || wasSaved {
		return s.writeLocked()
	}
	return nil
}

func (s *profileStore) remove(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, x := range s.list {
		if x.Name == name {
			s.list = append(s.list[:i], s.list[i+1:]...)
			if x.saved {
				return true, s.writeLocked()
			}
			return true, nil
		}
	}
	return false, nil
}

// writeLocked saves the saved profiles atomically with owner-only permissions.
func (s *profileStore) writeLocked() error {
	f := profileFile{Connections: []*Profile{}}
	for _, p := range s.list {
		if p.saved {
			f.Connections = append(f.Connections, p)
		}
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".connections-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}
