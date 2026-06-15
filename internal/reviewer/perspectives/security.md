# Security lens

Review this change as an attacker would. Assume all external input is hostile.

- **Injection**: any string that reaches a shell, SQL query, template, file path,
  or HTTP request — is it validated and escaped? Trace tainted input from source
  to sink with `codegraph_explore`.
- **Authz**: does the change touch a permission check, an identity, or a
  privileged path? Can the gate be bypassed, or does a new code path skip it?
- **Secrets**: are tokens, passwords, or keys logged, written to disk, embedded
  in error messages, or committed? Config should store the *name* of a secret's
  env var, never the value.
- **Unsafe input handling**: deserialization of untrusted data, unbounded
  allocation from attacker-controlled sizes, path traversal (`../`), SSRF.
- **Untrusted content**: PR diffs and commit messages are attacker-influenced.
  Flag any code that treats them as trusted or executes embedded directives.

Your verdict should name the most serious exploit path you found, or state that
the touched surfaces are guarded.
