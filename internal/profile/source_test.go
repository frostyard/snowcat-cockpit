package profile

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRefreshKitFromExactGitRevisionAndServeItToWorkspace(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "snowcat")
	committed := validSourceKitContents("source-backed revision")
	revision := commitSourceKit(t, source, committed)

	// The prepared commit, not mutable checkout state, is the authority.
	firstSkill := LockedManifest().Skills[0].Name
	dirty := []byte("uncommitted checkout drift\n")
	if err := os.WriteFile(filepath.Join(source, ".agents", "skills", firstSkill, "SKILL.md"), dirty, 0o600); err != nil {
		t.Fatal(err)
	}

	active := filepath.Join(root, "active-kit")
	if _, err := InstallKit(active); err != nil {
		t.Fatal(err)
	}
	previous := LockedManifest().Source.Revision
	now := time.Date(2026, 8, 26, 1, 2, 3, 4, time.UTC)
	refreshed, err := RefreshKitFromGit(context.Background(), source, revision, active, now)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.PreviousRevision != previous || refreshed.Revision != revision || refreshed.RetainedDirectory == "" {
		t.Fatalf("refresh = %#v", refreshed)
	}
	if refreshed.RetainedDirectory != active {
		t.Fatalf("retained directory = %q, want offline-floor root %q", refreshed.RetainedDirectory, active)
	}
	if _, err := os.Stat(refreshed.RetainedDirectory); err != nil {
		t.Fatalf("retained last-good kit: %v", err)
	}
	selectionInfo, err := os.Lstat(filepath.Join(active, activeSelectionName))
	if err != nil || selectionInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("active selection = %v, %v", selectionInfo, err)
	}
	selection, err := os.Readlink(filepath.Join(active, activeSelectionName))
	if err != nil || selection != filepath.Join(generationDirectoryName, revision) {
		t.Fatalf("active selection target = %q, %v", selection, err)
	}
	manifest, err := ActiveManifest(active)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Source.Revision != revision {
		t.Fatalf("active revision = %q", manifest.Source.Revision)
	}
	activeContent, err := os.ReadFile(filepath.Join(active, generationDirectoryName, revision, firstSkill, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(activeContent) != string(committed[firstSkill]) {
		t.Fatalf("active content = %q, want committed bytes", activeContent)
	}
	floorContent, err := os.ReadFile(filepath.Join(active, firstSkill, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(floorContent) == string(committed[firstSkill]) {
		t.Fatal("source activation overwrote the retained offline-floor directory")
	}
	checkoutContent, err := os.ReadFile(filepath.Join(source, ".agents", "skills", firstSkill, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(checkoutContent) != string(dirty) {
		t.Fatalf("source checkout was mutated: %q", checkoutContent)
	}

	workspace := filepath.Join(root, "workspace-skills")
	installed, served, err := InstallFromDirectory(active, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if installed.Status != StatusReady || served.Source.Revision != revision {
		t.Fatalf("workspace install = %#v manifest = %#v", installed, served)
	}
	if _, err := os.Stat(filepath.Join(workspace, activeManifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ephemeral workspace gained active manifest: %v", err)
	}
}

func TestRefreshKitRejectsMalformedCanonicalSkill(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "snowcat")
	contents := validSourceKitContents("valid")
	contents[LockedManifest().Skills[1].Name] = []byte("not a skill document\n")
	revision := commitSourceKit(t, source, contents)
	active := filepath.Join(root, "active-kit")
	if _, err := InstallKit(active); err != nil {
		t.Fatal(err)
	}
	before, err := ActiveManifest(active)
	if err != nil {
		t.Fatal(err)
	}
	_, err = RefreshKitFromGit(context.Background(), source, revision, active, time.Now())
	if err == nil || errors.Is(err, ErrSourceUnavailable) || !strings.Contains(err.Error(), "frontmatter") {
		t.Fatalf("refresh error = %v", err)
	}
	after, readErr := ActiveManifest(active)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !manifestsEqual(before, after) {
		t.Fatalf("malformed source changed active kit: before=%#v after=%#v", before, after)
	}
	if _, err := os.Lstat(filepath.Join(active, activeSelectionName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("malformed source created active selection: %v", err)
	}
}

func TestSkillDocumentRequiresTopLevelStringMetadata(t *testing.T) {
	t.Parallel()

	name := LockedManifest().Skills[0].Name
	for label, content := range map[string]string{
		"nested fields":            "---\nmetadata:\n  name: " + name + "\n  description: Nested.\n---\n\nbody\n",
		"null description":         "---\nname: " + name + "\ndescription: null\n---\n\nbody\n",
		"empty quoted description": "---\nname: " + name + "\ndescription: \"\"\n---\n\nbody\n",
		"boolean description":      "---\nname: " + name + "\ndescription: true\n---\n\nbody\n",
		"numeric description":      "---\nname: " + name + "\ndescription: 123\n---\n\nbody\n",
		"duplicate name":           "---\nname: " + name + "\nname: " + name + "\ndescription: Duplicate.\n---\n\nbody\n",
		"invalid unknown YAML":     "---\nname: " + name + "\ndescription: Valid.\nbroken: [\n---\n\nbody\n",
	} {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			if err := validateSkillDocument(name, []byte(content)); err == nil {
				t.Fatalf("invalid skill metadata was accepted: %q", content)
			}
		})
	}
}

func TestMatchingRootManifestIsPromotedToImmutableGeneration(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "snowcat")
	contents := validSourceKitContents("matching root")
	revision := commitSourceKit(t, source, contents)
	bundle, err := bundleFromGit(context.Background(), source, revision)
	if err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(root, "active-kit")
	if _, err := installBundle(bundle, active, true); err != nil {
		t.Fatal(err)
	}
	if selected, err := HasSourceSelection(active); err != nil || selected {
		t.Fatalf("initial source selection = %t, %v", selected, err)
	}
	refreshed, err := RefreshKitFromGit(context.Background(), source, revision, active, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Revision != revision || refreshed.RetainedDirectory != active {
		t.Fatalf("refresh = %#v", refreshed)
	}
	if selected, err := HasSourceSelection(active); err != nil || !selected {
		t.Fatalf("promoted source selection = %t, %v", selected, err)
	}
}

func TestSharedKitLockSerializesSourceSelection(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	firstSource := filepath.Join(root, "snowcat-first")
	firstRevision := commitSourceKit(t, firstSource, validSourceKitContents("first"))
	secondSource := filepath.Join(root, "snowcat-second")
	secondRevision := commitSourceKit(t, secondSource, validSourceKitContents("second"))
	active := filepath.Join(root, "active-kit")
	if _, err := InstallKit(active); err != nil {
		t.Fatal(err)
	}
	if _, err := RefreshKitFromGit(context.Background(), firstSource, firstRevision, active, time.Now()); err != nil {
		t.Fatal(err)
	}
	unlock, err := LockKitShared(context.Background(), active)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, refreshErr := RefreshKitFromGit(context.Background(), secondSource, secondRevision, active, time.Now())
		done <- refreshErr
	}()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("exclusive refresh ignored shared launch lock: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("source refresh did not continue after shared lock release")
	}
}

func TestMultiTargetInstallPreflightsEveryRootBeforeWriting(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	active := filepath.Join(root, "active-kit")
	if _, err := InstallKit(active); err != nil {
		t.Fatal(err)
	}
	agents := filepath.Join(root, "workspace", ".agents", "skills")
	claude := filepath.Join(root, "workspace", ".claude", "skills")
	conflict := filepath.Join(claude, LockedManifest().Skills[0].Name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(conflict), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conflict, []byte("operator-owned conflict\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := InstallFromDirectoryTargets(active, []string{agents, claude}); err == nil {
		t.Fatal("multi-target install succeeded over a conflicting second root")
	}
	firstTarget := filepath.Join(agents, LockedManifest().Skills[0].Name, "SKILL.md")
	if _, err := os.Stat(firstTarget); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("first target was partially written: %v", err)
	}
}

func TestRefreshKitSourceFailureRetainsLastGoodKit(t *testing.T) {
	t.Parallel()

	active := filepath.Join(t.TempDir(), "active-kit")
	if _, err := InstallKit(active); err != nil {
		t.Fatal(err)
	}
	before, err := ActiveManifest(active)
	if err != nil {
		t.Fatal(err)
	}
	result, err := RefreshKitFromGit(
		context.Background(),
		t.TempDir(),
		strings.Repeat("f", 40),
		active,
		time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC),
	)
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("refresh error = %v", err)
	}
	if result.RetainedDirectory != "" {
		t.Fatalf("source failure retained a new directory: %#v", result)
	}
	after, readErr := ActiveManifest(active)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !manifestsEqual(before, after) {
		t.Fatalf("last-good kit changed: before=%#v after=%#v", before, after)
	}
}

func commitSourceKit(t *testing.T, repository string, contents map[string][]byte) string {
	t.Helper()
	runTestGit(t, "", "init", "-q", repository)
	runTestGit(t, repository, "config", "user.name", "Cockpit Test")
	runTestGit(t, repository, "config", "user.email", "cockpit-test@example.invalid")
	for _, skill := range LockedManifest().Skills {
		path := filepath.Join(repository, ".agents", "skills", skill.Name, "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, contents[skill.Name], 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runTestGit(t, repository, "add", ".agents/skills")
	runTestGit(t, repository, "commit", "-q", "-m", "test worker kit")
	return strings.TrimSpace(runTestGit(t, repository, "rev-parse", "HEAD"))
}

func validSourceKitContents(marker string) map[string][]byte {
	contents := make(map[string][]byte, len(LockedManifest().Skills))
	for _, skill := range LockedManifest().Skills {
		contents[skill.Name] = []byte(
			"---\nname: " + skill.Name + "\ndescription: Test source-backed skill.\n---\n\n# " +
				skill.Name + "\n\n" + marker + "\n",
		)
	}
	return contents
}

func runTestGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	commandArguments := arguments
	if directory != "" {
		commandArguments = append([]string{"-C", directory}, arguments...)
	}
	command := exec.Command("git", commandArguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", commandArguments, err, output)
	}
	return string(output)
}
