package agentproc

import (
	"reflect"
	"testing"
)

func TestTranslateAddDirsForSandbox(t *testing.T) {
	cases := []struct {
		name    string
		addDirs []string
		cwd     string
		want    []string
	}{
		{
			name:    "nil_input",
			addDirs: nil,
			cwd:     "/data/worktrees/abc",
			want:    nil,
		},
		{
			name:    "empty_cwd_drops_everything",
			addDirs: []string{"/some/path"},
			cwd:     "",
			want:    []string{},
		},
		{
			name:    "absolute_under_cwd_translates",
			addDirs: []string{"/data/worktrees/abc/knowledge-base"},
			cwd:     "/data/worktrees/abc",
			want:    []string{"/work/knowledge-base"},
		},
		{
			name:    "nested_subpath_preserved",
			addDirs: []string{"/data/worktrees/abc/repos/project1/src"},
			cwd:     "/data/worktrees/abc",
			want:    []string{"/work/repos/project1/src"},
		},
		{
			name:    "outside_cwd_dropped",
			addDirs: []string{"/etc/passwd"},
			cwd:     "/data/worktrees/abc",
			want:    []string{},
		},
		{
			name:    "mixed_in_and_out",
			addDirs: []string{"/data/worktrees/abc/kb", "/etc/passwd", "/data/worktrees/abc/repos"},
			cwd:     "/data/worktrees/abc",
			want:    []string{"/work/kb", "/work/repos"},
		},
		{
			name:    "empty_entries_skipped",
			addDirs: []string{"/data/worktrees/abc/kb", "", "/data/worktrees/abc/repos"},
			cwd:     "/data/worktrees/abc",
			want:    []string{"/work/kb", "/work/repos"},
		},
		{
			name:    "cwd_itself_becomes_work",
			addDirs: []string{"/data/worktrees/abc"},
			cwd:     "/data/worktrees/abc",
			want:    []string{"/work"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := translateAddDirsForSandbox(c.addDirs, c.cwd)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("translateAddDirsForSandbox(%v, %q) = %v, want %v",
					c.addDirs, c.cwd, got, c.want)
			}
		})
	}
}

// TestTranslateAddDirsForSandbox_DropsSymlinkEscapes pins the
// defensive drop for paths that look in-tree but resolve outside.
// Pure filepath.Rel-level check — doesn't follow symlinks (the
// sandbox bind-mount handles those at the filesystem boundary).
func TestTranslateAddDirsForSandbox_DropsSymlinkEscapes(t *testing.T) {
	// `..` in the input — filepath.Rel would resolve outside cwd.
	got := translateAddDirsForSandbox(
		[]string{"/data/worktrees/abc/../other-worktree/sneaky"},
		"/data/worktrees/abc",
	)
	if len(got) != 0 {
		t.Errorf("got %v, want empty (the path resolves outside cwd)", got)
	}
}
