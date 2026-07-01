package a2a

import (
	"errors"
	"fmt"
	"testing"

	"github.com/hugr-lab/hugen/pkg/protocol"
)

// fakeRootStore is a programmable rootStore for registry tests.
type fakeRootStore struct {
	opens   int
	resumes int
	lookups int

	// bound maps contextID → an existing rootID the rebuild path should find
	// (the restart fixture). Empty by default.
	bound map[string]string
	// unresumable marks rootIDs that resumeRoot should reject.
	unresumable map[string]bool

	nextID int
}

func (f *fakeRootStore) openRoot(_ string) (string, error) {
	f.opens++
	f.nextID++
	return fmt.Sprintf("root-%d", f.nextID), nil
}

func (f *fakeRootStore) resumeRoot(rootID string) error {
	f.resumes++
	if f.unresumable[rootID] {
		return errors.New("not resumable")
	}
	return nil
}

func (f *fakeRootStore) boundRoot(contextID string) (string, bool, error) {
	f.lookups++
	if id, ok := f.bound[contextID]; ok {
		return id, true, nil
	}
	return "", false, nil
}

func TestResolve_NewContext_OpensOnceThenCaches(t *testing.T) {
	fs := &fakeRootStore{}
	r := newContextRegistry(fs, quietLogger())

	cs1, err := r.resolve("ctx-1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cs1.RootID() == "" {
		t.Fatal("resolve returned empty rootID")
	}
	if fs.opens != 1 {
		t.Errorf("opens = %d, want 1", fs.opens)
	}

	cs2, err := r.resolve("ctx-1")
	if err != nil {
		t.Fatalf("resolve (repeat): %v", err)
	}
	if cs2 != cs1 {
		t.Errorf("repeat resolve returned a different contextSession (%p vs %p)", cs2, cs1)
	}
	if fs.opens != 1 {
		t.Errorf("opens after cache hit = %d, want 1", fs.opens)
	}
	if fs.lookups != 1 {
		t.Errorf("boundRoot lookups = %d, want 1 (only on the first miss)", fs.lookups)
	}
}

func TestResolve_DistinctContexts_DistinctRoots(t *testing.T) {
	fs := &fakeRootStore{}
	r := newContextRegistry(fs, quietLogger())

	a, _ := r.resolve("ctx-a")
	b, _ := r.resolve("ctx-b")
	if a.RootID() == b.RootID() {
		t.Errorf("distinct contexts share rootID %q", a.RootID())
	}
	if fs.opens != 2 {
		t.Errorf("opens = %d, want 2", fs.opens)
	}
}

func TestResolve_Restart_ResumesBoundRoot(t *testing.T) {
	fs := &fakeRootStore{bound: map[string]string{"ctx-1": "root-9"}}
	r := newContextRegistry(fs, quietLogger())

	cs, err := r.resolve("ctx-1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cs.RootID() != "root-9" {
		t.Errorf("rootID = %q, want %q (resumed binding)", cs.RootID(), "root-9")
	}
	if fs.resumes != 1 {
		t.Errorf("resumes = %d, want 1", fs.resumes)
	}
	if fs.opens != 0 {
		t.Errorf("opens = %d, want 0 (should resume, not open)", fs.opens)
	}
}

func TestResolve_BoundButNotResumable_OpensFresh(t *testing.T) {
	fs := &fakeRootStore{
		bound:       map[string]string{"ctx-1": "root-dead"},
		unresumable: map[string]bool{"root-dead": true},
	}
	r := newContextRegistry(fs, quietLogger())

	cs, err := r.resolve("ctx-1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cs.RootID() == "root-dead" {
		t.Error("resolve returned the dead root instead of opening fresh")
	}
	if fs.resumes != 1 {
		t.Errorf("resumes = %d, want 1 (attempted)", fs.resumes)
	}
	if fs.opens != 1 {
		t.Errorf("opens = %d, want 1 (fell back to fresh)", fs.opens)
	}
}

func TestResolve_EmptyContextID(t *testing.T) {
	r := newContextRegistry(&fakeRootStore{}, quietLogger())
	if _, err := r.resolve(""); !errors.Is(err, errEmptyContextID) {
		t.Fatalf("resolve(\"\") err = %v, want errEmptyContextID", err)
	}
}

func TestForget_RebindsOnNextResolve(t *testing.T) {
	fs := &fakeRootStore{}
	r := newContextRegistry(fs, quietLogger())

	r.resolve("ctx-1") // opens root-1
	r.forget("ctx-1")
	cs, _ := r.resolve("ctx-1") // cache cleared → opens again

	if fs.opens != 2 {
		t.Errorf("opens = %d, want 2 (forget cleared the cache)", fs.opens)
	}
	if cs.RootID() == "" {
		t.Error("post-forget resolve returned empty rootID")
	}
}

// Compile-time check that the production participant is well-formed.
func TestServiceParticipant(t *testing.T) {
	p := serviceParticipant()
	if p.ID == "" || p.Kind != protocol.ParticipantUser {
		t.Fatalf("serviceParticipant = %+v, want non-empty user identity", p)
	}
}
