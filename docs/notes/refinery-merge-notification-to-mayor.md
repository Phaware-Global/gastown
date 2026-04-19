# Mayor Not Notified of Completed Merges

## Problem

When the refinery merges work, the notification chain is:

```
Refinery merges → MERGED mail to witness → witness cleans up polecat
                → nudge to mayor (ephemeral, easily lost)
```

The mayor receives only a `gt nudge` (`internal/refinery/engineer.go:1209`),
which is ephemeral — if the mayor is busy or in a different context, the nudge
is lost. There is no persistent inbox entry. The mayor never learns about
completed work unless it actively polls.

This means the human (interacting via the mayor) has no reliable notification
that tasks have been completed and merged.

## Impact

- Mayor cannot report work completion to the human
- No audit trail of merge events visible to the mayor
- Human must check git log or GitHub to discover landed work

## Workaround

For the jira_claude_channel rig's PR-based workflow, the formula overlay adds
`gt mail send mayor/` in the post-merge cleanup step. This only covers that
specific rig and only the human-merge path.

## Fork Fix

Change `internal/refinery/engineer.go:1209` from `gt nudge` to `gt mail send`:

```go
// Before:
nudgeMsg := fmt.Sprintf("MERGED: %s issue=%s branch=%s", mr.ID, mr.SourceIssue, mr.Branch)
nudgeCmd := exec.Command("gt", "nudge", "mayor/", nudgeMsg)

// After:
mailCmd := exec.Command("gt", "mail", "send", "mayor/",
    "-s", fmt.Sprintf("MERGED: %s", mr.SourceIssue),
    "-m", fmt.Sprintf("MR: %s\nIssue: %s\nBranch: %s\nRig: %s",
        mr.ID, mr.SourceIssue, mr.Branch, e.rig.Name))
```

Mail send automatically nudges the recipient, so the push-notification behavior
is preserved while adding the persistent inbox entry.
