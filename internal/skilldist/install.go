package skilldist

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"Eylu/internal/config"
	"Eylu/internal/skill"
)

const (
	maxArchiveFiles     = 2000
	maxExtractedBytes   = 16 << 20
	maxArchiveFileBytes = 2 << 20
)

type InstallOptions struct {
	Scope     string
	Workspace string
	Home      string
	Force     bool
}

func Install(ctx context.Context, registry *Registry, entry Entry, options InstallOptions) (Installation, error) {
	if err := validateEntry(entry, registry.Config.PublicKeys); err != nil {
		return Installation{}, err
	}
	target, boundary, err := targetAndBoundary(options.Scope, options.Workspace, options.Home, entry.Name)
	if err != nil {
		return Installation{}, err
	}
	if info, statErr := os.Lstat(target); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return Installation{}, errors.New("skill installation target is not a regular directory")
		}
		if !options.Force {
			if _, manifestErr := LoadManifest(target); manifestErr != nil {
				return Installation{}, errors.New("skill target is unmanaged; use an explicit force option to replace it")
			}
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return Installation{}, statErr
	}
	archive, err := registry.Download(ctx, entry)
	if err != nil {
		return Installation{}, err
	}
	packageDigest := sha256.Sum256(archive)
	if hex.EncodeToString(packageDigest[:]) != entry.SHA256 {
		return Installation{}, fmt.Errorf("skill %s package SHA-256 mismatch", entry.Name)
	}
	parent := filepath.Dir(target)
	if err := ensureSafeDirectory(boundary, parent); err != nil {
		return Installation{}, err
	}
	stagingRoot, err := os.MkdirTemp(parent, "."+entry.Name+"-install-*")
	if err != nil {
		return Installation{}, err
	}
	defer os.RemoveAll(stagingRoot)
	staging := filepath.Join(stagingRoot, entry.Name)
	if err := os.Mkdir(staging, 0o700); err != nil {
		return Installation{}, err
	}
	if err := extractArchive(archive, staging); err != nil {
		return Installation{}, err
	}
	treeDigest, err := TreeDigest(staging)
	if err != nil {
		return Installation{}, err
	}
	if treeDigest != entry.TreeSHA256 {
		return Installation{}, fmt.Errorf("skill %s extracted tree SHA-256 mismatch", entry.Name)
	}
	parsed, err := skill.ParseDirectory(staging, sourceForScope(options.Scope), true)
	if err != nil {
		return Installation{}, fmt.Errorf("validate installed Skill: %w", err)
	}
	if parsed.Name != entry.Name {
		return Installation{}, fmt.Errorf("package declares Skill %q, expected %q", parsed.Name, entry.Name)
	}
	manifest := Manifest{Version: ManifestVersion, Registry: registry.Name, Scope: options.Scope, Entry: entry, SkillDigest: parsed.Digest, InstalledAt: time.Now().UTC()}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Installation{}, err
	}
	if err := writeSynced(filepath.Join(staging, manifestName), manifestData, 0o600); err != nil {
		return Installation{}, err
	}
	installation := Installation{
		Name: entry.Name, Version: entry.Version, Registry: registry.Name, Scope: options.Scope,
		Digest: parsed.Digest, TreeSHA256: treeDigest, Path: target,
	}
	var lockPath string
	var lockData []byte
	if options.Scope == ScopeTeam {
		lockPath, lockData, err = prepareTeamLock(options.Workspace, installation)
		if err != nil {
			return Installation{}, err
		}
	}
	replacement, err := replaceDirectory(staging, target)
	if err != nil {
		return Installation{}, err
	}
	if options.Scope == ScopeTeam {
		if err := writeAtomic(lockPath, lockData, 0o600); err != nil {
			return Installation{}, errors.Join(err, replacement.Rollback())
		}
	}
	if err := replacement.Commit(); err != nil {
		return Installation{}, err
	}
	return installation, nil
}

func VerifyDirectory(directory string, registries map[string]config.SkillRegistryConfig) (Installation, error) {
	manifestData, err := os.ReadFile(filepath.Join(directory, manifestName))
	if err != nil {
		return Installation{}, err
	}
	manifest, err := decodeManifest(manifestData)
	if err != nil {
		return Installation{}, err
	}
	registryConfig, ok := registries[manifest.Registry]
	if !ok {
		return Installation{}, fmt.Errorf("skill registry %q is not configured", manifest.Registry)
	}
	if err := validateEntry(manifest.Entry, registryConfig.PublicKeys); err != nil {
		return Installation{}, err
	}
	treeDigest, err := TreeDigest(directory)
	if err != nil {
		return Installation{}, err
	}
	if treeDigest != manifest.Entry.TreeSHA256 {
		return Installation{}, errors.New("installed Skill tree digest mismatch")
	}
	parsed, err := skill.ParseDirectory(directory, sourceForScope(manifest.Scope), true)
	if err != nil {
		return Installation{}, err
	}
	if parsed.Name != manifest.Entry.Name || parsed.Digest != manifest.SkillDigest {
		return Installation{}, errors.New("installed Skill metadata digest mismatch")
	}
	return Installation{
		Name: parsed.Name, Version: manifest.Entry.Version, Registry: manifest.Registry, Scope: manifest.Scope,
		Digest: parsed.Digest, TreeSHA256: treeDigest, Path: directory,
	}, nil
}

func LoadManifest(directory string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(directory, manifestName))
	if err != nil {
		return Manifest{}, err
	}
	return decodeManifest(data)
}

func LatestUpdate(ctx context.Context, registry *Registry, manifest Manifest) (Entry, bool, error) {
	entry, err := registry.Select(ctx, manifest.Entry.Name, "")
	if err != nil {
		return Entry{}, false, err
	}
	comparison := semver.Compare(entry.Version, normalizeVersion(manifest.Entry.Version))
	if comparison < 0 {
		return Entry{}, false, fmt.Errorf("registry latest version %s is below installed version %s", entry.Version, manifest.Entry.Version)
	}
	return entry, comparison > 0, nil
}

func Target(scope, workspace, home, name string) (string, error) {
	target, _, err := targetAndBoundary(scope, workspace, home, name)
	return target, err
}

func targetAndBoundary(scope, workspace, home, name string) (string, string, error) {
	if !distributionNamePattern.MatchString(name) {
		return "", "", fmt.Errorf("invalid Skill name %q", name)
	}
	workspacePath, err := filepath.Abs(workspace)
	if err != nil {
		return "", "", err
	}
	switch scope {
	case ScopeUser:
		if home == "" {
			home, err = os.UserHomeDir()
			if err != nil {
				return "", "", err
			}
		}
		home, err = filepath.Abs(home)
		if err != nil {
			return "", "", err
		}
		return filepath.Join(home, ".eylu", "skills", name), home, nil
	case ScopeProject:
		return filepath.Join(workspacePath, ".eylu", "skills", name), workspacePath, nil
	case ScopeTeam:
		return filepath.Join(workspacePath, ".agents", "skills", name), workspacePath, nil
	default:
		return "", "", fmt.Errorf("invalid Skill installation scope %q", scope)
	}
}

func ensureSafeDirectory(boundary, directory string) error {
	boundary, err := filepath.Abs(boundary)
	if err != nil {
		return err
	}
	realBoundary, err := filepath.EvalSymlinks(boundary)
	if err != nil {
		return fmt.Errorf("resolve skill installation boundary: %w", err)
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(boundary, directory)
	if err != nil || relativeEscapes(relative) {
		return errors.New("skill installation directory escapes its boundary")
	}
	current := boundary
	for _, component := range strings.FieldsFunc(relative, func(value rune) bool { return value == '/' || value == '\\' }) {
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil {
				return err
			}
		} else if statErr != nil {
			return statErr
		} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("skill installation directory %s is not a regular directory", current)
		}
		resolved, err := filepath.EvalSymlinks(current)
		if err != nil {
			return err
		}
		resolvedRelative, err := filepath.Rel(realBoundary, resolved)
		if err != nil || relativeEscapes(resolvedRelative) {
			return errors.New("skill installation directory resolves outside its boundary")
		}
	}
	return nil
}

func relativeEscapes(relative string) bool {
	return filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func TreeDigest(directory string) (string, error) {
	type fileEntry struct {
		relative string
		path     string
		size     int64
	}
	files := make([]fileEntry, 0)
	var totalBytes int64
	err := filepath.WalkDir(directory, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill tree contains symlink %s", current)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("skill tree contains non-regular file %s", current)
		}
		relative, err := filepath.Rel(directory, current)
		if err != nil {
			return err
		}
		if filepath.ToSlash(relative) == manifestName {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxArchiveFileBytes {
			return fmt.Errorf("skill file %s exceeds %d bytes", relative, maxArchiveFileBytes)
		}
		totalBytes += info.Size()
		if totalBytes > maxExtractedBytes {
			return fmt.Errorf("skill tree exceeds %d bytes", maxExtractedBytes)
		}
		files = append(files, fileEntry{relative: filepath.ToSlash(relative), path: current, size: info.Size()})
		if len(files) > maxArchiveFiles {
			return fmt.Errorf("skill tree exceeds %d files", maxArchiveFiles)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].relative < files[j].relative })
	hasher := sha256.New()
	for _, file := range files {
		if err := hashTreeFile(hasher, file.relative, file.path, file.size); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func hashTreeFile(hasher hash.Hash, relative, filePath string, size int64) error {
	_ = binary.Write(hasher, binary.BigEndian, uint32(len(relative)))
	_, _ = io.WriteString(hasher, relative)
	_ = binary.Write(hasher, binary.BigEndian, uint64(size))
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(hasher, io.LimitReader(file, maxArchiveFileBytes+1))
	return err
}

func extractArchive(archive []byte, directory string) error {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return err
	}
	if len(reader.File) > maxArchiveFiles {
		return fmt.Errorf("skill archive exceeds %d entries", maxArchiveFiles)
	}
	var total int64
	for _, archived := range reader.File {
		if strings.Contains(archived.Name, `\`) {
			return fmt.Errorf("skill archive path %q uses a backslash", archived.Name)
		}
		clean := path.Clean(archived.Name)
		if clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || clean == manifestName {
			return fmt.Errorf("skill archive path %q is invalid", archived.Name)
		}
		mode := archived.Mode()
		if mode&os.ModeSymlink != 0 || (!mode.IsRegular() && !mode.IsDir()) {
			return fmt.Errorf("skill archive entry %q is not a regular file or directory", archived.Name)
		}
		target := filepath.Join(directory, filepath.FromSlash(clean))
		relative, err := filepath.Rel(directory, target)
		if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("skill archive path %q escapes extraction root", archived.Name)
		}
		if mode.IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			continue
		}
		if archived.UncompressedSize64 > maxArchiveFileBytes {
			return fmt.Errorf("skill archive file %q exceeds %d bytes", archived.Name, maxArchiveFileBytes)
		}
		total += int64(archived.UncompressedSize64)
		if total > maxExtractedBytes {
			return fmt.Errorf("skill archive exceeds %d extracted bytes", maxExtractedBytes)
		}
		if err := extractFile(archived, target); err != nil {
			return err
		}
	}
	return nil
}

func extractFile(archived *zip.File, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	reader, err := archived.Open()
	if err != nil {
		return err
	}
	defer reader.Close()
	mode := os.FileMode(0o600)
	if archived.Mode()&0o111 != 0 {
		mode = 0o700
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(file, io.LimitReader(reader, maxArchiveFileBytes+1))
	if copyErr == nil && written > maxArchiveFileBytes {
		copyErr = errors.New("skill archive file exceeded extraction limit")
	}
	if copyErr == nil {
		copyErr = file.Sync()
	}
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

type directoryReplacement struct {
	target    string
	backup    string
	hadTarget bool
}

func replaceDirectory(staging, target string) (*directoryReplacement, error) {
	parent := filepath.Dir(target)
	backup, err := os.MkdirTemp(parent, ".skill-backup-*")
	if err != nil {
		return nil, err
	}
	if err := os.Remove(backup); err != nil {
		return nil, err
	}
	hadTarget := false
	if info, statErr := os.Lstat(target); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, errors.New("skill installation target is not a regular directory")
		}
		if err := os.Rename(target, backup); err != nil {
			return nil, err
		}
		hadTarget = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	if err := os.Rename(staging, target); err != nil {
		if hadTarget {
			_ = os.Rename(backup, target)
		}
		return nil, err
	}
	return &directoryReplacement{target: target, backup: backup, hadTarget: hadTarget}, nil
}

func (r *directoryReplacement) Commit() error {
	if r == nil || !r.hadTarget {
		return nil
	}
	return os.RemoveAll(r.backup)
}

func (r *directoryReplacement) Rollback() error {
	if r == nil {
		return nil
	}
	if err := os.RemoveAll(r.target); err != nil {
		return err
	}
	if r.hadTarget {
		return os.Rename(r.backup, r.target)
	}
	return nil
}

func prepareTeamLock(workspace string, installation Installation) (string, []byte, error) {
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return "", nil, err
	}
	lockDirectory := filepath.Join(workspace, ".eylu")
	if err := ensureSafeDirectory(workspace, lockDirectory); err != nil {
		return "", nil, err
	}
	lockPath := filepath.Join(lockDirectory, "skills.lock.json")
	portable := installation
	if relative, err := filepath.Rel(workspace, installation.Path); err == nil {
		portable.Path = filepath.ToSlash(relative)
	}
	lock := TeamLock{Version: 1}
	if data, err := os.ReadFile(lockPath); err == nil {
		if err := json.Unmarshal(data, &lock); err != nil || lock.Version != 1 {
			return "", nil, errors.New("team Skill lock file is invalid")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}
	replaced := false
	for index := range lock.Skills {
		if lock.Skills[index].Name == installation.Name {
			lock.Skills[index] = portable
			replaced = true
			break
		}
	}
	if !replaced {
		lock.Skills = append(lock.Skills, portable)
	}
	sort.Slice(lock.Skills, func(i, j int) bool { return lock.Skills[i].Name < lock.Skills[j].Name })
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return "", nil, err
	}
	return lockPath, data, nil
}

func writeSynced(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func writeAtomic(target string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".skill-lock-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return replaceFile(name, target)
}

func sourceForScope(scope string) skill.Source {
	switch scope {
	case ScopeUser:
		return skill.SourceUserEylu
	case ScopeTeam:
		return skill.SourceProjectAgents
	default:
		return skill.SourceProjectEylu
	}
}

func VerifySignature(entry Entry, encodedPublicKey string) bool {
	publicKey, err := base64.StdEncoding.DecodeString(encodedPublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return false
	}
	signature, err := base64.StdEncoding.DecodeString(entry.Signature)
	return err == nil && len(signature) == ed25519.SignatureSize && ed25519.Verify(publicKey, SignaturePayload(entry), signature)
}
