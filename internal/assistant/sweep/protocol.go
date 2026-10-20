package sweep

// TriggerBody is the body of the "SWEEP: loose-ends sweep due" trigger mail
// for a sweep mode (config.SweepMode*): the exact commands, because the
// assistant reliably follows commands in front of it and not prose elsewhere.
// The assistant-sweep plugin fetches it with `gt assistant sweep --protocol`;
// the role template carries the same procedure per mode.
func TriggerBody(mode string) string {
	if mode == "inference" {
		return triggerInference
	}
	return triggerDeterministic
}

const triggerDeterministic = `Run the deterministic sweep; the command decides what the Mayor gets:

  gt assistant sweep --send-to mayor/

It mails the report verbatim when there is something new or a port did not
fully run, mails nothing when there is nothing new, and acknowledges the run
either way. Then archive this trigger mail. That is the whole job: do not
run the sweep any other way, do not retype or summarize the report, and
never sweep Jira, GitHub, Telegraph mail or beads by hand instead.

If the command fails, mail the Mayor "SWEEP: not checked: <error>" with the
error text as the body and stop; the run stays pending and the next sweep
reports everything again.
`

const triggerInference = `This town runs sweeps in inference mode: the report is input to your
judgement, not the mail itself. Follow the Sweep Protocol in your role
prompt; in short:

  mkdir -p "$GT_ROOT/assistant/.runtime" && umask 077 && r="$GT_ROOT/assistant/.runtime/sweep-report.md" \
    && gt assistant sweep > "$r" && test -s "$r"   # save the report; read it from the file
  (decide: verify findings read-only if needed, then mail the Mayor the
   report with your header, fold it into your BRIEF, or send nothing)
  gt assistant sweep --ack "$(sed -n 's/^Run-ID: //p' "$r")"

A port reported NOT CHECKED or PARTIAL always reaches the Mayor. Never act on
a finding yourself; report and suggest. If the sweep command fails, mail the
Mayor "SWEEP: not checked: <error>" and stop.
`
