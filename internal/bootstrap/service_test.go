package bootstrap

import (
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"testing"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

func TestInitPersistsDistinctPrincipalsAndSignedConstitution(t *testing.T) {
	store := testutil.OpenStore(t)
	keyDir := t.TempDir()
	owner := mustSigner(t, filepath.Join(keyDir, "owner.key"), "owner")
	cube := mustSigner(t, filepath.Join(keyDir, "cube.key"), "cube")
	root := mustSigner(t, filepath.Join(keyDir, "constitution-root.key"), "root")
	constitution := []byte(DefaultConstitution)

	service := New(store, clock.System{})
	result, err := service.Init(t.Context(), InitRequest{
		Constitution:       constitution,
		Owner:              owner,
		Cube:               cube,
		ConstitutionalRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.OwnerPrincipalID != owner.PrincipalID() || result.CubePrincipalID != cube.PrincipalID() || result.ConstitutionalRootPrincipalID != root.PrincipalID() {
		t.Fatalf("unexpected principal ids: %+v", result)
	}
	if result.CollectiveID == "" {
		t.Fatal("collective id is empty")
	}

	var principalCount, metadataCount, constitutionCount, policySetCount int
	if err := store.DB().QueryRow(`SELECT count(*) FROM principals`).Scan(&principalCount); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM collective_metadata`).Scan(&metadataCount); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM constitutions WHERE active = 1`).Scan(&constitutionCount); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT count(*) FROM policy_sets WHERE active = 1`).Scan(&policySetCount); err != nil {
		t.Fatal(err)
	}
	if principalCount != 3 || metadataCount != 1 || constitutionCount != 1 || policySetCount != 1 {
		t.Fatalf("counts principals=%d metadata=%d constitutions=%d active_policy_sets=%d", principalCount, metadataCount, constitutionCount, policySetCount)
	}

	var hashText string
	var signature []byte
	var signerID string
	if err := store.DB().QueryRow(`SELECT content_hash, signature, signer_principal_id FROM constitutions WHERE version = 1`).Scan(&hashText, &signature, &signerID); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(constitution)
	if hashText != result.ConstitutionHash {
		t.Fatalf("stored hash = %q, result hash = %q", hashText, result.ConstitutionHash)
	}
	if signerID != string(root.PrincipalID()) {
		t.Fatalf("constitution signer = %q, want %q", signerID, root.PrincipalID())
	}
	if !ed25519.Verify(root.PublicKey(), hash[:], signature) {
		t.Fatal("constitution signature is invalid")
	}

	rows, err := store.DB().Query(`SELECT length(public_key) FROM principals`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var size int
		if err := rows.Scan(&size); err != nil {
			t.Fatal(err)
		}
		if size != ed25519.PublicKeySize {
			t.Fatalf("persisted key length = %d, want public-key length %d", size, ed25519.PublicKeySize)
		}
	}
}

func TestInitRefusesToReplaceExistingOwnerOrConstitution(t *testing.T) {
	store := testutil.OpenStore(t)
	keyDir := t.TempDir()
	request := InitRequest{
		Constitution:       []byte(DefaultConstitution),
		Owner:              mustSigner(t, filepath.Join(keyDir, "owner.key"), "owner"),
		Cube:               mustSigner(t, filepath.Join(keyDir, "cube.key"), "cube"),
		ConstitutionalRoot: mustSigner(t, filepath.Join(keyDir, "root.key"), "root"),
	}
	service := New(store, clock.System{})
	if _, err := service.Init(t.Context(), request); err != nil {
		t.Fatal(err)
	}

	_, err := service.Init(t.Context(), request)
	if !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("second Init error = %v, want ErrAlreadyInitialized", err)
	}
}

func mustSigner(t *testing.T, path, prefix string) identity.Signer {
	t.Helper()
	signer, err := identity.CreateLocalEd25519(path, prefix)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}
