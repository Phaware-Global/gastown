package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/telegraph"
	"github.com/steveyegge/gastown/internal/telegraph/prompts"
	jiratr "github.com/steveyegge/gastown/internal/telegraph/providers/jira"
	"github.com/steveyegge/gastown/internal/telegraph/tlog"
	"github.com/steveyegge/gastown/internal/telegraph/transform"
	"github.com/steveyegge/gastown/internal/telegraph/transport"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	telegraphConfigFlag   string
	telegraphTownRootFlag string
)

var telegraphCmd = &cobra.Command{
	Use:     "telegraph",
	GroupID: GroupAgents,
	Short:   "Manage the Telegraph webhook bridge",
	Long:    "Telegraph bridges external webhook events (e.g. Jira) into Mayor mail.",
}

var telegraphStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Telegraph webhook listener",
	Long: `Start the Telegraph webhook listener for this town.

Reads configuration from telegraph.toml (default: <town-root>/settings/telegraph.toml).

Exit codes:
  0  clean shutdown (SIGINT/SIGTERM received)
  1  infra/config error`,
	RunE: runTelegraphStart,
}

func init() {
	telegraphStartCmd.Flags().StringVar(&telegraphConfigFlag, "config", "", "path to telegraph.toml (default: <town-root>/settings/telegraph.toml)")
	telegraphStartCmd.Flags().StringVar(&telegraphTownRootFlag, "town-root", "", "town root directory (default: resolved from cwd)")

	telegraphCmd.AddCommand(telegraphStartCmd)
	rootCmd.AddCommand(telegraphCmd)
}

// telegraphDeps carries seams for testing runTelegraphStartImpl without exec'ing the binary.
type telegraphDeps struct {
	sender   transform.MailSender
	listenFn func(addr string) (net.Listener, error)
	nudger   transform.Nudger
	log      *tlog.Logger
}

func runTelegraphStart(cmd *cobra.Command, args []string) error {
	townRoot := telegraphTownRootFlag
	if townRoot == "" {
		var err error
		townRoot, err = workspace.FindFromCwdOrError()
		if err != nil {
			return fmt.Errorf("telegraph: %w", err)
		}
	}

	cfgPath := telegraphConfigFlag
	if cfgPath == "" {
		cfgPath = telegraph.DefaultPath(townRoot)
	}

	cfg, resolved, err := telegraphSetup(cfgPath)
	if err != nil {
		return err
	}

	var logDst *os.File = os.Stderr
	if cfg.Telegraph.LogFile != "" {
		f, err := os.OpenFile(cfg.Telegraph.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("telegraph: opening log file %s: %w", cfg.Telegraph.LogFile, err)
		}
		defer f.Close()
		logDst = f
	}

	deps := telegraphDeps{log: tlog.New(logDst)}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return runTelegraphStartImpl(ctx, cfg, townRoot, resolved, deps)
}

// telegraphSetup loads, validates, and resolves providers for a telegraph config.
// Tests call this directly to assert early-exit error messages.
func telegraphSetup(cfgPath string) (*telegraph.Config, map[string]*telegraph.ResolvedProvider, error) {
	cfg, err := telegraph.LoadWithDefaults(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	resolved, err := cfg.ResolveProviders()
	if err != nil {
		return nil, nil, fmt.Errorf("telegraph: %w", err)
	}
	return cfg, resolved, nil
}

func runTelegraphStartImpl(ctx context.Context, cfg *telegraph.Config, townRoot string, resolved map[string]*telegraph.ResolvedProvider, deps telegraphDeps) error {
	logger := deps.log
	nudgeWindow, err := cfg.Telegraph.ParsedNudgeWindow()
	if err != nil {
		return fmt.Errorf("telegraph: nudge_window: %w", err)
	}

	// Build prompts resolver from inline config (separate-file overrides applied at load time).
	resolver, err := buildPromptsResolver(cfg, townRoot)
	if err != nil {
		return fmt.Errorf("telegraph: prompts: %w", err)
	}

	// Build L3 transformer.
	var transformer *transform.Transformer
	if deps.sender != nil {
		nudger := deps.nudger
		if nudger == nil {
			nudger = &transform.ExecNudger{}
		}
		transformer = transform.New(deps.sender, nudger, cfg.Telegraph.BodyCap, nudgeWindow, resolver, logger)
	} else {
		transformer = transform.NewProduction(townRoot, cfg.Telegraph.BodyCap, nudgeWindow, resolver, logger)
	}

	// Build L2 translators from enabled, resolved providers.
	translatorMap := make(map[string]telegraph.Translator, len(resolved))
	var providerNames []string
	for id, rp := range resolved {
		switch id {
		case "jira":
			var ignoreActors []string
			if pc := cfg.Telegraph.Providers[id]; pc != nil {
				ignoreActors = pc.IgnoreActors
			}
			translatorMap[id] = jiratr.New(rp.Secret, ignoreActors, nil)
		default:
			return fmt.Errorf("telegraph: unsupported provider %q", id)
		}
		providerNames = append(providerNames, id)
	}
	sort.Strings(providerNames)

	translators := make([]telegraph.Translator, 0, len(translatorMap))
	for _, t := range translatorMap {
		translators = append(translators, t)
	}

	rawCh := make(chan telegraph.RawEvent, cfg.Telegraph.BufferSize)
	handler := transport.NewHandler(translators, rawCh, logger)

	// wg tracks in-flight HTTP handlers so we never close rawCh while a handler
	// goroutine might still be executing its non-blocking send.
	var wg sync.WaitGroup
	wgHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wg.Add(1)
		defer wg.Done()
		handler.ServeHTTP(w, r)
	})

	listenFn := deps.listenFn
	if listenFn == nil {
		listenFn = func(addr string) (net.Listener, error) {
			return net.Listen("tcp", addr)
		}
	}
	ln, err := listenFn(cfg.Telegraph.ListenAddr)
	if err != nil {
		return fmt.Errorf("telegraph: listen %s: %w", cfg.Telegraph.ListenAddr, err)
	}

	fmt.Printf("[Telegraph] listening on %s, providers=[%s]\n",
		cfg.Telegraph.ListenAddr, strings.Join(providerNames, ", "))

	// Dispatch goroutine: range over rawCh, L2 translate → L3 transform.
	dispatchDone := make(chan struct{})
	go func() {
		defer close(dispatchDone)
		for evt := range rawCh {
			tr, ok := translatorMap[evt.Provider]
			if !ok {
				logger.Drop(evt.Provider, "", "", "", "no_translator")
				continue
			}
			norm, err := tr.Translate(evt.Body)
			if errors.Is(err, telegraph.ErrActorFiltered) {
				// norm is non-nil; use it to populate the audit log.
				logger.Drop(evt.Provider, norm.EventType, norm.EventID, norm.Actor, tlog.ReasonActorFiltered)
				continue
			}
			if err != nil {
				logger.Drop(evt.Provider, "", "", "", "translate_error")
				continue
			}
			if err := transformer.Transform(norm); err != nil {
				logger.Drop(evt.Provider, norm.EventType, norm.EventID, norm.Actor, "transform_error")
			}
		}
	}()

	srv := &http.Server{Handler: wgHandler, ReadHeaderTimeout: 10 * time.Second}
	serveDone := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if err == http.ErrServerClosed {
			err = nil
		}
		serveDone <- err
	}()

	var srvErr error
	select {
	case <-ctx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			srv.Close()
		}
		srvErr = <-serveDone
	case err := <-serveDone:
		// Serve exited unexpectedly; Close prevents keep-alive connections from
		// starting new handlers that would race with wg.Wait().
		srv.Close()
		srvErr = err
	}

	wg.Wait() // all handler goroutines have returned; rawCh is now safe to close
	close(rawCh)
	<-dispatchDone
	fmt.Println("[Telegraph] shutdown complete")

	if srvErr != nil {
		return fmt.Errorf("telegraph: server: %w", srvErr)
	}
	return nil
}

// buildPromptsResolver merges inline and separate-file prompt configs and
// constructs a Resolver. Returns nil (no-op resolver) when no prompts are
// configured. The separate file (~/gt/settings/telegraph.prompts.toml) wins
// on key collision with inline config.
func buildPromptsResolver(cfg *telegraph.Config, townRoot string) (*prompts.Resolver, error) {
	inline := cfg.Telegraph.Prompts
	overrides, err := loadPromptsFile(townRoot)
	if err != nil {
		return nil, err
	}
	if len(inline) == 0 {
		if len(overrides) == 0 {
			return nil, nil
		}
		inline = overrides
	} else {
		// Merge: start with inline, overlay with separate-file overrides.
		merged := make(map[string]string, len(inline)+len(overrides))
		for k, v := range inline {
			merged[k] = v
		}
		for k, v := range overrides {
			merged[k] = v
		}
		inline = merged
	}

	promptCap := cfg.Telegraph.PromptCap
	if promptCap == 0 {
		promptCap = 2048
	}

	byKey := make(map[string]string, len(inline))
	defaultPrompt := ""
	for k, v := range inline {
		if k == "default" {
			defaultPrompt = v
		} else {
			byKey[k] = v
		}
	}

	r, err := prompts.NewResolver(prompts.Config{
		Default: defaultPrompt,
		ByKey:   byKey,
		Cap:     promptCap,
	})
	if err != nil {
		return nil, err
	}

	specific := len(byKey)
	hasDefault := defaultPrompt != ""
	fmt.Printf("[Telegraph] prompts loaded: %d specific, default=%v\n", specific, hasDefault)

	return r, nil
}

// promptsOverrideFile is the TOML structure for telegraph.prompts.toml.
type promptsOverrideFile struct {
	Telegraph struct {
		Prompts map[string]string `toml:"prompts"`
	} `toml:"telegraph"`
}

// loadPromptsFile loads ~/gt/settings/telegraph.prompts.toml if it exists.
// Returns nil map (no error) if the file is absent.
func loadPromptsFile(townRoot string) (map[string]string, error) {
	path := townRoot + "/settings/telegraph.prompts.toml"
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}
	var fc promptsOverrideFile
	if _, err := toml.DecodeFile(path, &fc); err != nil {
		return nil, fmt.Errorf("parsing prompts override file %s: %w", path, err)
	}
	return fc.Telegraph.Prompts, nil
}
