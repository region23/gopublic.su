package storage

import (
	"errors"
	"os"
	"testing"

	apperrors "gopublic/internal/errors"
	"gopublic/internal/models"
)

// setupTestStore creates an in-memory SQLite store for testing.
func setupTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	// Use a temp file so CGO sqlite works (some drivers don't support :memory: + multiple conns)
	f, err := os.CreateTemp("", "gopublic-test-*.db")
	if err != nil {
		t.Fatalf("failed to create temp db: %v", err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	store, err := NewSQLiteStore(f.Name())
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// createTestUser inserts a minimal user and returns its ID.
func createTestUser(t *testing.T, store *SQLiteStore) uint {
	t.Helper()
	u := &models.User{Email: "test@example.com"}
	if err := store.CreateUser(u); err != nil {
		t.Fatalf("createTestUser: %v", err)
	}
	return u.ID
}

// TestCreateDomain_UniqueConstraint verifies that inserting a domain with the
// same name twice returns ErrDuplicateKey instead of a raw SQLite error.
// Regression test for: sqlite3.Error: UNIQUE constraint failed: domains.name
func TestCreateDomain_UniqueConstraint(t *testing.T) {
	store := setupTestStore(t)
	userID := createTestUser(t, store)

	first := &models.Domain{Name: "misty-river-abc123", UserID: userID}
	if err := store.CreateDomain(first); err != nil {
		t.Fatalf("first CreateDomain failed unexpectedly: %v", err)
	}

	// Attempt to insert the same domain name again (even for same user).
	duplicate := &models.Domain{Name: "misty-river-abc123", UserID: userID}
	err := store.CreateDomain(duplicate)
	if err == nil {
		t.Fatal("expected error for duplicate domain name, got nil")
	}
	if !errors.Is(err, apperrors.ErrDuplicateKey) {
		t.Errorf("expected ErrDuplicateKey, got: %v", err)
	}
}

// TestCreateDomain_DifferentNamesSucceed verifies that two distinct domain
// names can be created without conflict.
func TestCreateDomain_DifferentNamesSucceed(t *testing.T) {
	store := setupTestStore(t)
	userID := createTestUser(t, store)

	for _, name := range []string{"alpha-dog-001", "beta-cat-002"} {
		d := &models.Domain{Name: name, UserID: userID}
		if err := store.CreateDomain(d); err != nil {
			t.Errorf("CreateDomain(%q) failed: %v", name, err)
		}
	}
}

// TestCreateUserWithTokenAndDomains_DuplicateDomain verifies that the
// transactional registration path also surfaces ErrDuplicateKey when a
// generated domain name already exists in the database.
func TestCreateUserWithTokenAndDomains_DuplicateDomain(t *testing.T) {
	store := setupTestStore(t)

	// Pre-seed the domain that will collide.
	seed := &models.User{Email: "seed@example.com"}
	if err := store.CreateUser(seed); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := store.CreateDomain(&models.Domain{Name: "colliding-name-xyz", UserID: seed.ID}); err != nil {
		t.Fatalf("seed domain: %v", err)
	}

	// Now register a new user whose domain list includes the same name.
	newUser := &models.User{Email: "new@example.com"}
	reg := UserRegistration{
		User:    newUser,
		Domains: []string{"colliding-name-xyz"},
	}
	_, _, err := store.CreateUserWithTokenAndDomains(reg)
	if err == nil {
		t.Fatal("expected error for duplicate domain in registration, got nil")
	}
	if !errors.Is(err, apperrors.ErrDuplicateKey) {
		t.Errorf("expected ErrDuplicateKey, got: %v", err)
	}
}
