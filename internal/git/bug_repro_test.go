package git

import "testing"

// TestSameGitRemoteURLFalsePositiveOnSSHPort is a regression test for a bug
// found via CGPT metamorphic testing (see BUGS-FOUND.md): normalizeGitRemoteURL
// used to treat any "git@host:rest" shape as SCP-style syntax and rewrite the
// first ':' to '/'. An ssh://user@host:PORT/path URL also contains
// "git@host:" after the "ssh://" prefix is stripped, so its port number got
// folded into the path instead of being dropped — letting two genuinely
// different remotes normalize to the same string. That was a false positive
// for sameGitRemoteURL, the function backing the security guard in
// Git.RefuseForkBackedDefaultPush / Git.ForkBackedRemote.
func TestSameGitRemoteURLFalsePositiveOnSSHPort(t *testing.T) {
	sshWithPort := "ssh://git@example.com:2222/org/repo.git"
	httpsDifferentPath := "https://example.com/2222/org/repo"

	if got := sameGitRemoteURL(sshWithPort, httpsDifferentPath); got {
		t.Fatalf("sameGitRemoteURL(%q, %q) = true, but these are different remotes\n"+
			"normalizeGitRemoteURL(%q) = %q\nnormalizeGitRemoteURL(%q) = %q",
			sshWithPort, httpsDifferentPath,
			sshWithPort, normalizeGitRemoteURL(sshWithPort),
			httpsDifferentPath, normalizeGitRemoteURL(httpsDifferentPath))
	}

	// The same two remotes, still different, must not collide once the port
	// is correctly stripped instead of merged into the path.
	if got, want := normalizeGitRemoteURL(sshWithPort), "example.com/org/repo"; got != want {
		t.Errorf("normalizeGitRemoteURL(%q) = %q, want %q", sshWithPort, got, want)
	}
}

// TestNormalizeGitRemoteURLUppercaseScheme is a regression test for a bug
// found alongside the one above: a URL with an uppercase scheme (rare but
// valid — schemes are case-insensitive per RFC 3986) failed to normalize the
// same as its lowercase equivalent, because the TrimPrefix calls were
// case-sensitive and only stripped "https://" (not "HTTPS://") before the
// final ToLower() ran too late to help the prefix match.
func TestNormalizeGitRemoteURLUppercaseScheme(t *testing.T) {
	lower := normalizeGitRemoteURL("https://github.com/foo/bar.git")
	upper := normalizeGitRemoteURL("HTTPS://GitHub.com/Foo/Bar.git")
	if lower != upper {
		t.Errorf("normalize(%q)=%q != normalize(%q)=%q, want equal (differ only by scheme case)",
			"https://github.com/foo/bar.git", lower, "HTTPS://GitHub.com/Foo/Bar.git", upper)
	}
}

// TestNormalizeGitRemoteURLSCPStyleStillWorks guards the still-supported
// genuine SCP-style shorthand ("git@host:path", no scheme) against
// regressing while fixing the ssh://-with-port collision above.
func TestNormalizeGitRemoteURLSCPStyleStillWorks(t *testing.T) {
	got := normalizeGitRemoteURL("git@github.com:org/repo.git")
	want := normalizeGitRemoteURL("https://github.com/org/repo")
	if got != want {
		t.Errorf("SCP-style and https normalize differently: %q != %q", got, want)
	}
}
