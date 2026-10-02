package github

import (
	"strings"
	"testing"
)

var rules = PushRules{ForbiddenPaths: []string{".github/**", "CODEOWNERS", "docs/CODEOWNERS"}, MaxDiffLines: 800}

func rulesOf(vs []Violation) string {
	var s []string
	for _, v := range vs {
		s = append(s, v.Rule+":"+v.Detail)
	}
	return strings.Join(s, "; ")
}

func TestValidatePush(t *testing.T) {
	ok := Comparison{Files: []ChangedFile{{Filename: "src/app.ts", Additions: 40, Deletions: 2}}}
	if vs := ValidatePush(17, BranchName(17, "add-login"), ok, rules); len(vs) != 0 {
		t.Fatalf("clean push rejected: %s", rulesOf(vs))
	}

	cases := []struct {
		name   string
		branch string
		files  []ChangedFile
		want   string
	}{
		{"wrong issue", "agent/18-add-login", ok.Files, "branch"},
		{"not agent branch", "feature/x", ok.Files, "branch"},
		{"uppercase slug", "agent/17-Add", ok.Files, "branch"},
		{"workflow", "agent/17-x", []ChangedFile{{Filename: ".github/workflows/ci.yml"}}, "forbidden_path:.github/workflows/ci.yml"},
		{"github dir root", "agent/17-x", []ChangedFile{{Filename: ".github/CODEOWNERS"}}, "forbidden_path"},
		{"codeowners", "agent/17-x", []ChangedFile{{Filename: "CODEOWNERS"}}, "forbidden_path:CODEOWNERS"},
		{"renamed out of forbidden", "agent/17-x", []ChangedFile{{Filename: "ci.yml", PreviousFilename: ".github/workflows/ci.yml"}}, "forbidden_path"},
		{"too big", "agent/17-x", []ChangedFile{{Filename: "a.go", Additions: 500}, {Filename: "b.go", Additions: 200, Deletions: 101}}, "diff_size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rulesOf(ValidatePush(17, tc.branch, Comparison{Files: tc.files}, rules))
			if !strings.Contains(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	// A similarly named path outside the forbidden tree is fine.
	if vs := ValidatePush(17, "agent/17-x", Comparison{Files: []ChangedFile{{Filename: ".githubrc"}, {Filename: "src/CODEOWNERS.md"}}}, rules); len(vs) != 0 {
		t.Fatalf("false positive: %s", rulesOf(vs))
	}
}
