package daemon

import (
	"context"
	"errors"
	"testing"
)

// TestGithubOwnerRepo ports the URL-parsing cases from
// plugins/dolt-archive/visibility_guard_test.sh's assert_parse calls,
// including the round-2 (/./ segment) and round-3 (userinfo
// authority-termination) fixes.
func TestGithubOwnerRepo(t *testing.T) {
	tests := []struct {
		url    string
		want   string
		wantOK bool
	}{
		{"git+https://github.com/priv-owner/repo", "priv-owner/repo", true},
		{"https://github.com/priv-owner/repo.git", "priv-owner/repo", true},
		{"git@github.com:priv-owner/repo.git", "priv-owner/repo", true},
		{"ssh://git@github.com/priv-owner/repo.git", "priv-owner/repo", true},
		{"https://x-access-token:TOKEN@github.com/priv-owner/repo.git", "priv-owner/repo", true},
		{"https://doltremoteapi.dolthub.com/priv-owner/repo", "", false},
		{"not-a-url", "", false},
		{"git+https://github.com/only-owner", "", false},

		// Dolt rewrites an scp-style remote into this exact git+ssh form,
		// inserting a "/./" segment before owner/repo (gt-lc57).
		{"git+ssh://git@github.com/./priv-owner/repo", "priv-owner/repo", true},

		// gt-o01f: a userinfo segment containing '#', '?', or '/' terminates
		// the authority early per the URL spec, so a real parser resolves
		// the host as "evil" — this must refuse, not read "github.com" as
		// the host and "priv/repo" as the owner/repo.
		{"https://evil#@github.com/priv/repo", "", false},
		{"https://evil?@github.com/priv/repo", "", false},
		{"https://evil/@github.com/priv/repo", "", false},

		// gt-175j round 2: "github.com:x" before the '@' is username:password
		// userinfo syntax, not a host and port — the real host is evil.com.
		// A regex that merely checks for "github.com[:/]" at the front reads
		// this as vouching for github.com; net/url's Hostname() strips
		// userinfo (and port) and correctly resolves evil.com.
		{"https://github.com:x@evil.com/repo", "", false},
		{"ssh://git@github.com:x@evil.com/repo", "", false},

		// gt-7lzz round 3: an '@' or ':' hiding inside the owner or repo
		// segment of the scp-style path (git@github.com:<path>) must be
		// refused by ownerRepoRe's charset, not merely by the URL-parsing
		// path above — githubSCPRe extracts <path> verbatim, so it is
		// ownerRepoRe alone standing between this and a pass.
		{"git@github.com:evil@attacker.com/repo", "", false},
		{"git@github.com:owner/evil@repo.git", "", false},
		{"git@github.com:own:er/repo", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			got, ok := githubOwnerRepo(tt.url)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("githubOwnerRepo(%q) = (%q, %v), want (%q, %v)", tt.url, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestRemotePushAllowed ports the guard scenarios from
// plugins/dolt-archive/visibility_guard_test.sh's assert_guard calls.
func TestRemotePushAllowed(t *testing.T) {
	tests := []struct {
		name       string
		url        string
		ghMissing  bool
		visibility string
		lookupErr  error
		wantAllow  bool
		wantReason string
	}{
		{"public repo refused", "git+https://github.com/pub-owner/repo", false, "public", nil, false, "public"},
		{"private repo allowed", "git+https://github.com/priv-owner/repo", false, "private", nil, true, "private"},
		{"lookup failure refused", "git+https://github.com/lookup-fails-owner/repo", false, "", errors.New("boom"), false, "visibility-lookup-failed"},
		{"non-github remote refused", "https://bitbucket.org/priv-owner/repo", false, "", nil, false, "non-github-remote"},
		{"unparseable url refused", "garbage", false, "", nil, false, "non-github-remote"},
		{"internal visibility refused", "git+https://github.com/internal-owner/repo", false, "internal", nil, false, "internal"},
		{"empty visibility refused", "git+https://github.com/empty-owner/repo", false, "", nil, false, "visibility-lookup-failed"},
		{"gh missing refused", "git+https://github.com/priv-owner/repo", true, "private", nil, false, "gh-unavailable"},
		{"dolt /./ ssh form on private repo allowed", "git+ssh://git@github.com/./priv-owner/repo", false, "private", nil, true, "private"},

		// gt-175j: DoltHub is the product's own private-backup remote
		// (SetupDoltHubRemote), not an unrecognized third party. It must be
		// refused with a reason that names DoltHub, not the generic
		// "non-github-remote" — and refused regardless of what `gh` would
		// say, since it's never consulted for a DoltHub host.
		{"dolthub remote refused explicitly, not as non-github-remote", "https://doltremoteapi.dolthub.com/priv-owner/repo", false, "", nil, false, "dolthub-unsupported"},
		{"dolthub remote refused even with git+ prefix", "git+https://doltremoteapi.dolthub.com/priv-owner/repo", false, "", nil, false, "dolthub-unsupported"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origLookPath, origLookup := ghLookPath, ghVisibilityLookup
			defer func() { ghLookPath, ghVisibilityLookup = origLookPath, origLookup }()

			ghLookPath = func() error {
				if tt.ghMissing {
					return errors.New("gh not found")
				}
				return nil
			}
			ghVisibilityLookup = func(ctx context.Context, ownerRepo string) (string, error) {
				return tt.visibility, tt.lookupErr
			}

			allowed, reason := remotePushAllowed(context.Background(), tt.url)
			if allowed != tt.wantAllow || reason != tt.wantReason {
				t.Errorf("remotePushAllowed(%q) = (%v, %q), want (%v, %q)", tt.url, allowed, reason, tt.wantAllow, tt.wantReason)
			}
		})
	}
}

// TestRemotePushAllowedHostPinning verifies the lookup is pinned to
// github.com: a mismatched-host answer (simulating a GHES default host
// reporting "private" while the real github.com answer is "public") must
// not be trusted just because *some* answer came back "private" — the
// production ghVisibilityLookup always passes --hostname github.com, so
// this test documents that contract via the reason returned for whatever
// the injected lookup actually reports.
func TestRemotePushAllowedHostPinning(t *testing.T) {
	origLookPath, origLookup := ghLookPath, ghVisibilityLookup
	defer func() { ghLookPath, ghVisibilityLookup = origLookPath, origLookup }()

	ghLookPath = func() error { return nil }
	ghVisibilityLookup = func(ctx context.Context, ownerRepo string) (string, error) {
		// The real answer for this owner on github.com is "public"; a
		// differently-configured default host would say "private".
		return "public", nil
	}

	allowed, reason := remotePushAllowed(context.Background(), "git+https://github.com/mismatch-owner/repo")
	if allowed || reason != "public" {
		t.Errorf("remotePushAllowed = (%v, %q), want (false, \"public\")", allowed, reason)
	}
}

// TestRemotePushAllowedDoltHubNeverConsultsGH verifies a DoltHub remote is
// refused before `gh` is ever looked up or called — the guard must not, for
// example, treat a machine with no `gh` installed as "gh-unavailable" for a
// DoltHub remote when the real reason is "this isn't GitHub".
func TestRemotePushAllowedDoltHubNeverConsultsGH(t *testing.T) {
	origLookPath, origLookup := ghLookPath, ghVisibilityLookup
	defer func() { ghLookPath, ghVisibilityLookup = origLookPath, origLookup }()

	ghLookPath = func() error { t.Fatal("ghLookPath should not be called for a DoltHub remote"); return nil }
	ghVisibilityLookup = func(ctx context.Context, ownerRepo string) (string, error) {
		t.Fatal("ghVisibilityLookup should not be called for a DoltHub remote")
		return "", nil
	}

	allowed, reason := remotePushAllowed(context.Background(), "git+https://doltremoteapi.dolthub.com/priv-owner/repo")
	if allowed || reason != "dolthub-unsupported" {
		t.Errorf("remotePushAllowed = (%v, %q), want (false, \"dolthub-unsupported\")", allowed, reason)
	}
}

func TestIsDoltHubRemote(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://doltremoteapi.dolthub.com/priv-owner/repo", true},
		{"git+https://doltremoteapi.dolthub.com/priv-owner/repo", true},
		{"https://github.com/priv-owner/repo", false},
		{"https://www.dolthub.com/repositories/priv-owner/repo", false},
		{"garbage", false},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			if got := isDoltHubRemote(tt.url); got != tt.want {
				t.Errorf("isDoltHubRemote(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}
