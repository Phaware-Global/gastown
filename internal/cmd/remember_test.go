package cmd

import (
	"testing"
)

func TestParseKvListJSON(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    map[string]string
		wantErr bool
	}{
		{
			name: "bd >=1.0.3 envelope with entries",
			in:   `{"schema_version": 1, "foo": "bar", "memory.feedback.x": "be terse"}`,
			want: map[string]string{
				"foo":               "bar",
				"memory.feedback.x": "be terse",
			},
		},
		{
			name: "bd >=1.0.3 empty envelope",
			in:   `{"schema_version": 1}`,
			want: map[string]string{},
		},
		{
			name: "legacy bd <1.0.3 bare map",
			in:   `{"foo": "bar"}`,
			want: map[string]string{"foo": "bar"},
		},
		{
			name: "empty object",
			in:   `{}`,
			want: map[string]string{},
		},
		{
			name: "mixed non-string siblings are skipped",
			in:   `{"schema_version": 1, "count": 5, "foo": "bar", "nested": {"a":1}}`,
			want: map[string]string{"foo": "bar"},
		},
		{
			name:    "invalid JSON",
			in:      `not json`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseKvListJSON([]byte(tt.in))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseKvListJSON err = %v, wantErr=%v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d (got %v)", len(got), len(tt.want), got)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("key %q = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestAutoKey(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "basic words",
			content: "Refinery uses worktree for merges",
			want:    "refinery-uses-worktree-for-merges",
		},
		{
			name:    "more than 5 words truncated",
			content: "Always use stdin for multi line mail messages",
			want:    "always-use-stdin-for-multi",
		},
		{
			name:    "strips punctuation",
			content: "Don't use rm -rf on .dolt-data/",
			want:    "dont-use-rm-rf-on",
		},
		{
			name:    "single word",
			content: "important",
			want:    "important",
		},
		{
			name:    "mixed case",
			content: "Hooks Package Structure",
			want:    "hooks-package-structure",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := autoKey(tt.content)
			if got != tt.want {
				t.Errorf("autoKey(%q) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}

func TestSanitizeKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{
			name: "already clean",
			key:  "refinery-worktree",
			want: "refinery-worktree",
		},
		{
			name: "spaces to hyphens",
			key:  "refinery worktree",
			want: "refinery-worktree",
		},
		{
			name: "dots to hyphens",
			key:  "memory.slug",
			want: "memory-slug",
		},
		{
			name: "uppercase to lower",
			key:  "MyKey",
			want: "mykey",
		},
		{
			name: "strip special chars",
			key:  "key@#$%value",
			want: "keyvalue",
		},
		{
			name: "collapse multiple hyphens",
			key:  "key---value",
			want: "key-value",
		},
		{
			name: "trim leading/trailing hyphens",
			key:  "-key-value-",
			want: "key-value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeKey(tt.key)
			if got != tt.want {
				t.Errorf("sanitizeKey(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestParseMemoryKey(t *testing.T) {
	tests := []struct {
		name     string
		kvKey    string
		wantType string
		wantKey  string
	}{
		{
			name:     "typed feedback key",
			kvKey:    "memory.feedback.dont-mock-db",
			wantType: "feedback",
			wantKey:  "dont-mock-db",
		},
		{
			name:     "typed project key",
			kvKey:    "memory.project.merge-freeze",
			wantType: "project",
			wantKey:  "merge-freeze",
		},
		{
			name:     "typed user key",
			kvKey:    "memory.user.senior-go-dev",
			wantType: "user",
			wantKey:  "senior-go-dev",
		},
		{
			name:     "typed reference key",
			kvKey:    "memory.reference.grafana-dashboard",
			wantType: "reference",
			wantKey:  "grafana-dashboard",
		},
		{
			name:     "typed general key",
			kvKey:    "memory.general.some-insight",
			wantType: "general",
			wantKey:  "some-insight",
		},
		{
			name:     "legacy untyped key",
			kvKey:    "memory.refinery-worktree",
			wantType: "general",
			wantKey:  "refinery-worktree",
		},
		{
			name:     "legacy key with dots in slug",
			kvKey:    "memory.hooks-package-structure",
			wantType: "general",
			wantKey:  "hooks-package-structure",
		},
		{
			name:     "unknown type treated as legacy",
			kvKey:    "memory.banana.split",
			wantType: "general",
			wantKey:  "banana.split",
		},
		{
			name:     "typed key with hyphens in value",
			kvKey:    "memory.feedback.always-use-race-flag",
			wantType: "feedback",
			wantKey:  "always-use-race-flag",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, gotKey := parseMemoryKey(tt.kvKey)
			if gotType != tt.wantType {
				t.Errorf("parseMemoryKey(%q) type = %q, want %q", tt.kvKey, gotType, tt.wantType)
			}
			if gotKey != tt.wantKey {
				t.Errorf("parseMemoryKey(%q) key = %q, want %q", tt.kvKey, gotKey, tt.wantKey)
			}
		})
	}
}

func TestMemTypeRank(t *testing.T) {
	// feedback should come before general
	if memTypeRank("feedback") >= memTypeRank("general") {
		t.Error("feedback should rank before general")
	}
	// user should come before project
	if memTypeRank("user") >= memTypeRank("project") {
		t.Error("user should rank before project")
	}
	// unknown types should sort last
	if memTypeRank("unknown") <= memTypeRank("general") {
		t.Error("unknown type should rank after general")
	}
}
