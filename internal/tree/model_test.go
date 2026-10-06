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
	"testing"
)

const exampleTree = `{
  "greeting": "Hello, world!",
  "dir": {
    "subfile": "Content.\n",
    "subdir": {}
  },
  "symlink": ["link", "/"],
  "script": ["script", "#!/bin/sh\necho Howdy!"]
}`

func TestParseValid(t *testing.T) {
	e, err := Parse([]byte(exampleTree))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if e.Kind != KindDir {
		t.Fatalf("root kind = %q, want dir", e.Kind)
	}
	if len(e.Children) != 4 {
		t.Fatalf("root children = %d, want 4", len(e.Children))
	}
	if got := e.Children["greeting"]; got == nil || got.Kind != KindFile || got.Content != "Hello, world!" {
		t.Errorf("greeting = %+v", got)
	}
	dir := e.Children["dir"]
	if dir == nil || dir.Kind != KindDir || len(dir.Children) != 2 {
		t.Fatalf("dir = %+v", dir)
	}
	if sub := dir.Children["subdir"]; sub == nil || len(sub.Children) != 0 {
		t.Errorf("subdir = %+v, want empty dir", sub)
	}
	if got := e.Children["symlink"]; got == nil || got.Kind != KindSymlink || got.Target != "/" {
		t.Errorf("symlink = %+v", got)
	}
	if got := e.Children["script"]; got == nil || got.Kind != KindScript || got.Content != "#!/bin/sh\necho Howdy!" {
		t.Errorf("script = %+v", got)
	}
}

func TestParseInvalid(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"root not object", `["link", "/"]`},
		{"root string", `"hello"`},
		{"root number", `42`},
		{"empty tree", ``},
		{"multi-segment key", `{"a/b": "x"}`},
		{"dot key", `{".": "x"}`},
		{"dotdot key", `{"..": "x"}`},
		{"array too short", `{"s": ["link"]}`},
		{"array too long", `{"s": ["link", "a", "b"]}`},
		{"unknown array tag", `{"s": ["fif", "x"]}`},
		{"array second elem not string", `{"s": ["link", 42]}`},
		{"array first elem not string", `{"s": [42, "x"]}`},
		{"nested invalid", `{"dir": {"a/b": "x"}}`},
		{"number value", `{"n": 1}`},
		{"bool value", `{"b": true}`},
		{"null value", `{"n": null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.raw)); err == nil {
				t.Errorf("Parse(%s) = nil error, want error", tc.raw)
			}
		})
	}
}

func TestHashStableAcrossKeyOrder(t *testing.T) {
	a, err := Parse([]byte(`{"x": "1", "y": {"b": "2", "a": "3"}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse([]byte(`{"y": {"a": "3", "b": "2"}, "x": "1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if Hash(a) != Hash(b) {
		t.Errorf("hash differs for equivalent trees")
	}
	c, err := Parse([]byte(`{"x": "1", "y": {"b": "2", "a": "4"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if Hash(a) == Hash(c) {
		t.Errorf("hash equal for different trees")
	}
}
