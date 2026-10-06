/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tree

import (
	"os"
	"path/filepath"
	"testing"
)

func mustParse(t *testing.T, raw string) *Entry {
	t.Helper()
	e, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return e
}

func fileContent(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestApplyMaterializesTree(t *testing.T) {
	root := t.TempDir() + "/tree"
	e := mustParse(t, exampleTree)

	changes, err := Apply(root, e, Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(changes) == 0 {
		t.Fatal("expected changes on first apply")
	}

	if got := fileContent(t, root+"/greeting"); got != "Hello, world!" {
		t.Errorf("greeting = %q", got)
	}
	if got := fileContent(t, root+"/dir/subfile"); got != "Content.\n" {
		t.Errorf("dir/subfile = %q", got)
	}
	if fi, err := os.Lstat(root + "/dir"); err != nil || !fi.IsDir() {
		t.Errorf("dir is not a directory: %v", err)
	}
	if fi, err := os.Lstat(root + "/dir/subdir"); err != nil || !fi.IsDir() {
		t.Errorf("dir/subdir is not a directory: %v", err)
	}
	if fi, err := os.Lstat(root + "/symlink"); err != nil {
		t.Errorf("symlink: %v", err)
	} else if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink is not a symlink")
	}
	if tgt, err := os.Readlink(root + "/symlink"); err != nil || tgt != "/" {
		t.Errorf("symlink target = %q, %v", tgt, err)
	}
	fi, err := os.Lstat(root + "/script")
	if err != nil {
		t.Fatalf("script: %v", err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("script mode = %o, want 755", fi.Mode().Perm())
	}
	if fi2, err := os.Lstat(root + "/greeting"); err != nil || fi2.Mode().Perm() != 0o644 {
		t.Errorf("file mode = %o, %v", fi2.Mode().Perm(), err)
	}
}

func TestApplyIdempotent(t *testing.T) {
	root := t.TempDir() + "/tree"
	e := mustParse(t, exampleTree)

	if _, err := Apply(root, e, Options{}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	changes, err := Apply(root, e, Options{})
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("second Apply reported %d changes, want 0: %+v", len(changes), changes)
	}
}

func TestApplyRevertsDrift(t *testing.T) {
	root := t.TempDir() + "/tree"
	e := mustParse(t, `{"f": "correct"}`)

	if _, err := Apply(root, e, Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// External drift: content, mode, extra file, extra dir.
	if err := os.WriteFile(root+"/f", []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root+"/f", 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+"/extra", []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root+"/extradir", 0o755); err != nil {
		t.Fatal(err)
	}

	changes, err := Apply(root, e, Options{})
	if err != nil {
		t.Fatalf("drift Apply: %v", err)
	}
	if len(changes) != 3 {
		t.Fatalf("changes = %+v, want 3 entries (f replace + 2 prunes)", changes)
	}

	if got := fileContent(t, root+"/f"); got != "correct" {
		t.Errorf("f = %q, drift not reverted", got)
	}
	if fi, _ := os.Lstat(root + "/f"); fi.Mode().Perm() != 0o644 {
		t.Errorf("f mode = %o, drift not reverted", fi.Mode().Perm())
	}
	if _, err := os.Lstat(root + "/extra"); !os.IsNotExist(err) {
		t.Errorf("extra still exists")
	}
	if _, err := os.Lstat(root + "/extradir"); !os.IsNotExist(err) {
		t.Errorf("extradir still exists")
	}
}

func TestApplyReplacesTypeDrift(t *testing.T) {
	root := t.TempDir() + "/tree"
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	// Desired: dir where a file is; file where a dir is; symlink where a
	// file is; file where a symlink is.
	if err := os.WriteFile(root+"/a", []byte("was file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root+"/b", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/elsewhere", root+"/c"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root+"/d", 0o755); err != nil {
		t.Fatal(err)
	}

	e := mustParse(t, `{
	  "a": {"x": "nested"},
	  "b": "now a file",
	  "c": "was symlink",
	  "d": ["link", "/new-target"]
	}`)
	if _, err := Apply(root, e, Options{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := fileContent(t, root+"/a/x"); got != "nested" {
		t.Errorf("a/x = %q", got)
	}
	if got := fileContent(t, root+"/b"); got != "now a file" {
		t.Errorf("b = %q", got)
	}
	if got := fileContent(t, root+"/c"); got != "was symlink" {
		t.Errorf("c = %q", got)
	}
	if fi, err := os.Lstat(root + "/d"); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("d not a symlink: %v", err)
	}
	if tgt, _ := os.Readlink(root + "/d"); tgt != "/new-target" {
		t.Errorf("d target = %q", tgt)
	}
}

func TestApplySpecChangePrunes(t *testing.T) {
	root := t.TempDir() + "/tree"

	old := mustParse(t, `{"keep": "1", "drop": {"nested": "2"}}`)
	if _, err := Apply(root, old, Options{}); err != nil {
		t.Fatal(err)
	}

	new_ := mustParse(t, `{"keep": "1"}`)
	changes, err := Apply(root, new_, Options{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "drop" || changes[0].Action != "remove" {
		t.Fatalf("changes = %+v, want single removal of drop", changes)
	}
	if _, err := os.Lstat(root + "/drop"); !os.IsNotExist(err) {
		t.Errorf("drop still exists")
	}
	if got := fileContent(t, root+"/keep"); got != "1" {
		t.Errorf("keep = %q", got)
	}
}

func TestApplySymlinkPruneDoesNotFollow(t *testing.T) {
	root := t.TempDir() + "/tree"
	outside := t.TempDir() + "/outside-target"
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside+"/precious", []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A symlink inside the managed tree pointing outside, then removed from
	// the spec: the prune must remove the link, not the target.
	old := mustParse(t, `{"l": ["link", "`+outside+`"]}`)
	if _, err := Apply(root, old, Options{}); err != nil {
		t.Fatal(err)
	}
	new_ := mustParse(t, `{}`)
	if _, err := Apply(root, new_, Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root + "/l"); !os.IsNotExist(err) {
		t.Errorf("symlink not pruned")
	}
	if _, err := os.Stat(outside + "/precious"); err != nil {
		t.Errorf("symlink target was followed during prune: %v", err)
	}
}

func TestApplyRejectsUnsafeRoots(t *testing.T) {
	e := mustParse(t, `{"a": "b"}`)

	if _, err := Apply("/", e, Options{}); err == nil {
		t.Error("Apply(/) accepted")
	}
	if _, err := Apply("relative/path", e, Options{}); err == nil {
		t.Error("relative root accepted")
	}
	if _, err := Apply("", e, Options{}); err == nil {
		t.Error("empty root accepted")
	}

	tmp := t.TempDir()
	link := tmp + "/rootlink"
	if err := os.Symlink("/etc", link); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(link, e, Options{}); err == nil {
		t.Error("symlinked root accepted")
	}
}

func TestRemove(t *testing.T) {
	root := t.TempDir() + "/tree"
	e := mustParse(t, `{"f": "x"}`)
	if _, err := Apply(root, e, Options{}); err != nil {
		t.Fatal(err)
	}
	if err := Remove(root); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Errorf("root still exists")
	}
	if err := Remove("/"); err == nil {
		t.Error("Remove(/) accepted")
	}
}

func TestApplyEnforcesCustomModes(t *testing.T) {
	root := t.TempDir() + "/tree"
	e := mustParse(t, `{"f": "x", "s": ["script", "true"], "d": {"y": "z"}}`)
	if _, err := Apply(root, e, Options{
		FileMode:   0o600,
		ScriptMode: 0o700,
		DirMode:    0o700,
	}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		"f": 0o600,
		"s": 0o700,
		"d": 0o700,
	} {
		fi, err := os.Lstat(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode = %o, want %o", path, fi.Mode().Perm(), want)
		}
	}
}
