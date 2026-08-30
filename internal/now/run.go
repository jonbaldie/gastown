package now

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"

	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/mayor"
	"github.com/jonbaldie/gastown/internal/session"
	"github.com/jonbaldie/gastown/internal/workspace"
)

// Options is the gt now command input.
type Options struct {
	RepoArg        string
	TownFlag       string
	Name           string
	MayorSpec      string
	WorkersSpec    string
	RestartWorkers bool
	NoAttach       bool
	ProvisionOnly  bool
	Executable     string
	Stdout         io.Writer
	Stderr         io.Writer
}

// Result is the user-visible outcome of a successful gt now run.
type Result struct {
	TownRoot      string
	RigName       string
	Mix           string
	DoltPort      int
	AttachSession string
}

// Hooks are cmd-owned operations that gt now must call without importing cmd.
type Hooks struct {
	EnsureDoltReady func() error
	InitBeads       func(townRoot string) error
	InitAgentBeads  func(townRoot string) error
	EnsureDaemon    func(townRoot string) error
}

// Run starts or reuses a Town for a git repository and prepares the Mayor session.
func Run(ctx context.Context, opts Options, hooks Hooks) (Result, error) {
	opts = normalizeOptions(opts)
	if ctx == nil {
		ctx = context.Background()
	}

	if opts.ProvisionOnly {
		return runProvisionOnly(opts, hooks)
	}

	inputs, err := resolveRunInputs(opts)
	if err != nil {
		return Result{}, err
	}
	preparation, err := prepareTown(ctx, inputs, hooks)
	if err != nil {
		return Result{}, err
	}
	initialization, err := initializeTown(ctx, inputs, opts, hooks)
	if err != nil {
		return Result{}, err
	}
	if err := startTown(ctx, inputs, initialization, opts, hooks); err != nil {
		return Result{}, err
	}
	return buildResult(inputs, preparation, initialization, opts), nil
}

type runInputs struct {
	repoPath       string
	townRoot       string
	mayorProfile   Profile
	workersProfile Profile
}

type townPreparation struct {
	port int
}

type townInitialization struct {
	rigName      string
	mayorChanged bool
}

func normalizeOptions(opts Options) Options {
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	return opts
}

func runProvisionOnly(opts Options, hooks Hooks) (Result, error) {
	townRoot, err := ResolveTownRoot(opts.TownFlag)
	if err != nil {
		return Result{}, err
	}
	return Result{TownRoot: townRoot}, provisionTown(townRoot, hooks)
}

func resolveRunInputs(opts Options) (runInputs, error) {
	repoPath, err := ResolveRepo(opts.RepoArg)
	if err != nil {
		return runInputs{}, err
	}
	mayorProfile, workersProfile, err := ResolveProfiles(opts.MayorSpec, opts.WorkersSpec)
	if err != nil {
		return runInputs{}, err
	}
	if err := preflightRepo(repoPath); err != nil {
		return runInputs{}, err
	}
	townRoot, err := ResolveTownRoot(opts.TownFlag)
	if err != nil {
		return runInputs{}, err
	}
	if err := refuseTownHQConversion(repoPath, townRoot); err != nil {
		return runInputs{}, err
	}
	return runInputs{
		repoPath:       repoPath,
		townRoot:       townRoot,
		mayorProfile:   mayorProfile,
		workersProfile: workersProfile,
	}, nil
}

func prepareTown(ctx context.Context, inputs runInputs, hooks Hooks) (townPreparation, error) {
	if err := ensureTown(ctx, inputs.townRoot, hooks); err != nil {
		return townPreparation{}, err
	}
	port, err := chooseDoltPort(inputs.townRoot)
	if err != nil {
		return townPreparation{}, err
	}
	if err := os.Setenv("GT_TOWN_ROOT", inputs.townRoot); err != nil {
		return townPreparation{}, fmt.Errorf("setting GT_TOWN_ROOT: %w", err)
	}
	if err := os.Setenv("GT_DOLT_PORT", strconv.Itoa(port)); err != nil {
		return townPreparation{}, fmt.Errorf("setting GT_DOLT_PORT: %w", err)
	}
	if err := persistDoltPort(inputs.townRoot, port); err != nil {
		return townPreparation{}, err
	}
	if err := session.InitRegistry(inputs.townRoot); err != nil {
		return townPreparation{}, fmt.Errorf("initializing town registry: %w", err)
	}
	return townPreparation{port: port}, nil
}

func initializeTown(ctx context.Context, inputs runInputs, opts Options, hooks Hooks) (townInitialization, error) {
	var (
		doltErr, rigErr, mixErr error
		result                  townInitialization
	)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		doltErr = runTownTask(ctx, func() error { return startDolt(ctx, inputs.townRoot, hooks) })
	}()
	go func() {
		defer wg.Done()
		rigErr = runTownTask(ctx, func() error {
			var err error
			result.rigName, err = ensureRig(ctx, inputs.townRoot, inputs.repoPath, opts.Name)
			return err
		})
	}()
	go func() {
		defer wg.Done()
		mixErr = runTownTask(ctx, func() error {
			var err error
			result.mayorChanged, err = ApplyMix(inputs.townRoot, opts.MayorSpec, opts.WorkersSpec, inputs.mayorProfile, inputs.workersProfile)
			return err
		})
	}()
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return townInitialization{}, err
	}
	if err := errors.Join(doltErr, rigErr, mixErr); err != nil {
		return townInitialization{}, err
	}
	return result, nil
}

func runTownTask(ctx context.Context, task func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return task()
}

func startTown(ctx context.Context, inputs runInputs, initialization townInitialization, opts Options, hooks Hooks) error {
	if err := startDeferredProvision(opts.Executable, inputs.townRoot); err != nil {
		fmt.Fprintf(opts.Stderr, "warning: deferred Town provision did not start: %v\n", err)
	}
	if err := startSessions(ctx, inputs.townRoot, initialization.mayorChanged, opts, hooks); err != nil {
		return err
	}
	return requireLiveSession(mayor.SessionName())
}

func buildResult(inputs runInputs, preparation townPreparation, initialization townInitialization, opts Options) Result {
	result := Result{
		TownRoot: inputs.townRoot,
		RigName:  initialization.rigName,
		Mix:      inputs.mayorProfile.Format() + " / " + inputs.workersProfile.Format(),
		DoltPort: preparation.port,
	}
	if !opts.NoAttach {
		result.AttachSession = mayor.SessionName()
	}
	return result
}

func preflightRepo(repoPath string) error {
	repoGit := git.NewGit(repoPath)
	if !git.IsRepo(repoGit) {
		return fmt.Errorf("not a git repository: %s", repoPath)
	}
	empty, err := git.IsEmpty(repoGit)
	if err != nil {
		return fmt.Errorf("checking repository: %w", err)
	}
	if empty {
		return fmt.Errorf("repository %s is empty (no commits). Commit at least once before running gt now", repoPath)
	}
	return nil
}

func refuseTownHQConversion(repoPath, townRoot string) error {
	repoIsTown, err := workspace.IsWorkspace(repoPath)
	if err != nil {
		return err
	}
	same := SamePath(repoPath, townRoot)
	if repoIsTown && !same {
		return fmt.Errorf("this directory is a Town HQ (%s); run gt now from a project git repository", repoPath)
	}
	if !same {
		return nil
	}
	if !repoIsTown {
		return fmt.Errorf("refusing to convert this git repository into a Town HQ; pass --town for a separate Town")
	}
	registered, err := repoIsRegisteredRig(townRoot, repoPath)
	if err != nil {
		return fmt.Errorf("checking whether this Town HQ is a registered Rig: %w", err)
	}
	if !registered {
		return fmt.Errorf("this directory is a Town HQ; run gt now from a project git repository")
	}
	return nil
}
