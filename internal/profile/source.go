package profile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const activeManifestName = ".snowcat-worker-kit.json"
const canonicalRepository = "https://github.com/frostyard/snowcat"
const activeSelectionName = ".active"
const generationDirectoryName = ".generations"
const kitLockName = ".kit.lock"

// ErrSourceUnavailable marks a managed Git source lookup that could not
// produce the requested revision or skill bytes.
var ErrSourceUnavailable = errors.New("worker kit source unavailable")

var (
	revisionRE  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestRE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	skillNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)
)

type kitBundle struct {
	manifest Manifest
	files    map[string][]byte
}

// PreparedInstall is one verified active generation and a set of destination
// roots that were all checked before any file is written.
type PreparedInstall struct {
	Manifest Manifest
	bundle   kitBundle
	targets  []string
}

// RefreshResult reports a source-addressed worker-kit refresh. The previous
// directory is retained beside the active kit whenever the revision changes.
type RefreshResult struct {
	Directory         string       `json:"directory"`
	Status            string       `json:"status"`
	Revision          string       `json:"revision"`
	PreviousRevision  string       `json:"previousRevision,omitempty"`
	RetainedDirectory string       `json:"retainedDirectory,omitempty"`
	Checks            []SkillCheck `json:"checks"`
}

// ActiveManifest returns the manifest selected from skillsDirectory. A legacy
// installation without a manifest remains the embedded offline floor.
func ActiveManifest(skillsDirectory string) (Manifest, error) {
	if skillsDirectory == "" {
		return Manifest{}, fmt.Errorf("worker kit directory is not configured")
	}
	manifest, _, err := activeManifestAndDirectory(skillsDirectory)
	return manifest, err
}

// HasSourceSelection reports whether the node has selected an immutable
// source-backed generation rather than the root offline-floor directory.
func HasSourceSelection(skillsDirectory string) (bool, error) {
	_, err := os.Lstat(filepath.Join(skillsDirectory, activeSelectionName))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect active worker kit selection: %w", err)
	}
	return true, nil
}

// LockKitShared prevents a source generation switch until the caller has
// completed its revision-bound launch decision.
func LockKitShared(ctx context.Context, skillsDirectory string) (func(), error) {
	return lockKit(ctx, skillsDirectory, false)
}

func activeManifestAndDirectory(skillsDirectory string) (Manifest, string, error) {
	directory, err := activeKitDirectory(skillsDirectory)
	if err != nil {
		return Manifest{}, skillsDirectory, err
	}
	path := filepath.Join(directory, activeManifestName)
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		if directory != skillsDirectory {
			return Manifest{}, directory, fmt.Errorf("selected worker kit generation has no manifest")
		}
		return mustManifest(), directory, nil
	}
	if err != nil {
		return Manifest{}, directory, fmt.Errorf("read active worker kit manifest: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Manifest{}, directory, fmt.Errorf("inspect active worker kit manifest: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 64*1024 {
		return Manifest{}, directory, fmt.Errorf("active worker kit manifest is not a bounded regular file")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 64*1024))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, directory, fmt.Errorf("decode active worker kit manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Manifest{}, directory, fmt.Errorf("decode active worker kit manifest: trailing content")
	}
	if err := validateManifest(manifest); err != nil {
		return Manifest{}, directory, fmt.Errorf("validate active worker kit manifest: %w", err)
	}
	return manifest, directory, nil
}

// InstallFromDirectory copies the active, verified bundle into a worker or
// preflight workspace. It never falls back to different bytes silently.
func InstallFromDirectory(sourceSkillsDirectory, targetSkillsDirectory string) (InstallResult, Manifest, error) {
	results, manifest, err := InstallFromDirectoryTargets(sourceSkillsDirectory, []string{targetSkillsDirectory})
	if err != nil {
		return InstallResult{Directory: targetSkillsDirectory}, Manifest{}, err
	}
	return results[0], manifest, nil
}

// InstallFromDirectoryTargets reads one coherent active generation and copies
// those exact bytes into every target.
func InstallFromDirectoryTargets(sourceSkillsDirectory string, targetSkillsDirectories []string) ([]InstallResult, Manifest, error) {
	prepared, err := PrepareInstallFromDirectory(sourceSkillsDirectory, targetSkillsDirectories)
	if err != nil {
		return nil, Manifest{}, err
	}
	results, err := prepared.Install()
	return results, prepared.Manifest, err
}

// PrepareInstallFromDirectory loads one coherent active generation and
// preflights every destination before any destination is changed.
func PrepareInstallFromDirectory(sourceSkillsDirectory string, targetSkillsDirectories []string) (PreparedInstall, error) {
	manifest, selectedDirectory, err := activeManifestAndDirectory(sourceSkillsDirectory)
	if err != nil {
		return PreparedInstall{}, err
	}
	bundle, err := bundleFromDirectory(manifest, selectedDirectory)
	if err != nil {
		return PreparedInstall{}, err
	}
	for _, target := range targetSkillsDirectories {
		if err := preflightBundleTarget(bundle, target); err != nil {
			return PreparedInstall{}, err
		}
	}
	return PreparedInstall{
		Manifest: manifest,
		bundle:   bundle,
		targets:  append([]string(nil), targetSkillsDirectories...),
	}, nil
}

// Install writes a previously prepared generation to every destination.
func (prepared PreparedInstall) Install() ([]InstallResult, error) {
	results := make([]InstallResult, 0, len(prepared.targets))
	for _, target := range prepared.targets {
		result, err := installBundle(prepared.bundle, target, false)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

// VerifyDirectory proves that directory contains exactly the manifest-addressed
// skill bytes Cockpit expects to serve.
func VerifyDirectory(manifest Manifest, directory string) error {
	_, err := bundleFromDirectory(manifest, directory)
	return err
}

// ManifestFromGit reads Snowcat's canonical skill bytes from one exact commit
// without moving the managed source checkout.
func ManifestFromGit(ctx context.Context, sourceDirectory, revision string) (Manifest, error) {
	bundle, err := bundleFromGit(ctx, sourceDirectory, revision)
	if err != nil {
		return Manifest{}, err
	}
	return bundle.manifest, nil
}

// RefreshKitFromGit atomically replaces a healthy active kit with the
// canonical skills from one exact managed Snowcat commit. The prior kit is
// retained as the last-good rollback copy; source acquisition failures leave
// the active kit untouched.
func RefreshKitFromGit(
	ctx context.Context,
	sourceDirectory string,
	revision string,
	skillsDirectory string,
	_ time.Time,
) (RefreshResult, error) {
	result := RefreshResult{Directory: skillsDirectory, Status: StatusReady, Revision: revision}
	if skillsDirectory == "" {
		return result, fmt.Errorf("worker kit directory is not configured")
	}
	bundle, err := bundleFromGit(ctx, sourceDirectory, revision)
	if err != nil {
		return result, err
	}
	unlock, err := lockKit(ctx, skillsDirectory, true)
	if err != nil {
		return result, err
	}
	defer unlock()
	current, currentDirectory, err := activeManifestAndDirectory(skillsDirectory)
	if err != nil {
		return result, err
	}
	currentKit := inspectKit(current, currentDirectory)
	if currentKit.Status != StatusReady {
		return result, fmt.Errorf("active worker kit is %s; repair it before source refresh", currentKit.Status)
	}
	result.PreviousRevision = current.Source.Revision
	if currentDirectory != skillsDirectory && manifestsEqual(current, bundle.manifest) {
		result.Checks = currentKit.Checks
		return result, nil
	}

	generations := filepath.Join(skillsDirectory, generationDirectoryName)
	if err := os.MkdirAll(generations, 0o700); err != nil {
		return result, fmt.Errorf("create worker kit generations: %w", err)
	}
	stage, err := os.MkdirTemp(generations, ".stage-")
	if err != nil {
		return result, fmt.Errorf("create worker kit staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()
	installed, err := installBundle(bundle, stage, true)
	if err != nil {
		return result, fmt.Errorf("stage source worker kit: %w", err)
	}

	generation := filepath.Join(generations, bundle.manifest.Source.Revision)
	if info, err := os.Lstat(generation); err == nil {
		if !info.IsDir() {
			return result, fmt.Errorf("existing source worker kit generation is not a directory")
		}
		if manifestInfo, manifestErr := os.Lstat(filepath.Join(generation, activeManifestName)); manifestErr != nil || !manifestInfo.Mode().IsRegular() {
			return result, fmt.Errorf("existing source worker kit generation has no regular manifest")
		}
		existingManifest, manifestErr := ActiveManifest(generation)
		if manifestErr != nil {
			return result, fmt.Errorf("verify existing source worker kit generation manifest: %w", manifestErr)
		}
		if !manifestsEqual(existingManifest, bundle.manifest) {
			return result, fmt.Errorf("existing source worker kit generation manifest differs")
		}
		if _, verifyErr := bundleFromDirectory(bundle.manifest, generation); verifyErr != nil {
			return result, fmt.Errorf("verify existing source worker kit generation: %w", verifyErr)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, fmt.Errorf("inspect source worker kit generation: %w", err)
	} else if err := os.Rename(stage, generation); err != nil {
		return result, fmt.Errorf("retain source worker kit generation: %w", err)
	}
	selection, err := os.CreateTemp(skillsDirectory, ".active-")
	if err != nil {
		return result, fmt.Errorf("create worker kit selection: %w", err)
	}
	selectionPath := selection.Name()
	if err := selection.Close(); err != nil {
		return result, fmt.Errorf("close worker kit selection: %w", err)
	}
	if err := os.Remove(selectionPath); err != nil {
		return result, fmt.Errorf("prepare worker kit selection: %w", err)
	}
	defer func() { _ = os.Remove(selectionPath) }()
	relativeGeneration := filepath.Join(generationDirectoryName, bundle.manifest.Source.Revision)
	if err := os.Symlink(relativeGeneration, selectionPath); err != nil {
		return result, fmt.Errorf("create worker kit selection: %w", err)
	}
	if err := os.Rename(selectionPath, filepath.Join(skillsDirectory, activeSelectionName)); err != nil {
		return result, fmt.Errorf("activate source worker kit: %w", err)
	}
	result.Revision = bundle.manifest.Source.Revision
	result.RetainedDirectory = currentDirectory
	result.Checks = installed.Checks
	return result, nil
}

// IsCanonicalRepository reports whether repository is the Snowcat source
// repository named by the manifest.
func IsCanonicalRepository(manifest Manifest, repository string) bool {
	parsed, err := url.Parse(manifest.Source.Repository)
	if err != nil || !strings.EqualFold(parsed.Host, "github.com") {
		return false
	}
	return strings.EqualFold(strings.Trim(parsed.Path, "/"), repository)
}

// IsCanonicalRepositorySlug reports whether repository is Snowcat's canonical
// source checkout.
func IsCanonicalRepositorySlug(repository string) bool {
	return strings.EqualFold(repository, "frostyard/snowcat")
}

// SameSkillContent compares the named skill digests independently of source
// commit identity.
func SameSkillContent(left, right Manifest) bool {
	if len(left.Skills) != len(right.Skills) {
		return false
	}
	for index := range left.Skills {
		if left.Skills[index] != right.Skills[index] {
			return false
		}
	}
	return true
}

func embeddedBundle() (kitBundle, error) {
	manifest := mustManifest()
	files := make(map[string][]byte, len(manifest.Skills))
	for _, skill := range manifest.Skills {
		content, err := workerKit.ReadFile(filepath.ToSlash(filepath.Join("worker-kit", skill.Name, "SKILL.md")))
		if err != nil {
			return kitBundle{}, fmt.Errorf("read embedded skill %s: %w", skill.Name, err)
		}
		files[skill.Name] = content
	}
	bundle := kitBundle{manifest: manifest, files: files}
	if err := verifyBundle(bundle); err != nil {
		return kitBundle{}, err
	}
	return bundle, nil
}

func bundleFromDirectory(manifest Manifest, skillsDirectory string) (kitBundle, error) {
	files := make(map[string][]byte, len(manifest.Skills))
	for _, skill := range manifest.Skills {
		path := filepath.Join(skillsDirectory, skill.Name, "SKILL.md")
		content, err := readSkillFile(path)
		if err != nil {
			return kitBundle{}, fmt.Errorf("read active skill %s: %w", skill.Name, err)
		}
		files[skill.Name] = content
	}
	bundle := kitBundle{manifest: manifest, files: files}
	if err := verifyBundle(bundle); err != nil {
		return kitBundle{}, err
	}
	return bundle, nil
}

func bundleFromGit(ctx context.Context, sourceDirectory, revision string) (kitBundle, error) {
	if sourceDirectory == "" || !revisionRE.MatchString(revision) {
		return kitBundle{}, fmt.Errorf("source directory and a full 40-character revision are required")
	}
	if err := runGit(ctx, sourceDirectory, "cat-file", "-e", revision+"^{commit}"); err != nil {
		return kitBundle{}, fmt.Errorf("%w: verify Snowcat source revision: %v", ErrSourceUnavailable, err)
	}
	floor := mustManifest()
	files := make(map[string][]byte, len(floor.Skills))
	skills := make([]LockedSkill, 0, len(floor.Skills))
	for _, locked := range floor.Skills {
		path := filepath.ToSlash(filepath.Join(".agents", "skills", locked.Name, "SKILL.md"))
		content, err := gitOutput(ctx, sourceDirectory, "show", revision+":"+path)
		if err != nil {
			return kitBundle{}, fmt.Errorf("read %s at %s: %w", path, shortRevision(revision), err)
		}
		sum := sha256.Sum256(content)
		digest := hex.EncodeToString(sum[:])
		files[locked.Name] = content
		skills = append(skills, LockedSkill{Name: locked.Name, SHA256: digest})
	}
	bundle := kitBundle{
		manifest: Manifest{
			Version: floor.Version,
			Source:  Source{Repository: floor.Source.Repository, Revision: revision},
			Skills:  skills,
		},
		files: files,
	}
	if err := verifyBundle(bundle); err != nil {
		return kitBundle{}, err
	}
	return bundle, nil
}

func installBundle(bundle kitBundle, skillsDirectory string, persistManifest bool) (InstallResult, error) {
	result := InstallResult{Directory: skillsDirectory, Status: StatusReady}
	if skillsDirectory == "" {
		return result, fmt.Errorf("worker kit directory is not configured")
	}
	if err := verifyBundle(bundle); err != nil {
		return result, err
	}

	if err := preflightBundleTarget(bundle, skillsDirectory); err != nil {
		result.Status = StatusDrifted
		return result, err
	}

	if err := os.MkdirAll(skillsDirectory, 0o700); err != nil {
		return result, fmt.Errorf("create worker kit directory: %w", err)
	}
	for _, skill := range bundle.manifest.Skills {
		path := filepath.Join(skillsDirectory, skill.Name, "SKILL.md")
		if digest, err := fileDigest(path); err == nil && digest == skill.SHA256 {
			result.Checks = append(result.Checks, SkillCheck{
				Name: skill.Name, Status: StatusReady, Detail: "already matches the served revision",
			})
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return result, fmt.Errorf("create skill directory %s: %w", skill.Name, err)
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return result, fmt.Errorf("create skill %s: %w", skill.Name, err)
		}
		writeErr := func() error {
			if _, err := file.Write(bundle.files[skill.Name]); err != nil {
				return err
			}
			return file.Sync()
		}()
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			_ = os.Remove(path)
			if writeErr != nil {
				return result, fmt.Errorf("write skill %s: %w", skill.Name, writeErr)
			}
			return result, fmt.Errorf("close skill %s: %w", skill.Name, closeErr)
		}
		result.Checks = append(result.Checks, SkillCheck{
			Name: skill.Name, Status: StatusReady, Detail: "installed from the served worker kit",
		})
	}
	if persistManifest {
		if err := writeActiveManifest(skillsDirectory, bundle.manifest); err != nil {
			return result, err
		}
	}
	return result, nil
}

func preflightBundleTarget(bundle kitBundle, skillsDirectory string) error {
	if skillsDirectory == "" {
		return fmt.Errorf("worker kit directory is not configured")
	}
	for _, skill := range bundle.manifest.Skills {
		path := filepath.Join(skillsDirectory, skill.Name, "SKILL.md")
		digest, err := fileDigest(path)
		if errorsIsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect existing skill %s: %w", skill.Name, err)
		}
		if digest != skill.SHA256 {
			return fmt.Errorf("refusing to replace drifted skill %s", skill.Name)
		}
	}
	return nil
}

func writeActiveManifest(skillsDirectory string, manifest Manifest) error {
	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode active worker kit manifest: %w", err)
	}
	payload = append(payload, '\n')
	temporary, err := os.CreateTemp(skillsDirectory, ".worker-kit-manifest-")
	if err != nil {
		return fmt.Errorf("create active worker kit manifest: %w", err)
	}
	name := temporary.Name()
	defer func() { _ = os.Remove(name) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure active worker kit manifest: %w", err)
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write active worker kit manifest: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync active worker kit manifest: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close active worker kit manifest: %w", err)
	}
	if err := os.Rename(name, filepath.Join(skillsDirectory, activeManifestName)); err != nil {
		return fmt.Errorf("install active worker kit manifest: %w", err)
	}
	return nil
}

func verifyBundle(bundle kitBundle) error {
	if err := validateManifest(bundle.manifest); err != nil {
		return err
	}
	if len(bundle.files) != len(bundle.manifest.Skills) {
		return fmt.Errorf("worker kit files do not match manifest")
	}
	for _, skill := range bundle.manifest.Skills {
		content, ok := bundle.files[skill.Name]
		if !ok {
			return fmt.Errorf("worker kit skill %s is absent", skill.Name)
		}
		sum := sha256.Sum256(content)
		if hex.EncodeToString(sum[:]) != skill.SHA256 {
			return fmt.Errorf("worker kit skill %s does not match manifest", skill.Name)
		}
		if err := validateSkillDocument(skill.Name, content); err != nil {
			return err
		}
	}
	return nil
}

func validateManifest(manifest Manifest) error {
	if manifest.Version != 1 {
		return fmt.Errorf("worker kit manifest version must be 1")
	}
	if manifest.Source.Repository != canonicalRepository || !revisionRE.MatchString(manifest.Source.Revision) {
		return fmt.Errorf("worker kit manifest source is invalid")
	}
	floor := mustManifest()
	if len(manifest.Skills) != len(floor.Skills) {
		return fmt.Errorf("worker kit manifest skill count is invalid")
	}
	seen := make(map[string]bool, len(manifest.Skills))
	for index, skill := range manifest.Skills {
		if skill.Name != floor.Skills[index].Name || !skillNameRE.MatchString(skill.Name) || !digestRE.MatchString(skill.SHA256) || seen[skill.Name] {
			return fmt.Errorf("worker kit manifest skill is invalid")
		}
		seen[skill.Name] = true
	}
	return nil
}

func manifestsEqual(left, right Manifest) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return bytes.Equal(leftJSON, rightJSON)
}

func activeKitDirectory(skillsDirectory string) (string, error) {
	selectionPath := filepath.Join(skillsDirectory, activeSelectionName)
	info, err := os.Lstat(selectionPath)
	if errors.Is(err, os.ErrNotExist) {
		return skillsDirectory, nil
	}
	if err != nil {
		return skillsDirectory, fmt.Errorf("inspect active worker kit selection: %w", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return skillsDirectory, fmt.Errorf("active worker kit selection is not a symbolic link")
	}
	target, err := os.Readlink(selectionPath)
	if err != nil {
		return skillsDirectory, fmt.Errorf("read active worker kit selection: %w", err)
	}
	clean := filepath.Clean(target)
	if filepath.IsAbs(clean) || filepath.Dir(clean) != generationDirectoryName || !revisionRE.MatchString(filepath.Base(clean)) {
		return skillsDirectory, fmt.Errorf("active worker kit selection target is invalid")
	}
	selected := filepath.Join(skillsDirectory, clean)
	selectedInfo, err := os.Lstat(selected)
	if err != nil {
		return skillsDirectory, fmt.Errorf("inspect selected worker kit generation: %w", err)
	}
	if !selectedInfo.IsDir() {
		return skillsDirectory, fmt.Errorf("selected worker kit generation is not a directory")
	}
	return selected, nil
}

func validateSkillDocument(expectedName string, content []byte) error {
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return fmt.Errorf("worker kit skill %s is not bounded UTF-8 text", expectedName)
	}
	text := string(content)
	if !strings.HasPrefix(text, "---\n") {
		return fmt.Errorf("worker kit skill %s has no frontmatter", expectedName)
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return fmt.Errorf("worker kit skill %s has unterminated frontmatter", expectedName)
	}
	header := text[4 : 4+end]
	body := text[4+end+5:]
	var document yaml.Node
	decoder := yaml.NewDecoder(strings.NewReader(header))
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("worker kit skill %s has invalid YAML frontmatter: %w", expectedName, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("worker kit skill %s has multiple YAML documents", expectedName)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("worker kit skill %s frontmatter is not a mapping", expectedName)
	}
	var name string
	var description string
	seen := make(map[string]bool, 2)
	mapping := document.Content[0]
	for index := 0; index < len(mapping.Content); index += 2 {
		key := mapping.Content[index]
		value := mapping.Content[index+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
			return fmt.Errorf("worker kit skill %s frontmatter fields must be strings", expectedName)
		}
		if seen[key.Value] {
			return fmt.Errorf("worker kit skill %s has duplicate frontmatter field %s", expectedName, key.Value)
		}
		seen[key.Value] = true
		switch key.Value {
		case "name":
			name = value.Value
		case "description":
			description = value.Value
		default:
			return fmt.Errorf("worker kit skill %s has unknown frontmatter field %s", expectedName, key.Value)
		}
	}
	if name != expectedName || strings.TrimSpace(description) == "" || strings.TrimSpace(body) == "" {
		return fmt.Errorf("worker kit skill %s has invalid name, description, or body", expectedName)
	}
	return nil
}

func lockKit(ctx context.Context, skillsDirectory string, exclusive bool) (func(), error) {
	if skillsDirectory == "" {
		return nil, fmt.Errorf("worker kit directory is not configured")
	}
	file, err := os.OpenFile(filepath.Join(skillsDirectory, kitLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open worker kit lock: %w", err)
	}
	mode := syscall.LOCK_SH | syscall.LOCK_NB
	if exclusive {
		mode = syscall.LOCK_EX | syscall.LOCK_NB
	}
	for {
		err = syscall.Flock(int(file.Fd()), mode)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("lock worker kit: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("lock worker kit: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func runGit(ctx context.Context, directory string, arguments ...string) error {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", directory}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func gitOutput(ctx context.Context, directory string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", directory}, arguments...)...)
	var output cappedBuffer
	output.limit = int(maxSkillBytes) + 1
	var stderr cappedBuffer
	stderr.limit = 8 * 1024
	command.Stdout = &output
	command.Stderr = &stderr
	err := command.Run()
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	if output.overflow || output.Len() > int(maxSkillBytes) {
		return nil, fmt.Errorf("git output exceeds %d bytes", maxSkillBytes)
	}
	return output.Bytes(), nil
}

func readSkillFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	if info.Size() > maxSkillBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", maxSkillBytes)
	}
	content, err := io.ReadAll(io.LimitReader(file, maxSkillBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > maxSkillBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", maxSkillBytes)
	}
	return content, nil
}

type cappedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (buffer *cappedBuffer) Write(content []byte) (int, error) {
	original := len(content)
	remaining := buffer.limit - buffer.Len()
	if remaining <= 0 {
		buffer.overflow = true
		return original, nil
	}
	if len(content) > remaining {
		content = content[:remaining]
		buffer.overflow = true
	}
	_, _ = buffer.Buffer.Write(content)
	return original, nil
}
