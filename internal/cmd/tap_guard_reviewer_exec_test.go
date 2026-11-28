package cmd

import "testing"

func TestUnwrappedPRCode(t *testing.T) {
	blocked := []string{
		"go test ./...",
		"cd internal && go test -run X .",
		"CGO_CPPFLAGS=-I/x go test ./...",
		"env -u TMUX go test ./...",
		"timeout 600 go test ./...",
		"go -C tools run .",
		"/usr/local/go/bin/go generate ./...",
		"git diff HEAD; make test",
		"npm test",
		"npm --prefix web run build",
		"pytest -q",
		"python3 -m pytest",
		"cargo test",
		"./scripts/check.sh",
		"bash -c 'go test ./...'",
		"echo $(go test ./...)",
		"gt reviewer exec -- true && go test ./...",
		"go test ./... | tee out.log",
	}
	allowed := []string{
		"gt reviewer exec -- go test ./...",
		"gt reviewer exec go test ./...",
		"CGO_CPPFLAGS=-I/x gt reviewer exec -- go test ./...",
		"gt reviewer exec -- sh -c 'cd x && go test ./...'",
		"cd x && gt reviewer exec -- make test | tail -5",
		"go vet ./...",
		"go list ./...",
		"git diff abc..HEAD -- Makefile",
		"grep -rn 'go test' .",
		"echo \"run go test later\"",
		"codegraph explore 'make test'",
		"gt reviewer post --pr 1 --findings f.json",
	}
	for _, command := range blocked {
		if _, found := unwrappedPRCode(command); !found {
			t.Errorf("allowed %q", command)
		}
	}
	for _, command := range allowed {
		if what, found := unwrappedPRCode(command); found {
			t.Errorf("blocked %q (as %q)", command, what)
		}
	}
}
