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

// Package tree implements the json2dir conversion scheme: parsing and
// validating the CR tree field into a typed model, and materializing that
// model onto the filesystem with path-escape protection and atomic writes.
package tree

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// EntryKind enumerates the json2dir node types.
type EntryKind string

const (
	KindDir     EntryKind = "dir"
	KindFile    EntryKind = "file"
	KindSymlink EntryKind = "symlink"
	KindScript  EntryKind = "script"
)

// Entry is one node of the parsed tree.
type Entry struct {
	Kind EntryKind `json:"kind"`

	// Content is the file body for KindFile and KindScript.
	Content string `json:"content,omitempty"`

	// Target is the symlink target for KindSymlink.
	Target string `json:"target,omitempty"`

	// Children holds the entries of KindDir, keyed by entry name.
	Children map[string]*Entry `json:"children,omitempty"`
}

// ValidateEntryName enforces the json2dir path-safety rules for a single
// path segment. Nested structure must come from nested objects, never from
// multi-segment keys.
func ValidateEntryName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("empty entry name")
	case strings.ContainsRune(name, '/'):
		return fmt.Errorf("entry name %q must be a single path segment (contains '/')", name)
	case name == "." || name == "..":
		return fmt.Errorf("entry name %q is a special path segment", name)
	case strings.ContainsRune(name, 0):
		return fmt.Errorf("entry name %q contains a NUL byte", name)
	}
	return nil
}

// Parse decodes raw JSON (the CR spec tree field) into a validated Entry
// tree. It enforces:
//   - the root must be a JSON object;
//   - keys are single, non-special path segments (see ValidateEntryName);
//   - values are objects (dirs), strings (files), or two-element arrays
//     ["link"|"script", "<string>"].
//
// Parse never touches the filesystem; materialize-time re-validation lives
// in Apply.
func Parse(raw []byte) (*Entry, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("tree is empty")
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("tree root must be a JSON object: %w", err)
	}
	e, err := parseDir(root)
	if err != nil {
		return nil, err
	}
	e.Kind = KindDir
	return e, nil
}

func parseDir(m map[string]json.RawMessage) (*Entry, error) {
	e := &Entry{Kind: KindDir, Children: make(map[string]*Entry, len(m))}
	for name, raw := range m {
		if err := ValidateEntryName(name); err != nil {
			return nil, err
		}
		child, err := parseValue(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		e.Children[name] = child
	}
	return e, nil
}

func parseValue(raw json.RawMessage) (*Entry, error) {
	// Try object (directory) first: the common case.
	if raw[0] == '{' {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("invalid object: %w", err)
		}
		return parseDir(m)
	}
	// String: regular file.
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("invalid string: %w", err)
		}
		return &Entry{Kind: KindFile, Content: s}, nil
	}
	// Array: ["link", target] or ["script", content].
	if raw[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, fmt.Errorf("invalid array: %w", err)
		}
		if len(arr) != 2 {
			return nil, fmt.Errorf("array must have exactly two elements")
		}
		var kind string
		if err := json.Unmarshal(arr[0], &kind); err != nil {
			return nil, fmt.Errorf("first array element must be a string: %w", err)
		}
		var val string
		if err := json.Unmarshal(arr[1], &val); err != nil {
			return nil, fmt.Errorf("second array element must be a string: %w", err)
		}
		switch kind {
		case "link":
			return &Entry{Kind: KindSymlink, Target: val}, nil
		case "script":
			return &Entry{Kind: KindScript, Content: val}, nil
		default:
			return nil, fmt.Errorf("array tag must be \"link\" or \"script\", got %q", kind)
		}
	}
	return nil, fmt.Errorf("value must be an object, a string, or a [tag, value] array")
}

// Hash returns a deterministic hash of the canonicalized tree: encoding/json
// serializes map keys in sorted order, so equivalent trees (modulo key order)
// hash identically.
func Hash(e *Entry) string {
	b, err := json.Marshal(e)
	if err != nil {
		// Entry contains only strings and maps; marshal cannot fail.
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
