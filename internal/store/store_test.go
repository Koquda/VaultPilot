package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Koquda/VaultPilot/internal/billing"
)

func today() time.Time { return time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC) }
func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "workspace.json")
	s, err := Open(path, today())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}
func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestOpenCreatesProtectedDemoAndRestoresChanges(t *testing.T) {
	s, path := openTestStore(t)
	initial := s.Read()
	if !initial.Demo || initial.Revision != 1 {
		t.Fatalf("invalid first workspace: %+v", initial)
	}
	if err := billing.ValidateWorkspace(initial); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		mode os.FileMode
	}{{path, 0600}, {filepath.Dir(path), 0700}, {filepath.Join(filepath.Dir(path), ".vaultpilot.lock"), 0600}} {
		info, err := os.Stat(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != tc.mode {
			t.Errorf("%s permission %o", tc.path, info.Mode().Perm())
		}
	}
	var decoded envelope
	if err := json.Unmarshal(fileBytes(t, path), &decoded); err != nil || decoded.Version != 1 {
		t.Fatalf("invalid version envelope: %+v %v", decoded, err)
	}
	key := billing.BudgetKey("2026-10", "aws", "USD")
	ack := billing.AlertKey(initial.Active.ID, "aws", "Compute", "2026-10-08")
	if err := s.Update(func(w *billing.Workspace) error {
		w.Budgets[key] = billing.Budget{Month: "2026-10", Provider: "aws", Currency: "USD", Limit: 123000000}
		w.Acknowledged[ack] = true
		w.Revision = 0
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := s.Read()
	if want.Revision != 2 {
		t.Fatal("callback bypassed monotonic revision")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, today())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reflect.DeepEqual(want, reopened.Read()) {
		t.Fatal("restart did not restore exact state")
	}
}

func TestReadAndCallbackOwnershipAreDefensive(t *testing.T) {
	s, _ := openTestStore(t)
	original := s.Read()
	read := s.Read()
	read.Active.Rows[0].Service = "changed"
	for key := range read.Budgets {
		delete(read.Budgets, key)
	}
	read.Acknowledged[strings.Repeat("a", 64)] = true
	if !reflect.DeepEqual(original, s.Read()) {
		t.Fatal("read exposed mutable state")
	}
	var retained *billing.Workspace
	if err := s.Update(func(w *billing.Workspace) error { retained = w; return nil }); err != nil {
		t.Fatal(err)
	}
	saved := s.Read()
	retained.Active.Rows[0].Service = "changed later"
	retained.Budgets["invalid"] = billing.Budget{}
	retained.Acknowledged["invalid"] = true
	if !reflect.DeepEqual(saved, s.Read()) {
		t.Fatal("callback retained live state ownership")
	}
}

func TestRejectedUpdatePreservesLiveStateAndDisk(t *testing.T) {
	s, path := openTestStore(t)
	before := s.Read()
	disk := fileBytes(t, path)
	expected := errors.New("cancelled preview")
	err := s.Update(func(w *billing.Workspace) error { w.Active.Rows[0].Service = "corrupted"; return expected })
	if !errors.Is(err, expected) {
		t.Fatalf("callback error lost: %v", err)
	}
	if err = s.Update(func(w *billing.Workspace) error { w.Active.Rows[0].Cost = billing.MaxRow + 1; return nil }); err == nil {
		t.Fatal("invalid workspace accepted")
	}
	if !reflect.DeepEqual(before, s.Read()) || !bytes.Equal(disk, fileBytes(t, path)) {
		t.Fatal("rejected update changed state")
	}
}

func TestFailedSaveDoesNotPublishAndCleansTemporaryFile(t *testing.T) {
	s, path := openTestStore(t)
	before := s.Read()
	disk := fileBytes(t, path)
	backup := path + ".backup"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	err := s.Update(func(w *billing.Workspace) error { w.Demo = false; return nil })
	if err == nil {
		t.Fatal("rename failure ignored")
	}
	if !reflect.DeepEqual(before, s.Read()) || !bytes.Equal(disk, fileBytes(t, backup)) {
		t.Fatal("failed save published state or damaged old data")
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".workspace-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary files left: %v %v", files, err)
	}
}

func TestCorruptAndUnsupportedFilesRemainUntouched(t *testing.T) {
	valid := billing.Demo(today())
	for _, tc := range []struct {
		name string
		data []byte
	}{{"malformed", []byte("{broken")}, {"unsupported version", mustJSON(t, envelope{Version: 99, Workspace: valid})}, {"invalid fingerprint", mustJSON(t, envelope{Version: 1, Workspace: billing.Workspace{}})}, {"trailing document", append(mustJSON(t, envelope{Version: 1, Workspace: valid}), []byte(" {}")...)}, {"unknown field", []byte(`{"version":1,"workspace":{},"unexpected":true}`)}} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "workspace.json")
			if err := os.WriteFile(path, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			if s, err := Open(path, today()); err == nil {
				s.Close()
				t.Fatal("invalid saved state opened")
			}
			if !bytes.Equal(tc.data, fileBytes(t, path)) {
				t.Fatal("corrupt file overwritten")
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			s, err := Open(path, today())
			if err != nil {
				t.Fatalf("failed open retained process lock: %v", err)
			}
			s.Close()
		})
	}
}
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestExclusiveLockAcrossProcesses(t *testing.T) {
	if path := os.Getenv("VAULTPILOT_LOCK_TEST_PATH"); path != "" {
		s, err := Open(path, today())
		if err == nil {
			s.Close()
			t.Fatal("another process opened the workspace")
		}
		return
	}
	s, path := openTestStore(t)
	command := exec.Command(os.Args[0], "-test.run=^TestExclusiveLockAcrossProcesses$")
	command.Env = append(os.Environ(), "VAULTPILOT_LOCK_TEST_PATH="+path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("process lock failed: %v %s", err, output)
	}
	if _, err := Open(filepath.Join(filepath.Dir(path), "different.json"), today()); err == nil {
		t.Fatal("second writer in same directory opened")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, today())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err = s.Close(); err != nil {
		t.Fatal("close is not idempotent")
	}
	if err = s.Update(func(w *billing.Workspace) error { return nil }); err == nil {
		t.Fatal("closed writer accepted update")
	}
}

func TestConcurrentUpdatesAreSerializedAndPersisted(t *testing.T) {
	s, path := openTestStore(t)
	start := s.Read().Revision
	const count = 20
	var group sync.WaitGroup
	failures := make(chan error, count)
	for i := 0; i < count; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			key := billing.BudgetKey("2026-10", "", "USD")
			err := s.Update(func(w *billing.Workspace) error {
				budget := w.Budgets[key]
				budget.Month = "2026-10"
				budget.Currency = "USD"
				budget.Limit++
				w.Budgets[key] = budget
				return nil
			})
			if err != nil {
				failures <- fmt.Errorf("update %d: %w", i, err)
			}
			_ = s.Read()
		}(i)
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	got := s.Read()
	if got.Revision != start+count {
		t.Fatalf("lost update: revision %d", got.Revision)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, today())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reflect.DeepEqual(got, reopened.Read()) {
		t.Fatal("disk diverges from final live update")
	}
}
