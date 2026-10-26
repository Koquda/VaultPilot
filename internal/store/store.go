package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/Koquda/VaultPilot/internal/billing"
)

const fileVersion = 1
const maxSavedBytes = 16 << 20

type envelope struct {
	Version   int               `json:"version"`
	Workspace billing.Workspace `json:"workspace"`
}

type Store struct {
	mu        sync.RWMutex
	path      string
	workspace billing.Workspace
	lock      *os.File
	closed    bool
}

func Open(path string, today time.Time) (*Store, error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(directory, ".vaultpilot.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open workspace lock: %w", err)
	}
	if err = lock.Chmod(0600); err != nil {
		lock.Close()
		return nil, fmt.Errorf("protect workspace lock: %w", err)
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("workspace is already open in another process")
	}
	s := &Store{path: path, lock: lock}
	failed := true
	defer func() {
		if failed {
			s.Close()
		}
	}()
	input, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		s.workspace = billing.Demo(today)
		s.workspace.Revision = 1
		if err = billing.ValidateWorkspace(s.workspace); err != nil {
			return nil, fmt.Errorf("initialize workspace: %w", err)
		}
		if err = s.save(s.workspace); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("read saved workspace: %w", err)
	} else {
		defer input.Close()
		info, statErr := input.Stat()
		if statErr != nil {
			return nil, fmt.Errorf("inspect saved workspace: %w", statErr)
		}
		if info.Size() > maxSavedBytes {
			return nil, errors.New("saved workspace exceeds size limit; original preserved")
		}
		decoder := json.NewDecoder(io.LimitReader(input, maxSavedBytes+1))
		decoder.DisallowUnknownFields()
		var saved envelope
		if err = decoder.Decode(&saved); err != nil {
			return nil, fmt.Errorf("saved workspace is invalid; original preserved: %w", err)
		}
		var extra any
		if err = decoder.Decode(&extra); err != io.EOF {
			return nil, errors.New("saved workspace contains trailing data; original preserved")
		}
		if saved.Version != fileVersion {
			return nil, fmt.Errorf("unsupported saved workspace version %d; original preserved", saved.Version)
		}
		if err = billing.ValidateWorkspace(saved.Workspace); err != nil {
			return nil, fmt.Errorf("saved workspace is invalid; original preserved: %w", err)
		}
		if err = os.Chmod(path, 0600); err != nil {
			return nil, fmt.Errorf("protect saved workspace: %w", err)
		}
		s.workspace = saved.Workspace
	}
	failed = false
	return s, nil
}

func (s *Store) Read() billing.Workspace {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clone(s.workspace)
}

func (s *Store) Update(change func(*billing.Workspace) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("workspace is closed")
	}
	if s.workspace.Revision == math.MaxUint64 {
		return errors.New("workspace revision limit reached")
	}
	next := clone(s.workspace)
	if err := change(&next); err != nil {
		return err
	}
	next.Revision = s.workspace.Revision + 1
	if err := billing.ValidateWorkspace(next); err != nil {
		return fmt.Errorf("workspace change rejected: %w", err)
	}
	if err := s.save(next); err != nil {
		return err
	}
	s.workspace = clone(next)
	return nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	unlockErr := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	closeErr := s.lock.Close()
	return errors.Join(unlockErr, closeErr)
}

func (s *Store) save(w billing.Workspace) error {
	directory := filepath.Dir(s.path)
	file, err := os.CreateTemp(directory, ".workspace-*")
	if err != nil {
		return fmt.Errorf("save workspace: %w", err)
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err = file.Chmod(0600); err == nil {
		err = json.NewEncoder(file).Encode(envelope{Version: fileVersion, Workspace: w})
	}
	if err == nil {
		err = file.Sync()
	}
	if err == nil {
		err = file.Close()
	}
	if err == nil {
		err = os.Rename(file.Name(), s.path)
	}
	if err != nil {
		return fmt.Errorf("save workspace: %w", err)
	}
	return nil
}

func clone(w billing.Workspace) billing.Workspace {
	w.Active.Rows = append([]billing.Row(nil), w.Active.Rows...)
	budgets := make(map[string]billing.Budget, len(w.Budgets))
	for key, value := range w.Budgets {
		budgets[key] = value
	}
	w.Budgets = budgets
	acknowledged := make(map[string]bool, len(w.Acknowledged))
	for key, value := range w.Acknowledged {
		acknowledged[key] = value
	}
	w.Acknowledged = acknowledged
	return w
}
