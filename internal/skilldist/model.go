package skilldist

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const (
	IndexVersion    = 1
	ManifestVersion = 1
	ScopeUser       = "user"
	ScopeProject    = "project"
	ScopeTeam       = "team"
	manifestName    = ".eylu-install.json"
)

var distributionNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

type Index struct {
	Version   int       `json:"version"`
	Generated time.Time `json:"generated_at,omitempty"`
	Skills    []Entry   `json:"skills"`
}

type Entry struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	PackageURL  string `json:"package_url"`
	SHA256      string `json:"sha256"`
	TreeSHA256  string `json:"tree_sha256"`
	KeyID       string `json:"key_id"`
	Signature   string `json:"signature"`
}

type Manifest struct {
	Version     int       `json:"version"`
	Registry    string    `json:"registry"`
	Scope       string    `json:"scope"`
	Entry       Entry     `json:"entry"`
	SkillDigest string    `json:"skill_digest"`
	InstalledAt time.Time `json:"installed_at"`
}

type Installation struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Registry   string `json:"registry"`
	Scope      string `json:"scope"`
	Digest     string `json:"digest"`
	TreeSHA256 string `json:"tree_sha256"`
	Path       string `json:"path"`
}

type TeamLock struct {
	Version int            `json:"version"`
	Skills  []Installation `json:"skills"`
}

func SignaturePayload(entry Entry) []byte {
	return []byte(strings.Join([]string{"eylu-skill-v1", entry.Name, normalizeVersion(entry.Version), entry.PackageURL, entry.SHA256, entry.TreeSHA256, entry.KeyID}, "\n"))
}

func validateEntry(entry Entry, publicKeys map[string]string) error {
	if !distributionNamePattern.MatchString(entry.Name) {
		return fmt.Errorf("invalid Skill name %q", entry.Name)
	}
	entry.Version = normalizeVersion(entry.Version)
	if !semver.IsValid(entry.Version) {
		return fmt.Errorf("skill %s version %q is not semantic", entry.Name, entry.Version)
	}
	for label, digest := range map[string]string{"sha256": entry.SHA256, "tree_sha256": entry.TreeSHA256} {
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != 32 {
			return fmt.Errorf("skill %s %s is invalid", entry.Name, label)
		}
	}
	encodedKey, ok := publicKeys[entry.KeyID]
	if !ok {
		return fmt.Errorf("skill %s references unknown key %q", entry.Name, entry.KeyID)
	}
	publicKey, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("skill registry key %q is invalid", entry.KeyID)
	}
	signature, err := base64.StdEncoding.DecodeString(entry.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(publicKey, SignaturePayload(entry), signature) {
		return fmt.Errorf("skill %s signature verification failed", entry.Name)
	}
	return nil
}

func normalizeVersion(version string) string {
	if version != "" && !strings.HasPrefix(version, "v") {
		return "v" + version
	}
	return version
}

func decodeManifest(data []byte) (Manifest, error) {
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, err
	}
	if manifest.Version != ManifestVersion || !distributionNamePattern.MatchString(manifest.Entry.Name) {
		return Manifest{}, errors.New("unsupported or invalid Skill installation manifest")
	}
	return manifest, nil
}
