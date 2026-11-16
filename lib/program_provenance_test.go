package lib

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestCurrentProgramProvenanceIsFreshPerInstallationAndConcurrent(t *testing.T) {
	const callers = 12
	results := make([]programProvenance, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dir := t.TempDir()
			db := filepath.Join(dir, "state.db")
			if err := os.WriteFile(db, []byte("installation"), 0600); err != nil {
				errs[i] = err
				return
			}
			results[i], errs[i] = currentProgramProvenance(Config{DatabasePath: db}, int64(i+1))
		}(i)
	}
	wg.Wait()
	seenDB := map[string]bool{}
	seenSchema := map[int64]bool{}
	for i, p := range results {
		if errs[i] != nil {
			t.Fatalf("provenance call %d: %v", i, errs[i])
		}
		if p.SHA256 == "" || p.Executable == "" || p.Schema != int64(i+1) {
			t.Fatalf("incomplete provenance: %+v", p)
		}
		seenDB[p.Database+":"+p.DatabaseIdentity] = true
		seenSchema[p.Schema] = true
		if i > 0 && p.SHA256 != results[0].SHA256 {
			t.Fatalf("same running executable produced different fingerprints: %s != %s", p.SHA256, results[0].SHA256)
		}
	}
	if len(seenDB) != callers || len(seenSchema) != callers {
		t.Fatalf("installation/schema bindings were reused: %d DBs, %d schemas", len(seenDB), len(seenSchema))
	}
}

func TestExecutablePathMustStillNameRunningInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manager")
	if err := os.WriteFile(path, []byte("running"), 0700); err != nil {
		t.Fatal(err)
	}
	identity, err := filesystemPathIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyExecutablePath(path, identity); err != nil {
		t.Fatalf("original executable path rejected: %v", err)
	}
	replacement := filepath.Join(dir, "replacement")
	if err := os.WriteFile(replacement, []byte("replacement"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if err := verifyExecutablePath(path, identity); err == nil {
		t.Fatal("replacement executable path accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := verifyExecutablePath(path, identity); err == nil {
		t.Fatal("removed executable path accepted")
	}
}
