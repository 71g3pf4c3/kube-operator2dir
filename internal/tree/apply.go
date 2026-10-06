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
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Options controls ownership and permission bits applied during
// materialization. Zero-value modes fall back to ApplyDefaults.
type Options struct {
	FileMode   os.FileMode
	ScriptMode os.FileMode
	DirMode    os.FileMode
	UID        *int
	GID        *int
}

// DefaultFileMode, DefaultScriptMode and DefaultDirMode are used when the CR
// spec does not override them.
const (
	DefaultFileMode   os.FileMode = 0o644
	DefaultScriptMode os.FileMode = 0o755
	DefaultDirMode    os.FileMode = 0o755
)

// ApplyDefaults fills zero-value modes with the defaults.
func (o *Options) ApplyDefaults() {
	if o.FileMode == 0 {
		o.FileMode = DefaultFileMode
	}
	if o.ScriptMode == 0 {
		o.ScriptMode = DefaultScriptMode
	}
	if o.DirMode == 0 {
		o.DirMode = DefaultDirMode
	}
}

// Change describes one mutation performed (or that would need to be
// performed) relative to the observed on-disk state.
type Change struct {
	Path   string `json:"path"`   // relative to root, slash-separated
	Action string `json:"action"` // create|replace|remove|fix-perms|fix-owner
}

// Apply materializes e under root with replace semantics:
//
//   - entries on disk that are absent from the spec are removed (prune);
//   - files whose content, mode or managed ownership drift are atomically
//     replaced (temp file + rename in the same directory);
//   - symlinks are created with os.Symlink and never followed;
//   - path safety: every path is built from validated single-segment names
//     under a validated root; before any mutation the current entry type is
//     checked with lstat so we never write through a symlink or remove
//     through one.
//
// Known residual risk (documented, same class as any lstat-then-write
// scheme): an attacker winning the race between the lstat check and the
// write on the same host could redirect a mutation. The strict openat/
// O_NOFOLLOW traversal is a possible future hardening.
//
// Apply returns the list of changes it made (drift report).
func Apply(root string, e *Entry, opts Options) ([]Change, error) {
	if e == nil || e.Kind != KindDir {
		return nil, fmt.Errorf("tree root must be a directory entry")
	}
	opts.ApplyDefaults()

	root, err := validateRoot(root)
	if err != nil {
		return nil, err
	}
	var changes []Change
	if err := ensureRoot(root, opts); err != nil {
		return nil, err
	}
	changes, err = applyDir(root, "", e, opts)
	if err != nil {
		return nil, err
	}
	return changes, nil
}

// Remove prunes the whole materialized tree: it removes root (recursively).
// It shares validateRoot's safety rules, so it can never be steered at the
// filesystem root or through a symlink.
func Remove(root string) error {
	root, err := validateRoot(root)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("prune %s: %w", root, err)
	}
	return nil
}

// validateRoot normalizes and safety-checks the root path. The root must be
// absolute, must not be the filesystem root, and must not itself be a
// symlink (a symlinked root would silently relocate every subsequent write).
func validateRoot(root string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("root must not be empty")
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("root %q must be an absolute path", root)
	}
	clean := filepath.Clean(root)
	if clean == "/" {
		return "", fmt.Errorf("root must not be the filesystem root")
	}
	if fi, err := os.Lstat(clean); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("root %s is a symlink; refusing to operate", clean)
	}
	return clean, nil
}

func ensureRoot(root string, opts Options) error {
	fi, err := os.Lstat(root)
	switch {
	case os.IsNotExist(err):
		if err := os.MkdirAll(root, opts.DirMode); err != nil {
			return fmt.Errorf("create root %s: %w", root, err)
		}
		return enforceDirMeta(root, opts)
	case err != nil:
		return fmt.Errorf("stat root %s: %w", root, err)
	case !fi.IsDir():
		return fmt.Errorf("root %s exists and is not a directory", root)
	}
	return enforceDirMeta(root, opts)
}

// enforceDirMeta enforces the managed mode/owner on a directory (the
// directory entry itself, not its content).
func enforceDirMeta(path string, opts Options) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm() != opts.DirMode.Perm() {
		if err := os.Chmod(path, opts.DirMode.Perm()); err != nil {
			return fmt.Errorf("chmod %s: %w", path, err)
		}
	}
	return enforceOwner(path, fi, opts)
}

func applyDir(dir, rel string, e *Entry, opts Options) ([]Change, error) {
	var changes []Change
	if err := enforceDirMeta(dir, opts); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	existing := make(map[string]os.DirEntry, len(entries))
	for _, de := range entries {
		existing[de.Name()] = de
	}

	for name, child := range e.Children {
		childPath := filepath.Join(dir, name)
		childRel := name
		if rel != "" {
			childRel = rel + "/" + name
		}
		var ch []Change
		var err error
		switch child.Kind {
		case KindDir:
			ch, err = applyChildDir(childPath, childRel, child, opts)
		case KindFile:
			ch, err = applyFile(childPath, childRel, child.Content, opts.FileMode, opts)
		case KindScript:
			ch, err = applyFile(childPath, childRel, child.Content, opts.ScriptMode, opts)
		case KindSymlink:
			ch, err = applySymlink(childPath, childRel, child.Target, opts)
		default:
			err = fmt.Errorf("unknown entry kind %q", child.Kind)
		}
		if err != nil {
			return nil, err
		}
		changes = append(changes, ch...)
	}

	// Prune: entries on disk that the spec no longer defines.
	for name := range existing {
		if _, want := e.Children[name]; want {
			continue
		}
		pruneRel := name
		if rel != "" {
			pruneRel = rel + "/" + name
		}
		// RemoveAll on a symlink removes the link itself, never follows it.
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return nil, fmt.Errorf("prune %s: %w", pruneRel, err)
		}
		changes = append(changes, Change{Path: pruneRel, Action: "remove"})
	}

	return changes, nil
}

// applyChildDir materializes a desired directory at path: whatever is there
// of a different type (file, symlink) is removed first, then the directory
// (fresh or pre-existing) is enforced recursively.
func applyChildDir(path, rel string, e *Entry, opts Options) ([]Change, error) {
	fi, err := os.Lstat(path)
	switch {
	case os.IsNotExist(err):
		if err := os.Mkdir(path, opts.DirMode); err != nil {
			return nil, fmt.Errorf("create dir %s: %w", path, err)
		}
	case err != nil:
		return nil, fmt.Errorf("stat %s: %w", path, err)
	case !fi.IsDir():
		// Replace file/symlink with the desired directory.
		if err := os.RemoveAll(path); err != nil {
			return nil, fmt.Errorf("replace %s with dir: %w", path, err)
		}
		if err := os.Mkdir(path, opts.DirMode); err != nil {
			return nil, fmt.Errorf("create dir %s: %w", path, err)
		}
	}
	return applyDir(path, rel, e, opts)
}

// applyFile materializes a regular file with atomic replace semantics: the
// new content is written to a temp file in the target directory and renamed
// over the target, so a crash never leaves a half-written file.
func applyFile(path, rel, content string, mode os.FileMode, opts Options) ([]Change, error) {
	fi, err := os.Lstat(path)
	switch {
	case os.IsNotExist(err):
		if err := writeAtomic(path, content, mode, opts); err != nil {
			return nil, err
		}
		return []Change{{Path: rel, Action: "create"}}, nil
	case err != nil:
		return nil, fmt.Errorf("stat %s: %w", path, err)
	case fi.Mode()&os.ModeSymlink != 0, fi.IsDir():
		// Drift: expected a file, found a symlink or directory. Remove and
		// rewrite. Never write through the existing entry.
		if err := os.RemoveAll(path); err != nil {
			return nil, fmt.Errorf("remove drifted entry %s: %w", path, err)
		}
		if err := writeAtomic(path, content, mode, opts); err != nil {
			return nil, err
		}
		return []Change{{Path: rel, Action: "replace"}}, nil
	}

	// Regular file: compare content, mode, managed ownership.
	cur, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if bytes.Equal(cur, []byte(content)) &&
		fi.Mode().Perm() == mode.Perm() &&
		ownerMatches(fi, opts) {
		return nil, nil
	}
	if err := writeAtomic(path, content, mode, opts); err != nil {
		return nil, err
	}
	return []Change{{Path: rel, Action: "replace"}}, nil
}

// applySymlink materializes a symlink. Note that targets may legitimately
// point outside the tree (json2dir allows ["link", "/"]); creating a link
// never writes through it.
func applySymlink(path, rel, target string, opts Options) ([]Change, error) {
	fi, err := os.Lstat(path)
	switch {
	case os.IsNotExist(err):
		// fall through to create
	case err != nil:
		return nil, fmt.Errorf("stat %s: %w", path, err)
	case fi.Mode()&os.ModeSymlink != 0:
		cur, err := os.Readlink(path)
		if err != nil {
			return nil, fmt.Errorf("readlink %s: %w", path, err)
		}
		if cur == target {
			return nil, nil
		}
		// Different target: replace.
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove symlink %s: %w", path, err)
		}
		if err := os.Symlink(target, path); err != nil {
			return nil, fmt.Errorf("create symlink %s: %w", path, err)
		}
		return []Change{{Path: rel, Action: "replace"}}, nil
	default:
		// Expected a symlink, found a file or directory.
		if err := os.RemoveAll(path); err != nil {
			return nil, fmt.Errorf("remove drifted entry %s: %w", path, err)
		}
	}

	if err := os.Symlink(target, path); err != nil {
		return nil, fmt.Errorf("create symlink %s: %w", path, err)
	}
	return []Change{{Path: rel, Action: "create"}}, nil
}

// writeAtomic writes content to a fresh temp file in the target directory
// and renames it over path. The temp file is created with O_EXCL and is
// removed on failure.
func writeAtomic(path, content string, mode os.FileMode, opts Options) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".operator2dir-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if _, err := tmp.WriteString(content); err != nil {
		cleanup()
		return fmt.Errorf("write temp file %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temp file %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp file %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, mode.Perm()); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("chmod temp file %s: %w", tmpName, err)
	}
	if opts.UID != nil && opts.GID != nil {
		if err := os.Chown(tmpName, *opts.UID, *opts.GID); err != nil {
			os.Remove(tmpName)
			return fmt.Errorf("chown temp file %s: %w", tmpName, err)
		}
	} else if opts.UID != nil {
		if err := os.Chown(tmpName, *opts.UID, -1); err != nil {
			os.Remove(tmpName)
			return fmt.Errorf("chown temp file %s: %w", tmpName, err)
		}
	} else if opts.GID != nil {
		if err := os.Chown(tmpName, -1, *opts.GID); err != nil {
			os.Remove(tmpName)
			return fmt.Errorf("chown temp file %s: %w", tmpName, err)
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}

// ownerMatches reports whether the on-disk ownership equals the managed one.
// Unmanaged ownership (nil UID and GID) always matches.
func ownerMatches(fi os.FileInfo, opts Options) bool {
	if opts.UID == nil && opts.GID == nil {
		return true
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	if opts.UID != nil && int(st.Uid) != *opts.UID {
		return false
	}
	if opts.GID != nil && int(st.Gid) != *opts.GID {
		return false
	}
	return true
}

// enforceOwner chowns path when the managed ownership is set and differs.
func enforceOwner(path string, fi os.FileInfo, opts Options) error {
	if ownerMatches(fi, opts) {
		return nil
	}
	uid, gid := -1, -1
	if opts.UID != nil {
		uid = *opts.UID
	}
	if opts.GID != nil {
		gid = *opts.GID
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("chown %s: %w", path, err)
	}
	return nil
}
