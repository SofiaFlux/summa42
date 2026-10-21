package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
)

type LocalEd25519 struct {
	principalID domain.ID
	privateKey  ed25519.PrivateKey
	publicKey   ed25519.PublicKey
}

// NewLocalEd25519 loads an existing key. It never creates one: a missing key
// file is an error, so a mistyped path or a lost key cannot silently become a
// new identity. Bootstrap creates keys with CreateLocalEd25519.
func NewLocalEd25519(path, principalPrefix string) (*LocalEd25519, error) {
	prefix := strings.TrimSpace(principalPrefix)
	if prefix == "" {
		return nil, errors.New("principal prefix must not be empty")
	}
	privateKey, err := loadLocalEd25519(path)
	if err != nil {
		return nil, err
	}
	return newLocalSigner(prefix, privateKey)
}

// CreateLocalEd25519 is the bootstrap path (`summa42 init`): it generates the
// key file when it is missing and otherwise loads the existing one, so a
// re-run after a partial init keeps the same identity.
func CreateLocalEd25519(path, principalPrefix string) (*LocalEd25519, error) {
	prefix := strings.TrimSpace(principalPrefix)
	if prefix == "" {
		return nil, errors.New("principal prefix must not be empty")
	}
	privateKey, err := loadLocalEd25519(path)
	if errors.Is(err, os.ErrNotExist) {
		privateKey, err = createLocalEd25519(path)
		if errors.Is(err, os.ErrExist) {
			privateKey, err = loadLocalEd25519(path)
		}
	}
	if err != nil {
		return nil, err
	}
	return newLocalSigner(prefix, privateKey)
}

func newLocalSigner(prefix string, privateKey ed25519.PrivateKey) (*LocalEd25519, error) {
	publicKey, ok := privateKey.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("stored key is not Ed25519")
	}
	publicCopy := append(ed25519.PublicKey(nil), publicKey...)
	privateCopy := append(ed25519.PrivateKey(nil), privateKey...)
	return &LocalEd25519{
		principalID: principalID(prefix, publicCopy),
		privateKey:  privateCopy,
		publicKey:   publicCopy,
	}, nil
}

// NewConstitutionalRootForCeremony is intentionally explicit: normal Box startup
// must not load constitutional root material. It creates the key, so it is for
// the init ceremony only.
func NewConstitutionalRootForCeremony(path string) (*LocalEd25519, error) {
	return CreateLocalEd25519(path, "root")
}

func (s *LocalEd25519) PrincipalID() domain.ID {
	return s.principalID
}

func (s *LocalEd25519) PublicKey() ed25519.PublicKey {
	return append(ed25519.PublicKey(nil), s.publicKey...)
}

func (s *LocalEd25519) Sign(message []byte) ([]byte, error) {
	return ed25519.Sign(s.privateKey, message), nil
}

func (s *LocalEd25519) CustodyProfile() string {
	return LocalDevFileCustody
}

func principalID(prefix string, publicKey ed25519.PublicKey) domain.ID {
	digest := sha256.Sum256(publicKey)
	return domain.ID(prefix + "_" + hex.EncodeToString(digest[:16]))
}

func loadLocalEd25519(path string) (ed25519.PrivateKey, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	// POSIX mode bits carry no meaning on Windows; ACLs guard the file there.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("key file %s has mode %o, want 600", path, info.Mode().Perm())
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, rest := pem.Decode(encoded)
	if block == nil || len(rest) != 0 || block.Type != "PRIVATE KEY" {
		return nil, errors.New("invalid PKCS#8 PEM private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKCS#8 private key: %w", err)
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("PKCS#8 key is not Ed25519")
	}
	return privateKey, nil
}

func createLocalEd25519(path string) (ed25519.PrivateKey, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	removeOnError := true
	defer func() {
		_ = file.Close()
		if removeOnError {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(encoded); err != nil {
		return nil, err
	}
	if err := file.Sync(); err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	removeOnError = false
	return privateKey, nil
}
