package constants

import (
	"fmt"
	"strconv"
	"strings"
)

// NoHooksPath is the core.hooksPath under which git runs no client-side hook.
const NoHooksPath = "/dev/null"

// GitHooksPathEnv returns the environment that points git's core.hooksPath at
// dir: command-scope config that overrides repo and worktree config without
// writing to either.
//
// It is for processes that run git against content they did not author. A
// repository's hooks path may be a tracked directory, in which case checking
// out a branch also chooses the hooks that run next.
//
// Command-scope config already carried by getenv is kept, with the new entry
// after it. Earlier hooks path entries are dropped so that repeated
// application does not grow the list.
func GitHooksPathEnv(dir string, getenv func(string) string) map[string]string {
	inherited, err := strconv.Atoi(getenv("GIT_CONFIG_COUNT"))
	if err != nil {
		inherited = 0
	}
	env := make(map[string]string)
	n := 0
	add := func(key, value string) {
		env[fmt.Sprintf("GIT_CONFIG_KEY_%d", n)] = key
		env[fmt.Sprintf("GIT_CONFIG_VALUE_%d", n)] = value
		n++
	}
	for i := 0; i < inherited; i++ {
		key := getenv(fmt.Sprintf("GIT_CONFIG_KEY_%d", i))
		if key != "" && !strings.EqualFold(key, "core.hooksPath") {
			add(key, getenv(fmt.Sprintf("GIT_CONFIG_VALUE_%d", i)))
		}
	}
	add("core.hooksPath", dir)
	env["GIT_CONFIG_COUNT"] = strconv.Itoa(n)
	return env
}
