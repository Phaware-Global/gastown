package cmd

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/workerca"
)

// gt worker — manage the enrolled socket-execution worker fleet
// (docs/design/remote-polecat-execution-socket.md §3.1).
var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Manage enrolled remote-execution workers",
	Long: `Manage the machines that run polecats for rigs using the socket
execution backend.

Enrollment is a one-time, operator-driven exchange that gives a worker machine
a certificate signed by this town's WORKER CA (distinct from the proxy CA, so
a stolen machine cert can never mint polecat identities). After enrolling, a
rig can select that machine with:

  "execution": {
    "backend": "socket",
    "socket": { "address": "10.0.1.42:9878",
                "tls": { "mode": "auto", "worker_name": "<name>" } }
  }`,
}

var workerEnrollCmd = &cobra.Command{
	Use:   "enroll <name>",
	Short: "Enroll a worker machine (signs its machine certificate)",
	Long: `Enroll a worker machine into this town's worker fleet.

On the WORKER, first start enrollment mode with a join token you choose:

  gt-worker-client enroll --listen 0.0.0.0:9878 --join-token <token> \
      --tls-dir /etc/gt-worker/tls

Then, here on the orchestrator:

  gt worker enroll gpu-box-1 --address 10.0.1.42:9878 --join-token <token>

The worker generates its machine keypair locally (the private key never
leaves it) and sends a CSR; this command signs it with the worker CA and
returns the machine certificate plus the CA certificates. Carry the join
token out-of-band — it is what authenticates the bootstrap.

Use --generate-token to have gt mint a token for you to copy to the worker.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		address, _ := cmd.Flags().GetString("address")
		token, _ := cmd.Flags().GetString("join-token")
		generate, _ := cmd.Flags().GetBool("generate-token")

		if generate {
			if token != "" {
				return fmt.Errorf("--generate-token and --join-token are mutually exclusive")
			}
			t, err := workerca.NewJoinToken()
			if err != nil {
				return err
			}
			fmt.Printf("Join token: %s\n\n", t)
			fmt.Printf("Run this on the worker, then re-run enroll with --join-token:\n")
			fmt.Printf("  gt-worker-client enroll --listen <addr> --join-token %s --tls-dir /etc/gt-worker/tls\n", t)
			return nil
		}
		if address == "" {
			return fmt.Errorf("--address is required (the worker's enrollment listener, host:port)")
		}
		if token == "" {
			return fmt.Errorf("--join-token is required (use --generate-token to mint one)")
		}

		ca, err := openWorkerCA(cmd)
		if err != nil {
			return err
		}
		w, err := ca.EnrollWorker(context.Background(), name, address, token)
		if err != nil {
			return err
		}
		fmt.Printf("Enrolled %s at %s\n", w.Name, w.Address)
		fmt.Printf("  serial:    %s\n", w.Serial)
		fmt.Printf("  valid to:  %s\n", w.NotAfter.Format(time.RFC3339))
		fmt.Printf("\nSelect it from a rig's settings/config.json:\n")
		fmt.Printf("  \"execution\": { \"backend\": \"socket\", \"socket\": { \"address\": %q, \"tls\": { \"mode\": \"auto\", \"worker_name\": %q } } }\n",
			w.Address, w.Name)
		return nil
	},
}

var workerListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List enrolled workers",
	RunE: func(cmd *cobra.Command, args []string) error {
		ca, err := openWorkerCA(cmd)
		if err != nil {
			return err
		}
		reg, err := ca.LoadRegistry()
		if err != nil {
			return err
		}
		if len(reg.Workers) == 0 {
			fmt.Println("No workers enrolled. Enroll one with: gt worker enroll <name> --address <host:port> --join-token <token>")
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tADDRESS\tSTATUS\tENROLLED\tEXPIRES")
		now := time.Now()
		for _, w := range reg.Workers {
			status := "active"
			switch {
			case w.Revoked:
				status = "revoked"
			case now.After(w.NotAfter):
				status = "expired"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				w.Name, w.Address, status,
				w.EnrolledAt.Format("2006-01-02"), w.NotAfter.Format("2006-01-02"))
		}
		return tw.Flush()
	},
}

var workerRevokeCmd = &cobra.Command{
	Use:   "revoke <name>",
	Short: "Revoke an enrolled worker's machine certificate",
	Long: `Revoke a worker, cutting it off from this town.

The daemon refuses to dial a revoked worker. Re-enroll (with a fresh join
token) to rotate a machine certificate back into service.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ca, err := openWorkerCA(cmd)
		if err != nil {
			return err
		}
		if err := ca.Revoke(args[0]); err != nil {
			return err
		}
		fmt.Printf("Revoked worker %s\n", args[0])
		return nil
	},
}

// openWorkerCA resolves the material dir (--ca-dir or the default) and loads
// or creates the worker CA.
func openWorkerCA(cmd *cobra.Command) (*workerca.CA, error) {
	dir, _ := cmd.Flags().GetString("ca-dir")
	if dir == "" {
		var err error
		dir, err = workerca.DefaultDir()
		if err != nil {
			return nil, err
		}
	}
	return workerca.LoadOrCreate(dir)
}

func init() {
	for _, c := range []*cobra.Command{workerEnrollCmd, workerListCmd, workerRevokeCmd} {
		c.Flags().String("ca-dir", "", "worker CA material directory (default $GT_WORKER_CA_DIR or ~/.gt/worker-ca)")
	}
	workerEnrollCmd.Flags().String("address", "", "worker's enrollment listener, host:port")
	workerEnrollCmd.Flags().String("join-token", "", "single-use token carried out-of-band to the worker")
	workerEnrollCmd.Flags().Bool("generate-token", false, "mint a join token and print the worker-side command")

	workerCmd.AddCommand(workerEnrollCmd, workerListCmd, workerRevokeCmd)
	rootCmd.AddCommand(workerCmd)
}
