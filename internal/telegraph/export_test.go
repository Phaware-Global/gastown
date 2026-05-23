package telegraph

// Test-only export of the URL parser. Lives in *_test.go so it's excluded
// from the production build surface — external callers of telegraph
// still can't depend on parseGitHubRepoURL.
var ParseGitHubRepoURLForTest = parseGitHubRepoURL
