// Package cli implements RoleMux's small, stable command surface. Provider
// output never reaches stdout: JSON mode emits exactly one object and human
// mode keeps diagnostics on stderr.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/basant-kumar/rolemux/internal/capability"
	"github.com/basant-kumar/rolemux/internal/catalog"
	"github.com/basant-kumar/rolemux/internal/config"
	"github.com/basant-kumar/rolemux/internal/install"
	"github.com/basant-kumar/rolemux/internal/picker"
	"github.com/basant-kumar/rolemux/internal/runner"
	"github.com/basant-kumar/rolemux/internal/task"
	"github.com/basant-kumar/rolemux/internal/workflow"
)

const usageText = `RoleMux — The right mind for every role.

Usage:
  rolemux help
  rolemux -h
  rolemux --help
  rolemux --version [--json]
  rolemux version [--json]  (backward-compatible alias)
  rolemux models [--refresh] [--runner PROVIDER] [--json]
  rolemux configure [--global|--project] [--from PATH|-]
                    [--role planner|implementer|reviewer|plan-reviewer|code-reviewer]
                    [--runner PROVIDER] [--model MODEL] [--effort EFFORT] [--speed SPEED]
                    [--review-max-rounds N] [--json]
  Review safety limit: --review-max-rounds N defaults to 5, accepts 0 for unlimited,
  and applies to newly created tasks.
  rolemux plan start --task TEXT [--id TASK-ID] [--json]
  rolemux plan answer TASK-ID --answer TEXT [--json]
  rolemux plan review TASK-ID [--json]
  rolemux plan graph TASK-ID [--json]
  rolemux quick start --task TEXT --scope PATH[,PATH...] [--id TASK-ID] [--json]
  rolemux work start TASK-ID UNIT-ID [--json]
  rolemux work integrate TASK-ID [--json]
  rolemux implement TASK-ID [--scope PATH[,PATH...]] [--json]
  rolemux implement answer TASK-ID --answer TEXT [--json]
  rolemux code review TASK-ID [--json]
  rolemux status TASK-ID [--full] [--json]
  rolemux usage TASK-ID [--json]
  rolemux retry TASK-ID [--json]
  rolemux list [--json]
  rolemux doctor [--json]
  rolemux install --global --hosts all|antigravity,claude,codex,copilot [--force] [--json]
`

const backgroundModelRefreshTimeout = 30 * time.Second

type app struct {
	ctx           context.Context
	in            io.Reader
	out           io.Writer
	errOut        io.Writer
	version       string
	cwd           string
	environ       []string
	runners       *runner.Registry
	refreshModels func(config.Config, string, string, runner.Adapter, runner.ModelListRequest)
}

// Run executes one command and returns its documented process exit code.
func Run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer, version string) int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(errOut, "rolemux: %v\n", err)
		return workflow.ExitAction
	}
	a := &app{ctx: ctx, in: in, out: out, errOut: errOut, version: version, cwd: cwd, environ: os.Environ(), runners: runner.BuiltinRegistry()}
	return a.run(args)
}

func (a *app) run(args []string) int {
	jsonMode := containsFlag(args, "--json")
	if len(args) == 0 {
		return a.fail("", usage("a command is required"), jsonMode, workflow.Result{})
	}
	if args[0] == "help" || containsHelpFlag(args) {
		if jsonMode {
			return a.success("help", map[string]any{"usage": usageText}, nil, nil, true)
		}
		fmt.Fprint(a.out, usageText)
		return workflow.ExitOK
	}
	switch args[0] {
	case "--version", "version":
		return a.runVersion(args[1:])
	case "models":
		return a.runModels(args[1:])
	case "configure":
		return a.runConfigure(args[1:])
	case "plan":
		return a.runPlan(args[1:])
	case "quick":
		return a.runQuick(args[1:])
	case "work":
		return a.runWork(args[1:])
	case "implement":
		return a.runImplement(args[1:])
	case "code":
		return a.runCode(args[1:])
	case "status":
		return a.runStatus(args[1:])
	case "usage":
		return a.runUsage(args[1:])
	case "retry":
		return a.runRetry(args[1:])
	case "list":
		return a.runList(args[1:])
	case "doctor":
		return a.runDoctor(args[1:])
	case "install":
		return a.runInstall(args[1:])
	default:
		return a.fail(args[0], usage("unknown command %q", args[0]), jsonMode, workflow.Result{})
	}
}

func (a *app) runQuick(args []string) int {
	jsonMode := containsFlag(args, "--json")
	if len(args) == 0 || args[0] != "start" {
		return a.fail("quick", usage("quick requires start"), jsonMode, workflow.Result{})
	}
	opts, err := parse(args[1:], map[string]bool{"--json": false, "--task": true, "--scope": true, "--id": true})
	if err != nil || len(opts.positionals) != 0 || !opts.present("--task") || !opts.present("--scope") {
		if err == nil {
			err = usage("quick start requires --task, --scope, and no positional arguments")
		}
		return a.fail("quick-start", err, opts.json(), workflow.Result{})
	}
	service, err := a.workflowServiceForTask(opts.value("--id"))
	if err != nil {
		return a.fail("quick-start", err, opts.json(), workflow.Result{})
	}
	result, callErr := service.StartQuick(opts.value("--task"), opts.value("--scope"), opts.value("--id"))
	return a.workflowResult("quick-start", result, callErr, opts.json())
}

func (a *app) runVersion(args []string) int {
	opts, err := parse(args, map[string]bool{"--json": false})
	if err != nil || len(opts.positionals) != 0 {
		if err == nil {
			err = usage("version accepts no positional arguments")
		}
		return a.fail("version", err, opts.json(), workflow.Result{})
	}
	if opts.json() {
		return a.success("version", map[string]string{"version": a.version}, nil, nil, true)
	}
	fmt.Fprintln(a.out, a.version)
	return workflow.ExitOK
}

func (a *app) runModels(args []string) int {
	opts, err := parse(args, map[string]bool{"--json": false, "--refresh": false, "--runner": true})
	if err != nil || len(opts.positionals) != 0 {
		if err == nil {
			err = usage("models accepts no positional arguments")
		}
		return a.fail("models", err, opts.json(), workflow.Result{})
	}
	provider := strings.ToLower(opts.value("--runner"))
	if provider != "" && !a.runnerRegistry().Has(provider) {
		return a.fail("models", usage("unknown runner %q", provider), opts.json(), workflow.Result{})
	}
	root := a.optionalRepo()
	cfg, err := config.LoadWithEnv(root, a.environ)
	if err != nil {
		return a.fail("models", configProblem(err), opts.json(), workflow.Result{})
	}
	cfg, adapters, adapterErrors := prepareAdapters(cfg, root, a.runnerRegistry())
	providers := a.runnerRegistry().Names()
	if provider != "" {
		providers = []string{provider}
	}
	cat := catalog.New(adapters, cfg, catalog.DefaultCachePath(a.environ))
	models := []runner.ModelInfo{}
	advisories := []task.Diagnostic{}
	for _, name := range providers {
		if adapterErrors[name] != nil {
			advisories = append(advisories, task.Diagnostic{Code: "PROVIDER_UNAVAILABLE", Severity: "warning", Message: adapterErrors[name].Error(), TaskID: name})
			continue
		}
		runtime := workflow.RuntimeSnapshot(name, cfg.Provider(name))
		found, listErr := cat.Models(a.ctx, name, opts.bool("--refresh"), runner.ModelListRequest{Refresh: opts.bool("--refresh"), Runtime: runtime})
		if listErr != nil {
			advisories = append(advisories, task.Diagnostic{Code: "MODEL_DISCOVERY_FAILED", Severity: "warning", Message: listErr.Error(), TaskID: name})
			continue
		}
		models = append(models, found...)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Provider == models[j].Provider {
			return models[i].ID < models[j].ID
		}
		return models[i].Provider < models[j].Provider
	})
	if len(models) == 0 {
		message := "no models are available"
		if len(advisories) > 0 {
			message = advisories[0].Message
		}
		return a.fail("models", action("MODEL_DISCOVERY_FAILED", message), opts.json(), workflow.Result{})
	}
	if opts.json() {
		return a.success("models", map[string]any{"models": models}, nil, advisories, true)
	}
	for _, model := range models {
		label := model.Label
		if label == "" || label == model.ID {
			label = "-"
		}
		fmt.Fprintf(a.out, "%-8s  %-32s  %-24s  %-9s  %s\n", model.Provider, model.ID, label, model.Availability, model.Origin)
	}
	for _, advisory := range advisories {
		fmt.Fprintf(a.errOut, "warning: %s: %s\n", advisory.TaskID, advisory.Message)
	}
	return workflow.ExitOK
}

func (a *app) runConfigure(args []string) int {
	jsonMode := containsFlag(args, "--json")
	opts, err := parse(args, map[string]bool{
		"--json": false, "--global": false, "--project": false, "--from": true,
		"--role": true, "--runner": true, "--model": true, "--effort": true, "--speed": true,
		"--review-max-rounds": true,
	})
	if err != nil || len(opts.positionals) != 0 {
		if err == nil {
			err = usage("configure accepts no positional arguments")
		}
		return a.fail("configure", err, jsonMode, workflow.Result{})
	}
	if opts.bool("--global") && opts.bool("--project") {
		return a.fail("configure", usage("--global and --project are mutually exclusive"), jsonMode, workflow.Result{})
	}
	profileDirect := opts.present("--role") || opts.present("--runner") || opts.present("--model") || opts.present("--effort") || opts.present("--speed")
	limitDirect := opts.present("--review-max-rounds")
	direct := profileDirect || limitDirect
	from := opts.present("--from")
	if from && direct {
		return a.fail("configure", usage("--from is mutually exclusive with direct settings flags"), jsonMode, workflow.Result{})
	}
	interactive := !from && !direct
	if jsonMode && interactive {
		return a.fail("configure", usage("--json requires --from or explicit role selection"), true, workflow.Result{})
	}
	if !opts.bool("--global") && !opts.bool("--project") && !interactive {
		return a.fail("configure", usage("select exactly one of --global or --project"), jsonMode, workflow.Result{})
	}
	if interactive && !isInteractive(a.in) {
		return a.fail("configure", usage("interactive configure requires a terminal, or pass --role/--from and a target"), jsonMode, workflow.Result{})
	}
	var reviewMaxRounds *int
	if limitDirect {
		value, parseErr := parseReviewMaxRounds(opts.value("--review-max-rounds"))
		if parseErr != nil {
			return a.fail("configure", parseErr, jsonMode, workflow.Result{})
		}
		reviewMaxRounds = &value
	}
	root := a.optionalRepo()
	if interactive {
		target, draft, before, pickErr := a.pickInteractiveConfiguration(root, opts.bool("--global"), opts.bool("--project"))
		if pickErr != nil {
			return a.fail("configure", pickErr, false, workflow.Result{})
		}
		if err := config.ConfigureSettingsAtomic(target, draft.profiles, draft.reviewMaxRounds, before); err != nil {
			return a.fail("configure", configProblem(err), false, workflow.Result{})
		}
		return a.configureSuccess(target, "updated", false)
	}
	target, err := configTarget(root, opts.bool("--global"), opts.bool("--project"), a.environ)
	if err != nil {
		return a.fail("configure", usage("%v", err), jsonMode, workflow.Result{})
	}
	before, err := config.FileHash(target)
	if err != nil {
		return a.fail("configure", configProblem(err), jsonMode, workflow.Result{})
	}
	if from {
		data, readErr := a.readConfigSource(opts.value("--from"))
		if readErr != nil {
			return a.fail("configure", configProblem(readErr), jsonMode, workflow.Result{})
		}
		if err := config.ImportConfigAtomic(target, data, before); err != nil {
			return a.fail("configure", configProblem(err), jsonMode, workflow.Result{})
		}
		return a.configureSuccess(target, "imported", jsonMode)
	}
	if direct {
		profiles := map[string]config.Profile(nil)
		if profileDirect {
			if !opts.present("--role") || !opts.present("--runner") || !opts.present("--model") {
				return a.fail("configure", usage("--role, --runner, and --model are required together"), jsonMode, workflow.Result{})
			}
			role, roleErr := config.CanonicalRole(opts.value("--role"))
			if roleErr != nil {
				return a.fail("configure", configProblem(roleErr), jsonMode, workflow.Result{})
			}
			profile := config.Profile{Provider: strings.ToLower(opts.value("--runner")), Model: opts.value("--model"), Effort: opts.value("--effort"), Speed: opts.value("--speed")}
			cfg, loadErr := config.LoadWithEnv(root, a.environ)
			if loadErr != nil {
				return a.fail("configure", configProblem(loadErr), jsonMode, workflow.Result{})
			}
			if selectionErr := a.validateProfileSelection(root, cfg, role, profile); selectionErr != nil {
				return a.fail("configure", configProblem(selectionErr), jsonMode, workflow.Result{})
			}
			profiles = map[string]config.Profile{role: profile}
		}
		if err := config.ConfigureSettingsAtomic(target, profiles, reviewMaxRounds, before); err != nil {
			return a.fail("configure", configProblem(err), jsonMode, workflow.Result{})
		}
		return a.configureSuccess(target, "updated", jsonMode)
	}
	return a.fail("configure", usage("configuration mode is required"), false, workflow.Result{})
}

func parseReviewMaxRounds(raw string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, usage("--review-max-rounds must be a nonnegative integer")
	}
	return value, nil
}

func (a *app) validateProfileSelection(root string, cfg config.Config, role string, profile config.Profile) error {
	if err := config.ValidateProfile(profile); err != nil {
		return err
	}
	cfg, adapters, adapterErrors := prepareAdapters(cfg, root, a.runnerRegistry())
	if err := adapterErrors[profile.Provider]; err != nil {
		return err
	}
	adapter := adapters[profile.Provider]
	if adapter == nil {
		return fmt.Errorf("provider %s is unavailable", profile.Provider)
	}
	runtime := workflow.RuntimeSnapshot(profile.Provider, cfg.Provider(profile.Provider))
	models, err := catalog.New(adapters, cfg, catalog.DefaultCachePath(a.environ)).Models(a.ctx, profile.Provider, true, runner.ModelListRequest{Refresh: true, Runtime: runtime})
	if err != nil {
		return err
	}
	return runner.ValidateSelection(runner.Role(role), profile.Model, profile.Effort, profile.Speed, models, adapter)
}

func (a *app) configureSuccess(path, status string, jsonMode bool) int {
	result := map[string]string{"status": status, "path": path}
	if jsonMode {
		return a.success("configure", result, nil, nil, true)
	}
	fmt.Fprintf(a.out, "%s %s\n", status, path)
	return workflow.ExitOK
}

type settingsDraft struct {
	profiles        map[string]config.Profile
	reviewMaxRounds *int
}

func (a *app) pickInteractiveConfiguration(root string, global, project bool) (string, settingsDraft, string, error) {
	screen := picker.NewScreen(a.out)
	screen.Enter()
	defer screen.Leave()

	target := config.ExplicitConfigPath(a.environ)
	if target == "" && !global && !project {
		if root == "" {
			return "", settingsDraft{}, "", usage("interactive project configuration requires a Git worktree")
		}
		projectTarget, err := configTarget(root, false, true, a.environ)
		if err != nil {
			return "", settingsDraft{}, "", usage("%v", err)
		}
		choice, action, err := picker.Select(a.ctx, a.in, a.out, yesNoOptions("Cancel", "Configure this project"), picker.View{
			Title: "Configure RoleMux", Step: "Project", Subtitle: fmt.Sprintf("Configure this project (%s)?", projectTarget), FullScreen: true,
		})
		if err != nil {
			return "", settingsDraft{}, "", err
		}
		if action != picker.ActionSelected || choice.ID != "yes" {
			return "", settingsDraft{}, "", usage("configuration cancelled")
		}
		project = true
		target = projectTarget
	}
	if target == "" {
		var err error
		target, err = configTarget(root, global, project, a.environ)
		if err != nil {
			return "", settingsDraft{}, "", usage("%v", err)
		}
	}
	before, err := config.FileHash(target)
	if err != nil {
		return "", settingsDraft{}, "", configProblem(err)
	}
	draft, err := a.pickProfiles(root, screen)
	return target, draft, before, err
}

type wizardScreenKind int

const (
	wizardRole wizardScreenKind = iota
	wizardReviewMaxRounds
	wizardProvider
	wizardModel
	wizardVerifyModel
	wizardEffort
	wizardSpeed
	wizardSplitPlanReview
	wizardSplitCodeReview
)

type wizardScreen struct {
	kind wizardScreenKind
	role string
}

type profileDraft struct {
	provider string
	models   []runner.ModelInfo
	model    runner.ModelInfo
	effort   string
	speed    string
}

type providerReadiness struct {
	adapter       runner.Adapter
	checked       bool
	authenticated bool
	externalAuth  bool
	account       string
	status        string
	message       string
}

func (a *app) pickProfiles(root string, terminal *picker.Screen) (settingsDraft, error) {
	cfg, err := config.LoadWithEnv(root, a.environ)
	if err != nil {
		return settingsDraft{}, configProblem(err)
	}
	var adapters map[string]runner.Adapter
	var adapterErrors map[string]error
	var cat *catalog.Catalog
	var readiness map[string]providerReadiness
	drafts := map[string]*profileDraft{}
	modelsByProvider := map[string][]runner.ModelInfo{}
	separate := map[string]bool{}
	history := []wizardScreen{}
	current := wizardScreen{kind: wizardRole}
	selection := "all"
	notice := ""
	var reviewMaxRounds *int

	goBack := func() bool {
		if len(history) == 0 {
			return false
		}
		current = history[len(history)-1]
		history = history[:len(history)-1]
		notice = ""
		return true
	}
	advance := func(next wizardScreen) {
		history = append(history, current)
		current = next
		notice = ""
	}

	for {
		view := wizardView(current, len(history) > 0, notice, drafts)
		view.InitialID = wizardInitialID(current, cfg, drafts)
		var options []picker.Option
		switch current.kind {
		case wizardRole:
			options = wizardRoleOptions()
		case wizardReviewMaxRounds:
			options = reviewMaxRoundsOptions(cfg.EffectiveReviewMaxRounds())
		case wizardProvider:
			options = providerWizardOptions(a.runnerRegistry().Names(), readiness)
		case wizardModel:
			options = picker.ModelOptions(drafts[current.role].models)
		case wizardVerifyModel:
			options = yesNoOptions("No, choose another model", "Yes, select anyway")
		case wizardSplitPlanReview, wizardSplitCodeReview:
			options = yesNoOptions("No, use shared reviewer", "Yes, configure separately")
		case wizardEffort:
			options = picker.EffortOptions(drafts[current.role].model)
		case wizardSpeed:
			options = picker.SpeedOptions(drafts[current.role].model)
		}
		choice, pickAction, pickErr := picker.Select(a.ctx, a.in, a.out, options, view)
		if pickErr != nil {
			return settingsDraft{}, pickErr
		}
		if pickAction == picker.ActionCancel {
			return settingsDraft{}, usage("configuration cancelled")
		}
		if pickAction == picker.ActionBack {
			if !goBack() {
				return settingsDraft{}, usage("configuration cancelled")
			}
			continue
		}

		switch current.kind {
		case wizardRole:
			selection = choice.ID
			if selection == "review-max-rounds" {
				advance(wizardScreen{kind: wizardReviewMaxRounds})
				continue
			}
			if adapters == nil {
				cfg, adapters, adapterErrors = prepareAdapters(cfg, root, a.runnerRegistry())
				cat = catalog.New(adapters, cfg, catalog.DefaultCachePath(a.environ))
			}
			role := choice.ID
			if selection == "all" {
				role = config.RolePlanner
			}
			if readiness == nil {
				readiness = a.installedProviders(cfg, adapters, adapterErrors)
			}
			advance(wizardScreen{kind: wizardProvider, role: role})

		case wizardReviewMaxRounds:
			value, parseErr := strconv.Atoi(choice.ID)
			if parseErr != nil || value < 0 {
				return settingsDraft{}, usage("invalid review safety limit %q", choice.ID)
			}
			reviewMaxRounds = &value
			return settingsDraft{reviewMaxRounds: reviewMaxRounds}, nil

		case wizardProvider:
			ready := readiness[choice.ID]
			if ready.adapter == nil {
				provider := cfg.Provider(choice.ID)
				adapter, resolved, buildErr := a.runnerRegistry().Build(choice.ID, provider.CLIPath, root)
				if buildErr != nil {
					ready.message = providerInstallMessage(choice.ID, buildErr)
					readiness[choice.ID] = ready
					notice = ready.message
					continue
				}
				provider.CLIPath = resolved
				cfg.Providers[choice.ID] = provider
				adapters[choice.ID] = adapter
				cat = catalog.New(adapters, cfg, catalog.DefaultCachePath(a.environ))
				ready = providerReadiness{adapter: adapter, status: "installed"}
				readiness[choice.ID] = ready
			}
			statusView := wizardProviderStatusView(current, len(history) > 0, notice, drafts, choice.ID)
			if !ready.checked {
				terminal.ShowViewStatus(statusView, "Checking "+providerDisplayName(choice.ID)+" sign-in…")
				ready = a.inspectProvider(choice.ID, cfg, ready.adapter, nil)
				readiness[choice.ID] = ready
			}
			if ready.externalAuth && !ready.authenticated {
				notice = ready.message
				continue
			}
			if supporter, ok := ready.adapter.(runner.RoleSupporter); ok {
				if supportErr := supporter.SupportsRole(runner.Role(current.role)); supportErr != nil {
					notice = supportErr.Error()
					continue
				}
			}
			if !ready.authenticated && !ready.externalAuth {
				loginErr := a.loginProvider(choice.ID, ready.adapter, root, terminal, statusView)
				if loginErr != nil {
					ready.status = "login required"
					ready.message = fmt.Sprintf("Login did not complete: %v", loginErr)
					readiness[choice.ID] = ready
					notice = ready.message
					continue
				}
				probeCtx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
				auth, authErr := ready.adapter.Auth(probeCtx)
				cancel()
				if authErr != nil || !auth.Authenticated {
					ready.status = "login required"
					ready.message = loginRequiredMessage(choice.ID, auth, authErr)
					readiness[choice.ID] = ready
					notice = ready.message
					continue
				}
				ready.authenticated, ready.account, ready.status, ready.message = true, auth.Account, "signed in", ""
				readiness[choice.ID] = ready
			}
			models := modelsByProvider[choice.ID]
			if models == nil {
				runtime := workflow.RuntimeSnapshot(choice.ID, cfg.Provider(choice.ID))
				request := runner.ModelListRequest{Refresh: true, Runtime: runtime}
				if cached, ok := cat.CachedModels(choice.ID, ready.account, request); ok {
					models = cached
					a.refreshModelCatalog(cfg, cat.CachePath, choice.ID, ready.adapter, request)
				} else {
					terminal.ShowViewStatus(statusView, "Discovering "+providerDisplayName(choice.ID)+" models for the first time…")
					var modelErr error
					models, modelErr = cat.Models(a.ctx, choice.ID, true, request)
					if modelErr != nil {
						notice = "Model discovery failed: " + modelErr.Error()
						continue
					}
				}
				modelsByProvider[choice.ID] = models
			}
			drafts[current.role] = &profileDraft{provider: choice.ID, models: models}
			advance(wizardScreen{kind: wizardModel, role: current.role})

		case wizardModel:
			draft := drafts[current.role]
			draft.model, draft.effort, draft.speed = findModel(draft.models, choice.ID), "", ""
			if picker.UnknownAvailabilityWarning(draft.model) != "" {
				advance(wizardScreen{kind: wizardVerifyModel, role: current.role})
			} else if len(draft.model.Efforts) > 0 {
				advance(wizardScreen{kind: wizardEffort, role: current.role})
			} else if len(draft.model.SpeedOptions) > 0 {
				advance(wizardScreen{kind: wizardSpeed, role: current.role})
			} else if next, done := selectedNextProfileScreen(selection, current.role); done {
				return settingsDraft{profiles: selectedProfiles(selection, drafts, separate), reviewMaxRounds: reviewMaxRounds}, nil
			} else {
				advance(next)
			}

		case wizardVerifyModel:
			if choice.ID == "no" {
				goBack()
				continue
			}
			draft := drafts[current.role]
			if len(draft.model.Efforts) > 0 {
				advance(wizardScreen{kind: wizardEffort, role: current.role})
			} else if len(draft.model.SpeedOptions) > 0 {
				advance(wizardScreen{kind: wizardSpeed, role: current.role})
			} else if next, done := selectedNextProfileScreen(selection, current.role); done {
				return settingsDraft{profiles: selectedProfiles(selection, drafts, separate), reviewMaxRounds: reviewMaxRounds}, nil
			} else {
				advance(next)
			}

		case wizardEffort:
			drafts[current.role].effort = choice.ID
			if len(drafts[current.role].model.SpeedOptions) > 0 {
				advance(wizardScreen{kind: wizardSpeed, role: current.role})
			} else if next, done := selectedNextProfileScreen(selection, current.role); done {
				return settingsDraft{profiles: selectedProfiles(selection, drafts, separate), reviewMaxRounds: reviewMaxRounds}, nil
			} else {
				advance(next)
			}

		case wizardSpeed:
			drafts[current.role].speed = choice.ID
			if next, done := selectedNextProfileScreen(selection, current.role); done {
				return settingsDraft{profiles: selectedProfiles(selection, drafts, separate), reviewMaxRounds: reviewMaxRounds}, nil
			} else {
				advance(next)
			}

		case wizardSplitPlanReview:
			separate[config.RolePlanReviewer] = choice.ID == "yes"
			if choice.ID == "yes" {
				advance(wizardScreen{kind: wizardProvider, role: config.RolePlanReviewer})
			} else {
				advance(wizardScreen{kind: wizardSplitCodeReview})
			}

		case wizardSplitCodeReview:
			separate[config.RoleCodeReviewer] = choice.ID == "yes"
			if choice.ID == "yes" {
				advance(wizardScreen{kind: wizardProvider, role: config.RoleCodeReviewer})
			} else {
				return settingsDraft{profiles: selectedProfiles(selection, drafts, separate), reviewMaxRounds: reviewMaxRounds}, nil
			}
		}
	}
}

func wizardInitialID(screen wizardScreen, cfg config.Config, drafts map[string]*profileDraft) string {
	draft := drafts[screen.role]
	profile := cfg.Profiles[screen.role]
	if profile.Provider == "" && (screen.role == config.RolePlanReviewer || screen.role == config.RoleCodeReviewer) {
		profile = cfg.Profiles[config.RoleReviewer]
	}
	switch screen.kind {
	case wizardRole:
		return "all"
	case wizardReviewMaxRounds:
		return strconv.Itoa(cfg.EffectiveReviewMaxRounds())
	case wizardProvider:
		if draft != nil && draft.provider != "" {
			return draft.provider
		}
		return profile.Provider
	case wizardModel:
		if draft == nil {
			return ""
		}
		if draft.model.ID != "" {
			return draft.model.ID
		}
		if profile.Provider == draft.provider && profile.Model != "" {
			for _, model := range draft.models {
				if modelSelectorMatches(model, profile.Model) {
					return model.ID
				}
			}
		}
		for _, model := range draft.models {
			if model.IsDefault {
				return model.ID
			}
		}
	case wizardEffort:
		if draft == nil {
			return ""
		}
		if draft.effort != "" {
			return draft.effort
		}
		if profile.Provider == draft.provider && modelSelectorMatches(draft.model, profile.Model) && profile.Effort != "" {
			return profile.Effort
		}
		return draft.model.DefaultEffort
	case wizardSpeed:
		if draft == nil {
			return ""
		}
		if draft.speed != "" {
			return draft.speed
		}
		if profile.Provider == draft.provider && modelSelectorMatches(draft.model, profile.Model) && profile.Speed != "" {
			return profile.Speed
		}
		if draft.model.DefaultSpeed != "" {
			return draft.model.DefaultSpeed
		}
		return "standard"
	}
	return ""
}

func modelSelectorMatches(model runner.ModelInfo, selector string) bool {
	if selector == "" {
		return false
	}
	if model.ID == selector {
		return true
	}
	for _, alias := range model.Aliases {
		if alias == selector {
			return true
		}
	}
	return false
}

func (a *app) installedProviders(cfg config.Config, adapters map[string]runner.Adapter, adapterErrors map[string]error) map[string]providerReadiness {
	result := map[string]providerReadiness{}
	for _, name := range a.runnerRegistry().Names() {
		if adapters[name] == nil {
			result[name] = providerReadiness{checked: true, status: "not installed", message: providerInstallMessage(name, adapterErrors[name])}
			continue
		}
		provider := cfg.Provider(name)
		runtime := workflow.RuntimeSnapshot(name, provider)
		if len(runtime.AuthEnvRefs) > 0 && !provider.RequiresOpenAIAuth {
			missing := missingEnvironment(runtime.AuthEnvRefs, a.environ)
			if len(missing) > 0 {
				result[name] = providerReadiness{adapter: adapters[name], checked: true, externalAuth: true, status: "credentials required", message: "Set required credential environment: " + strings.Join(missing, ", ")}
			} else {
				result[name] = providerReadiness{adapter: adapters[name], checked: true, authenticated: true, externalAuth: true, status: "configured credentials"}
			}
			continue
		}
		result[name] = providerReadiness{adapter: adapters[name], status: "installed"}
	}
	return result
}

func (a *app) inspectProvider(name string, cfg config.Config, adapter runner.Adapter, adapterErr error) providerReadiness {
	if adapter == nil {
		return providerReadiness{checked: true, status: "not installed", message: providerInstallMessage(name, adapterErr)}
	}
	provider := cfg.Provider(name)
	runtime := workflow.RuntimeSnapshot(name, provider)
	if len(runtime.AuthEnvRefs) > 0 && !provider.RequiresOpenAIAuth {
		missing := missingEnvironment(runtime.AuthEnvRefs, a.environ)
		if len(missing) > 0 {
			return providerReadiness{adapter: adapter, checked: true, externalAuth: true, status: "credentials required", message: "Set required credential environment: " + strings.Join(missing, ", ")}
		}
		return providerReadiness{adapter: adapter, checked: true, authenticated: true, externalAuth: true, status: "configured credentials"}
	}
	if hinter, ok := adapter.(runner.LocalAuthHinter); ok {
		auth := hinter.LocalAuthHint()
		if auth.Authenticated {
			return providerReadiness{adapter: adapter, checked: true, authenticated: true, account: auth.Account, status: "credentials found", message: auth.Message}
		}
		return providerReadiness{adapter: adapter, checked: true, status: "login required", message: loginRequiredMessage(name, auth, nil)}
	}
	probeCtx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
	auth, authErr := adapter.Auth(probeCtx)
	cancel()
	if authErr == nil && auth.Authenticated {
		return providerReadiness{adapter: adapter, checked: true, authenticated: true, account: auth.Account, status: "signed in"}
	}
	return providerReadiness{adapter: adapter, checked: true, status: "login required", message: loginRequiredMessage(name, auth, authErr)}
}

func (a *app) refreshModelCatalog(cfg config.Config, cachePath, provider string, adapter runner.Adapter, request runner.ModelListRequest) {
	if a.refreshModels != nil {
		a.refreshModels(cfg, cachePath, provider, adapter, request)
		return
	}
	// Use a provider-only adapter map so later wizard changes cannot race this
	// refresh. Cache writes for the same path are serialized by catalog.
	adapters := map[string]runner.Adapter{provider: adapter}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), backgroundModelRefreshTimeout)
		defer cancel()
		_, _ = catalog.New(adapters, cfg, cachePath).Models(ctx, provider, true, request)
	}()
}

func (a *app) loginProvider(name string, adapter runner.Adapter, root string, terminal *picker.Screen, view picker.View) error {
	authenticator, ok := adapter.(runner.Authenticator)
	if !ok {
		return fmt.Errorf("%s adapter does not support interactive login; run %s", providerDisplayName(name), providerLoginCommand(name))
	}
	terminal.Leave()
	role := roleDisplayName(view.ActiveRole)
	if strings.TrimSpace(view.ActiveRole) == "" {
		fmt.Fprintf(a.out, "RoleMux: %s login required; starting `%s`.\n\n", providerDisplayName(name), providerLoginCommand(name))
	} else {
		fmt.Fprintf(a.out, "RoleMux: %s login required for %s; starting `%s`.\n\n", providerDisplayName(name), role, providerLoginCommand(name))
	}
	err := authenticator.Login(a.ctx, runner.LoginRequest{RepoRoot: root, Stdin: a.in, Stdout: a.out, Stderr: a.errOut})
	terminal.Enter()
	return err
}

func wizardRoleOptions() []picker.Option {
	return []picker.Option{
		{ID: "all", Label: "All roles", Description: "Configure the complete planning, implementation, and review pipeline"},
		{ID: config.RolePlanner, Label: "Planner"},
		{ID: config.RoleImplementer, Label: "Implementer"},
		{ID: config.RoleReviewer, Label: "Shared reviewer", Description: "Default for both plan and code review"},
		{ID: config.RolePlanReviewer, Label: "Plan reviewer"},
		{ID: config.RoleCodeReviewer, Label: "Code reviewer"},
		{ID: "review-max-rounds", Label: "Review safety limit", Description: "Use --review-max-rounds N for an arbitrary nonnegative value"},
	}
}

func reviewMaxRoundsOptions(current int) []picker.Option {
	options := make([]picker.Option, 0, 4)
	seen := map[int]bool{}
	add := func(value int, label, description string) {
		if seen[value] {
			for i := range options {
				if options[i].ID != strconv.Itoa(value) {
					continue
				}
				if !strings.Contains(options[i].Label, label) {
					options[i].Label += " / " + label
				}
				if description != "" && !strings.Contains(options[i].Description, description) {
					options[i].Description += "; " + description
				}
				break
			}
			return
		}
		seen[value] = true
		options = append(options, picker.Option{ID: strconv.Itoa(value), Label: label, Description: description})
	}
	currentLabel := fmt.Sprintf("Current (%d)", current)
	if current == 0 {
		currentLabel = "Current (Unlimited)"
	}
	add(current, currentLabel, "Keep the current effective review safety limit")
	add(config.DefaultReviewMaxRounds, "Default (5)", "Use the default review safety limit")
	add(10, "10", "Allow up to ten accepted reviewer verdicts")
	add(0, "Unlimited", "Set 0 for no review-round ceiling")
	return options
}

func selectedNextProfileScreen(selection, role string) (wizardScreen, bool) {
	if selection != "all" {
		return wizardScreen{}, true
	}
	return nextProfileScreen(role)
}

func selectedProfiles(selection string, drafts map[string]*profileDraft, separate map[string]bool) map[string]config.Profile {
	if selection == "all" {
		return buildProfiles(drafts, separate)
	}
	draft := drafts[selection]
	if draft == nil {
		return nil
	}
	return map[string]config.Profile{selection: {Provider: draft.provider, Model: draft.model.ID, Effort: draft.effort, Speed: draft.speed}}
}

func nextProfileScreen(role string) (wizardScreen, bool) {
	switch role {
	case config.RolePlanner:
		return wizardScreen{kind: wizardProvider, role: config.RoleImplementer}, false
	case config.RoleImplementer:
		return wizardScreen{kind: wizardProvider, role: config.RoleReviewer}, false
	case config.RoleReviewer:
		return wizardScreen{kind: wizardSplitPlanReview}, false
	case config.RolePlanReviewer:
		return wizardScreen{kind: wizardSplitCodeReview}, false
	case config.RoleCodeReviewer:
		return wizardScreen{}, true
	default:
		return wizardScreen{}, true
	}
}

func buildProfiles(drafts map[string]*profileDraft, separate map[string]bool) map[string]config.Profile {
	result := map[string]config.Profile{}
	for _, role := range []string{config.RolePlanner, config.RoleImplementer, config.RoleReviewer, config.RolePlanReviewer, config.RoleCodeReviewer} {
		if (role == config.RolePlanReviewer || role == config.RoleCodeReviewer) && !separate[role] {
			continue
		}
		draft := drafts[role]
		if draft != nil {
			result[role] = config.Profile{Provider: draft.provider, Model: draft.model.ID, Effort: draft.effort, Speed: draft.speed}
		}
	}
	return result
}

func findModel(models []runner.ModelInfo, id string) runner.ModelInfo {
	for _, model := range models {
		if model.ID == id {
			return model
		}
	}
	return runner.ModelInfo{ID: id}
}

func yesNoOptions(noLabel, yesLabel string) []picker.Option {
	return []picker.Option{{ID: "no", Label: noLabel}, {ID: "yes", Label: yesLabel}}
}

func providerWizardOptions(names []string, readiness map[string]providerReadiness) []picker.Option {
	options := make([]picker.Option, 0, len(names))
	for _, name := range names {
		label := providerDisplayName(name)
		if status := readiness[name].status; status != "" {
			label += " · " + status
		}
		options = append(options, picker.Option{ID: name, Label: label})
	}
	return options
}

func providerDisplayName(name string) string {
	labels := map[string]string{"codex": "Codex", "claude": "Claude Code", "copilot": "GitHub Copilot", "antigravity": "Google Antigravity"}
	if label := labels[name]; label != "" {
		return label
	}
	return name
}

func providerLoginCommand(name string) string {
	if command := map[string]string{"codex": "codex login", "claude": "claude auth login", "copilot": "copilot login", "antigravity": "agy"}[name]; command != "" {
		return command
	}
	return name + " login"
}

func loginRequiredMessage(name string, auth runner.AuthStatus, err error) string {
	message := strings.TrimSpace(auth.Message)
	if message == "" && err != nil {
		message = err.Error()
	}
	if message == "" {
		message = "authentication was not found"
	}
	return fmt.Sprintf("%s login required (%s); select it to run `%s`", providerDisplayName(name), message, providerLoginCommand(name))
}

func providerInstallMessage(name string, err error) string {
	if name == "copilot" {
		return "GitHub Copilot CLI is not installed; run `brew install copilot-cli`, then select it again"
	}
	if name == "antigravity" {
		return "Google Antigravity CLI is not installed; install it from `antigravity.google/docs/cli/install`, then select it again"
	}
	message := providerDisplayName(name) + " CLI is not installed"
	if err != nil {
		message += ": " + err.Error()
	}
	return message
}

func missingEnvironment(names, environ []string) []string {
	available := map[string]bool{}
	for _, item := range environ {
		name, value, ok := strings.Cut(item, "=")
		if ok && value != "" {
			available[name] = true
		}
	}
	missing := []string{}
	for _, name := range names {
		if !available[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

func (a *app) runPlan(args []string) int {
	jsonMode := containsFlag(args, "--json")
	if len(args) == 0 {
		return a.fail("plan", usage("plan requires start, answer, or review"), jsonMode, workflow.Result{})
	}
	switch args[0] {
	case "start":
		opts, err := parse(args[1:], map[string]bool{"--json": false, "--task": true, "--id": true})
		if err != nil || len(opts.positionals) != 0 || !opts.present("--task") {
			if err == nil {
				err = usage("plan start requires --task and no positional arguments")
			}
			return a.fail("plan-start", err, opts.json(), workflow.Result{})
		}
		service, err := a.workflowServiceForTask(opts.value("--id"))
		if err != nil {
			return a.fail("plan-start", err, opts.json(), workflow.Result{})
		}
		result, callErr := service.StartPlan(a.ctx, opts.value("--task"), opts.value("--id"))
		return a.workflowResult("plan-start", result, callErr, opts.json())
	case "answer":
		opts, err := parse(args[1:], map[string]bool{"--json": false, "--answer": true})
		if err != nil || len(opts.positionals) != 1 || !opts.present("--answer") {
			if err == nil {
				err = usage("plan answer requires TASK-ID and --answer")
			}
			return a.fail("plan-answer", err, opts.json(), workflow.Result{})
		}
		service, err := a.workflowServiceForTask(opts.positionals[0])
		if err != nil {
			return a.fail("plan-answer", err, opts.json(), workflow.Result{})
		}
		result, callErr := service.AnswerPlan(a.ctx, opts.positionals[0], opts.value("--answer"))
		return a.workflowResult("plan-answer", result, callErr, opts.json())
	case "review":
		opts, err := parse(args[1:], map[string]bool{"--json": false})
		if err != nil || len(opts.positionals) != 1 {
			if err == nil {
				err = usage("plan review requires TASK-ID")
			}
			return a.fail("plan-review", err, opts.json(), workflow.Result{})
		}
		service, err := a.workflowServiceForTask(opts.positionals[0])
		if err != nil {
			return a.fail("plan-review", err, opts.json(), workflow.Result{})
		}
		result, callErr := service.ReviewPlan(a.ctx, opts.positionals[0])
		return a.workflowResult("plan-review", result, callErr, opts.json())
	case "graph":
		opts, err := parse(args[1:], map[string]bool{"--json": false})
		if err != nil || len(opts.positionals) != 1 {
			if err == nil {
				err = usage("plan graph requires TASK-ID")
			}
			return a.fail("plan-graph", err, opts.json(), workflow.Result{})
		}
		service, err := a.workflowServiceForTask(opts.positionals[0])
		if err != nil {
			return a.fail("plan-graph", err, opts.json(), workflow.Result{})
		}
		graph, graphErr := service.Graph(opts.positionals[0])
		if graphErr != nil {
			return a.fail("plan-graph", graphErr, opts.json(), workflow.Result{})
		}
		if opts.json() {
			return a.success("plan-graph", graph, nil, nil, true)
		}
		printWorkGraph(a.out, graph)
		return workflow.ExitOK
	default:
		return a.fail("plan", usage("unknown plan command %q", args[0]), jsonMode, workflow.Result{})
	}
}

func (a *app) runWork(args []string) int {
	jsonMode := containsFlag(args, "--json")
	if len(args) == 0 {
		return a.fail("work", usage("work requires start or integrate"), jsonMode, workflow.Result{})
	}
	switch args[0] {
	case "start":
		opts, err := parse(args[1:], map[string]bool{"--json": false})
		if err != nil || len(opts.positionals) != 2 {
			if err == nil {
				err = usage("work start requires TASK-ID and UNIT-ID")
			}
			return a.fail("work-start", err, opts.json(), workflow.Result{})
		}
		service, err := a.workflowServiceForTask(opts.positionals[0])
		if err != nil {
			return a.fail("work-start", err, opts.json(), workflow.Result{})
		}
		result, callErr := service.StartWork(opts.positionals[0], opts.positionals[1])
		return a.workflowResult("work-start", result, callErr, opts.json())
	case "integrate":
		opts, err := parse(args[1:], map[string]bool{"--json": false})
		if err != nil || len(opts.positionals) != 1 {
			if err == nil {
				err = usage("work integrate requires TASK-ID")
			}
			return a.fail("work-integrate", err, opts.json(), workflow.Result{})
		}
		service, err := a.workflowServiceForIntegration(opts.positionals[0])
		if err != nil {
			return a.fail("work-integrate", err, opts.json(), workflow.Result{})
		}
		result, callErr := service.ReviewIntegration(a.ctx, opts.positionals[0])
		return a.workflowResult("work-integrate", result, callErr, opts.json())
	default:
		return a.fail("work", usage("unknown work command %q", args[0]), jsonMode, workflow.Result{})
	}
}

func (a *app) runImplement(args []string) int {
	if len(args) > 0 && args[0] == "answer" {
		opts, err := parse(args[1:], map[string]bool{"--json": false, "--answer": true})
		if err != nil || len(opts.positionals) != 1 || !opts.present("--answer") {
			if err == nil {
				err = usage("implement answer requires TASK-ID and --answer")
			}
			return a.fail("implement-answer", err, opts.json(), workflow.Result{})
		}
		service, err := a.workflowServiceForTask(opts.positionals[0])
		if err != nil {
			return a.fail("implement-answer", err, opts.json(), workflow.Result{})
		}
		result, callErr := service.AnswerImplement(a.ctx, opts.positionals[0], opts.value("--answer"))
		return a.workflowResult("implement-answer", result, callErr, opts.json())
	}
	opts, err := parse(args, map[string]bool{"--json": false, "--scope": true})
	if err != nil || len(opts.positionals) != 1 {
		if err == nil {
			err = usage("implement requires TASK-ID and optional --scope")
		}
		return a.fail("implement", err, opts.json(), workflow.Result{})
	}
	service, err := a.workflowServiceForTask(opts.positionals[0])
	if err != nil {
		return a.fail("implement", err, opts.json(), workflow.Result{})
	}
	result, callErr := service.Implement(a.ctx, opts.positionals[0], opts.value("--scope"))
	return a.workflowResult("implement", result, callErr, opts.json())
}

func (a *app) runCode(args []string) int {
	jsonMode := containsFlag(args, "--json")
	if len(args) == 0 || args[0] != "review" {
		return a.fail("code", usage("code requires review"), jsonMode, workflow.Result{})
	}
	opts, err := parse(args[1:], map[string]bool{"--json": false})
	if err != nil || len(opts.positionals) != 1 {
		if err == nil {
			err = usage("code review requires TASK-ID")
		}
		return a.fail("code-review", err, opts.json(), workflow.Result{})
	}
	service, err := a.workflowServiceForTask(opts.positionals[0])
	if err != nil {
		return a.fail("code-review", err, opts.json(), workflow.Result{})
	}
	result, callErr := service.ReviewCode(a.ctx, opts.positionals[0])
	return a.workflowResult("code-review", result, callErr, opts.json())
}

func (a *app) runStatus(args []string) int {
	opts, err := parse(args, map[string]bool{"--json": false, "--full": false})
	if err != nil || len(opts.positionals) != 1 {
		if err == nil {
			err = usage("status requires TASK-ID")
		}
		return a.fail("status", err, opts.json(), workflow.Result{})
	}
	service, err := a.workflowServiceForTask(opts.positionals[0])
	if err != nil {
		return a.fail("status", err, opts.json(), workflow.Result{})
	}
	state, loadErr := service.Status(opts.positionals[0])
	if loadErr != nil {
		return a.fail("status", loadErr, opts.json(), workflow.Result{})
	}
	if opts.json() {
		result := any(compactStatus(state))
		if opts.bool("--full") {
			result = state
		}
		return a.success("status", result, &state, state.Advisories, true)
	}
	printState(a.out, state)
	return workflow.ExitOK
}

func (a *app) runUsage(args []string) int {
	opts, err := parse(args, map[string]bool{"--json": false})
	if err != nil || len(opts.positionals) != 1 {
		if err == nil {
			err = usage("usage requires TASK-ID")
		}
		return a.fail("usage", err, opts.json(), workflow.Result{})
	}
	service, err := a.workflowServiceForTask(opts.positionals[0])
	if err != nil {
		return a.fail("usage", err, opts.json(), workflow.Result{})
	}
	state, loadErr := service.Status(opts.positionals[0])
	if loadErr != nil {
		return a.fail("usage", loadErr, opts.json(), workflow.Result{})
	}
	summary := summarizeUsage(state)
	if opts.json() {
		return a.success("usage", summary, &state, nil, true)
	}
	printUsage(a.out, summary)
	return workflow.ExitOK
}

func (a *app) runRetry(args []string) int {
	opts, err := parse(args, map[string]bool{"--json": false})
	if err != nil || len(opts.positionals) != 1 {
		if err == nil {
			err = usage("retry requires TASK-ID")
		}
		return a.fail("retry", err, opts.json(), workflow.Result{})
	}
	service, err := a.workflowServiceForTask(opts.positionals[0])
	if err != nil {
		return a.fail("retry", err, opts.json(), workflow.Result{})
	}
	result, callErr := service.Retry(a.ctx, opts.positionals[0])
	return a.workflowResult("retry", result, callErr, opts.json())
}

func (a *app) runList(args []string) int {
	opts, err := parse(args, map[string]bool{"--json": false})
	if err != nil || len(opts.positionals) != 0 {
		if err == nil {
			err = usage("list accepts no positional arguments")
		}
		return a.fail("list", err, opts.json(), workflow.Result{})
	}
	service, err := a.workflowService()
	if err != nil {
		return a.fail("list", err, opts.json(), workflow.Result{})
	}
	states, listErr := service.List()
	if listErr != nil {
		return a.fail("list", listErr, opts.json(), workflow.Result{})
	}
	if opts.json() {
		tasks := make([]statusSummary, 0, len(states))
		for _, state := range states {
			tasks = append(tasks, compactStatus(state))
		}
		return a.success("list", map[string]any{"tasks": tasks}, nil, nil, true)
	}
	for _, state := range states {
		fmt.Fprintf(a.out, "%s\t%s\tplan:%d code:%d\n", state.ID, state.Phase, state.PlanRound, state.CodeRound)
	}
	return workflow.ExitOK
}

func (a *app) runDoctor(args []string) int {
	opts, err := parse(args, map[string]bool{"--json": false})
	if err != nil || len(opts.positionals) != 0 {
		if err == nil {
			err = usage("doctor accepts no positional arguments")
		}
		return a.fail("doctor", err, opts.json(), workflow.Result{})
	}
	root := a.optionalRepo()
	checks := []doctorCheck{}
	if root == "" {
		checks = append(checks, doctorCheck{Name: "git_repository", OK: false, Message: "run RoleMux from a Git worktree"})
	} else {
		checks = append(checks, doctorCheck{Name: "git_repository", OK: true, Path: root})
	}
	cfg, loadErr := config.LoadWithEnv(root, a.environ)
	if loadErr != nil {
		checks = append(checks, doctorCheck{Name: "configuration", OK: false, Message: loadErr.Error()})
		return a.doctorResult(checks, false, workflow.ExitUsage, opts.json())
	}
	effective, profileErr := cfg.EffectiveProfiles()
	if profileErr != nil {
		checks = append(checks, doctorCheck{Name: "configuration", OK: false, Message: profileErr.Error()})
	} else {
		checks = append(checks, doctorCheck{Name: "configuration", OK: true})
	}
	cfg, adapters, adapterErrors := prepareAdapters(cfg, root, a.runnerRegistry())
	_ = cfg
	used := map[string]bool{}
	for _, profile := range effective {
		used[profile.Provider] = true
	}
	ready := root != "" && profileErr == nil
	for _, name := range a.runnerRegistry().Names() {
		adapter := adapters[name]
		if adapter == nil {
			message := "provider executable is unavailable"
			if adapterErrors[name] != nil {
				message = adapterErrors[name].Error()
			}
			checks = append(checks, doctorCheck{Name: "provider_" + name, OK: false, Required: used[name], Message: message})
			if used[name] {
				ready = false
			}
			continue
		}
		probeCtx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
		if prober, ok := adapter.(runner.CapabilityProber); ok {
			probeErr := prober.Probe(probeCtx)
			if probeErr != nil {
				cancel()
				checks = append(checks, doctorCheck{Name: "provider_" + name, OK: false, Required: used[name], Message: probeErr.Error()})
				if used[name] {
					ready = false
				}
				continue
			}
		}
		auth, authErr := adapter.Auth(probeCtx)
		cancel()
		ok := authErr == nil && auth.Authenticated
		message := auth.Message
		if authErr != nil {
			message = authErr.Error()
		}
		if !ok && message == "" {
			message = "not authenticated; log in with the provider CLI"
		}
		checks = append(checks, doctorCheck{Name: "provider_" + name, OK: ok, Required: used[name], Version: auth.Version, Message: message})
		if used[name] && !ok {
			ready = false
		}
	}
	if profileErr == nil {
		cat := catalog.New(adapters, cfg, catalog.DefaultCachePath(a.environ))
		modelsByProvider := map[string][]runner.ModelInfo{}
		for _, role := range []string{config.RolePlanner, config.RolePlanReviewer, config.RoleImplementer, config.RoleCodeReviewer} {
			profile, providerConfig := cfg.ResolveProfile(effective[role])
			adapter := adapters[profile.Provider]
			selectionErr := error(nil)
			if adapter == nil {
				selectionErr = fmt.Errorf("provider %s is unavailable", profile.Provider)
			} else {
				models := modelsByProvider[profile.Provider]
				if models == nil {
					probeCtx, cancel := context.WithTimeout(a.ctx, 20*time.Second)
					models, selectionErr = cat.Models(probeCtx, profile.Provider, true, runner.ModelListRequest{Refresh: true, Runtime: workflow.RuntimeSnapshot(profile.Provider, providerConfig)})
					cancel()
					if selectionErr == nil {
						modelsByProvider[profile.Provider] = models
					}
				}
				if selectionErr == nil {
					selectionErr = runner.ValidateSelection(runner.Role(role), profile.Model, profile.Effort, profile.Speed, models, adapter)
				}
			}
			message := fmt.Sprintf("%s / %s", profile.Provider, profile.Model)
			if profile.Effort != "" {
				message += " / effort=" + profile.Effort
			}
			if profile.Speed != "" {
				message += " / speed=" + profile.Speed
			}
			if selectionErr != nil {
				message = selectionErr.Error()
				ready = false
			}
			checks = append(checks, doctorCheck{Name: "selection_" + role, OK: selectionErr == nil, Required: true, Message: message})
		}
	}
	if root != "" {
		store := task.NewStore(root)
		_, stateErr := store.List()
		checks = append(checks, doctorCheck{Name: "private_state", OK: stateErr == nil, Path: store.Dir, Message: errorText(stateErr)})
		if stateErr != nil {
			ready = false
		}
	}
	pxpipe := capability.Discover(capability.Options{Provider: "claude", RepoRoot: root, Environ: a.environ}).Helpers
	if len(pxpipe) > 0 {
		checks = append(checks, doctorCheck{Name: "helper_pxpipe", OK: true, Path: pxpipe[0].Path, Message: "optional; RoleMux starts a private per-turn server (no daemon required), prints its dashboard URL, and wraps Codex only after verified ChatGPT authentication/route; inspect events with pxpipe stats --file"})
	} else {
		checks = append(checks, doctorCheck{Name: "helper_pxpipe", OK: false, Required: false, Message: "optional; without pxpipe Claude/Codex task turns remain direct; install it for private Claude wrapping or verified ChatGPT Codex wrapping"})
	}
	home, homeErr := os.UserHomeDir()
	if homeErr == nil {
		for _, host := range []struct {
			name  string
			parts []string
		}{
			{"antigravity", []string{".gemini", "antigravity-cli", "skills", "rolemux", "SKILL.md"}},
			{"claude", []string{".claude", "skills", "rolemux", "SKILL.md"}},
			{"codex", []string{".agents", "skills", "rolemux", "SKILL.md"}},
			{"copilot", []string{".copilot", "skills", "rolemux", "SKILL.md"}},
		} {
			path := filepath.Join(append([]string{home}, host.parts...)...)
			data, readErr := os.ReadFile(path)
			checks = append(checks, doctorCheck{Name: "skill_" + host.name, OK: readErr == nil && bytes.Equal(data, install.Content()), Required: false, Path: path, Message: skillMessage(readErr, data)})
		}
	}
	exit := workflow.ExitOK
	if !ready {
		if profileErr != nil || root == "" {
			exit = workflow.ExitUsage
		} else {
			exit = workflow.ExitAction
		}
	}
	return a.doctorResult(checks, ready, exit, opts.json())
}

func (a *app) runInstall(args []string) int {
	opts, err := parse(args, map[string]bool{"--json": false, "--global": false, "--hosts": true, "--force": false})
	if err != nil || len(opts.positionals) != 0 || !opts.bool("--global") || !opts.present("--hosts") {
		if err == nil {
			err = usage("install requires --global and --hosts")
		}
		return a.fail("install", err, opts.json(), workflow.Result{})
	}
	hosts, err := install.ParseHosts(opts.value("--hosts"))
	if err != nil {
		return a.fail("install", usage("%v", err), opts.json(), workflow.Result{})
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return a.fail("install", action("HOME_UNAVAILABLE", err.Error()), opts.json(), workflow.Result{})
	}
	results, err := install.InstallGlobal(home, hosts, opts.bool("--force"))
	if err != nil {
		return a.fail("install", configProblem(err), opts.json(), workflow.Result{})
	}
	if opts.json() {
		return a.success("install", map[string]any{"installations": results}, nil, nil, true)
	}
	for _, result := range results {
		fmt.Fprintf(a.out, "%s\t%s\t%s\n", result.Host, result.Status, result.Path)
	}
	return workflow.ExitOK
}

func (a *app) workflowService() (*workflow.Service, error) {
	root, err := task.DiscoverRepository(a.cwd)
	if err != nil {
		return nil, usage("%v", err)
	}
	cfg, err := config.LoadWithEnv(root, a.environ)
	if err != nil {
		return nil, configProblem(err)
	}
	cfg, adapters, _ := prepareAdapters(cfg, root, a.runnerRegistry())
	service := workflow.New(root, cfg, adapters)
	service.ModelCachePath = catalog.DefaultCachePath(a.environ)
	service.Diagnostic = func(message string) { fmt.Fprintf(a.errOut, "rolemux: %s\n", message) }
	environ := append([]string(nil), a.environ...)
	service.Capabilities = func(provider string, role runner.Role, taskText string) workflow.CapabilityContext {
		inventory := capability.Discover(capability.Options{Provider: provider, Role: string(role), Task: taskText, RepoRoot: root, CodexAdminSkills: "/etc/codex/skills", Environ: environ})
		return workflow.CapabilityContext{Note: inventory.Note(string(role)), SkillDirectories: inventory.SkillDirectories}
	}
	return service, nil
}

// workflowServiceForTask preserves a command's task ID when service setup
// fails before the workflow can return a Result. fail then uses that ID only
// to attempt a read-only task-store recovery; it never invents task state.
func (a *app) workflowServiceForTask(taskID string) (*workflow.Service, error) {
	service, err := a.workflowService()
	if err != nil && strings.TrimSpace(taskID) != "" {
		return nil, &taskIDError{err: err, taskID: taskID}
	}
	return service, err
}

// workflowServiceForIntegration keeps recovery attached to the durable
// integration task once ReviewIntegration has created it. Before that point,
// the parent task is the only useful fallback identifier.
func (a *app) workflowServiceForIntegration(parentID string) (*workflow.Service, error) {
	service, err := a.workflowService()
	if err == nil || strings.TrimSpace(parentID) == "" {
		return service, err
	}
	taskID := parentID
	if root, discoverErr := task.DiscoverRepository(a.cwd); discoverErr == nil {
		integrationID := task.IntegrationTaskID(parentID)
		if _, loadErr := task.NewStore(root).Load(integrationID); loadErr == nil || !errors.Is(loadErr, task.ErrNotFound) {
			taskID = integrationID
		}
	}
	return nil, &taskIDError{err: err, taskID: taskID}
}

func prepareAdapters(cfg config.Config, root string, registry *runner.Registry) (config.Config, map[string]runner.Adapter, map[string]error) {
	if cfg.Providers == nil {
		cfg.Providers = map[string]config.Provider{}
	}
	adapters := map[string]runner.Adapter{}
	errorsByProvider := map[string]error{}
	for _, name := range registry.Names() {
		provider := cfg.Provider(name)
		adapter, resolved, err := registry.Build(name, provider.CLIPath, root)
		if err != nil {
			errorsByProvider[name] = err
			continue
		}
		provider.CLIPath = resolved
		adapters[name] = adapter
		cfg.Providers[name] = provider
	}
	return cfg, adapters, errorsByProvider
}

type options struct {
	values      map[string]string
	bools       map[string]bool
	seen        map[string]bool
	positionals []string
}

func parse(args []string, spec map[string]bool) (options, error) {
	result := options{values: map[string]string{}, bools: map[string]bool{}, seen: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			result.positionals = append(result.positionals, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "--") {
			result.positionals = append(result.positionals, arg)
			continue
		}
		name, value, hasValue := strings.Cut(arg, "=")
		expectsValue, ok := spec[name]
		if !ok {
			return result, usage("unknown option %s", name)
		}
		if result.seen[name] {
			return result, usage("option %s may be specified only once", name)
		}
		result.seen[name] = true
		if expectsValue {
			if !hasValue {
				if i+1 >= len(args) {
					return result, usage("option %s requires a value", name)
				}
				i++
				value = args[i]
			}
			result.values[name] = value
			continue
		}
		if hasValue {
			return result, usage("boolean option %s does not accept a value", name)
		}
		result.bools[name] = true
	}
	return result, nil
}

func (o options) value(name string) string { return o.values[name] }
func (o options) bool(name string) bool    { return o.bools[name] }
func (o options) present(name string) bool { return o.seen[name] }
func (o options) json() bool               { return o.bool("--json") }

type commandError struct {
	code string
	text string
	exit int
}

func (e *commandError) Error() string { return e.text }

type taskIDError struct {
	err    error
	taskID string
}

func (e *taskIDError) Error() string { return e.err.Error() }
func (e *taskIDError) Unwrap() error { return e.err }

func usage(format string, args ...any) error {
	return &commandError{code: "USAGE", text: fmt.Sprintf(format, args...), exit: workflow.ExitUsage}
}

func configProblem(err error) error {
	code := "CONFIGURATION"
	if errors.Is(err, config.ErrConfigConflict) {
		code = "CONFIG_CONFLICT"
	}
	if errors.Is(err, install.ErrConflict) {
		code = "INSTALL_CONFLICT"
	}
	return &commandError{code: code, text: err.Error(), exit: workflow.ExitUsage}
}

func action(code, message string) error {
	return &commandError{code: code, text: message, exit: workflow.ExitAction}
}

type taskSummary struct {
	ID                    string `json:"id"`
	Phase                 string `json:"phase"`
	Round                 int    `json:"round"`
	MaxRounds             int    `json:"max_rounds"`
	PlanRound             int    `json:"plan_round,omitempty"`
	CodeRound             int    `json:"code_round,omitempty"`
	Scope                 string `json:"scope,omitempty"`
	PendingQuestion       string `json:"pending_question,omitempty"`
	PendingQuestionSource string `json:"pending_question_source,omitempty"`
	ParentTaskID          string `json:"parent_task_id,omitempty"`
	WorkUnitID            string `json:"work_unit_id,omitempty"`
	IntegrationReview     bool   `json:"integration_review,omitempty"`
	WorkGraph             bool   `json:"work_graph,omitempty"`
	Complexity            string `json:"complexity,omitempty"`
	DirectImplementation  bool   `json:"direct_implementation,omitempty"`
}

type operationSummary struct {
	Operation    string    `json:"operation"`
	Role         string    `json:"role"`
	OwnerPID     int       `json:"owner_pid,omitempty"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	KnownSession bool      `json:"known_session"`
	SessionID    string    `json:"session_id,omitempty"`
	Loop         string    `json:"loop,omitempty"`
}

type statusSummary struct {
	workflow.Control
	ID                    string                          `json:"id"`
	Phase                 string                          `json:"phase"`
	PlanRound             int                             `json:"plan_round,omitempty"`
	CodeRound             int                             `json:"code_round,omitempty"`
	Scope                 string                          `json:"scope,omitempty"`
	PendingQuestion       string                          `json:"pending_question,omitempty"`
	PendingQuestionSource string                          `json:"pending_question_source,omitempty"`
	Findings              []task.Finding                  `json:"findings,omitempty"`
	Profiles              map[string]task.ProfileSnapshot `json:"profiles,omitempty"`
	Usage                 map[string]task.TokenUsage      `json:"usage,omitempty"`
	InFlight              *operationSummary               `json:"in_flight,omitempty"`
	Retry                 *operationSummary               `json:"retry,omitempty"`
	UpdatedAt             time.Time                       `json:"updated_at"`
	ParentTaskID          string                          `json:"parent_task_id,omitempty"`
	WorkUnitID            string                          `json:"work_unit_id,omitempty"`
	IntegrationReview     bool                            `json:"integration_review,omitempty"`
	WorkGraph             bool                            `json:"work_graph,omitempty"`
	Complexity            string                          `json:"complexity,omitempty"`
	DirectImplementation  bool                            `json:"direct_implementation,omitempty"`
}

func compactStatus(st task.State) statusSummary {
	result := statusSummary{
		Control: workflow.ControlFor(st),
		ID:      st.ID, Phase: st.Phase, PlanRound: st.PlanRound, CodeRound: st.CodeRound,
		Scope: st.Scope, PendingQuestion: st.PendingQuestion, PendingQuestionSource: st.PendingQuestionSource,
		Findings: st.Findings, Profiles: st.ProfilesSnapshot, Usage: st.Usage, UpdatedAt: st.UpdatedAt,
		ParentTaskID: st.ParentTaskID, WorkUnitID: st.WorkUnitID, IntegrationReview: st.IntegrationReview, WorkGraph: st.WorkGraph,
		Complexity: st.Complexity, DirectImplementation: st.DirectImplementation,
	}
	if st.InFlight != nil {
		result.InFlight = &operationSummary{Operation: st.InFlight.Operation, Role: st.InFlight.Role, OwnerPID: st.InFlight.OwnerPID, StartedAt: st.InFlight.StartedAt, KnownSession: st.InFlight.KnownSession, SessionID: st.InFlight.SessionID, Loop: st.InFlight.Loop}
	}
	if st.Retry != nil {
		result.Retry = &operationSummary{Operation: st.Retry.Operation, Role: st.Retry.Role, StartedAt: st.Retry.CreatedAt, KnownSession: st.Retry.KnownSession, SessionID: st.Retry.SessionID, Loop: st.Retry.Loop}
	}
	return result
}

type usageNumbers struct {
	task.TokenUsage
	UncachedInputTokens int64 `json:"uncached_input_tokens,omitempty"`
}

type roleUsage struct {
	Role     string `json:"role"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	Speed    string `json:"speed,omitempty"`
	usageNumbers
}

type usageSummary struct {
	TaskID string       `json:"task_id"`
	Phase  string       `json:"phase"`
	Roles  []roleUsage  `json:"roles"`
	Totals usageNumbers `json:"totals"`
}

func summarizeUsage(st task.State) usageSummary {
	roles := make([]string, 0, len(st.Usage))
	for role := range st.Usage {
		roles = append(roles, role)
	}
	sort.Strings(roles)

	summary := usageSummary{TaskID: st.ID, Phase: st.Phase, Roles: make([]roleUsage, 0, len(roles))}
	var totals task.TokenUsage
	for _, role := range roles {
		u := st.Usage[role]
		profile := st.ProfilesSnapshot[role]
		summary.Roles = append(summary.Roles, roleUsage{
			Role: role, Provider: profile.Provider, Model: profile.Model, Effort: profile.Effort, Speed: profile.Speed,
			usageNumbers: usageNumbersFor(u, profile.Provider),
		})
		totals.Add(u)
	}
	// Mixed providers can use different cache accounting. Sum the already
	// normalized per-role uncached values rather than guessing from totals.
	summary.Totals = usageNumbersFor(totals, "")
	summary.Totals.UncachedInputTokens = 0
	for _, role := range summary.Roles {
		summary.Totals.UncachedInputTokens += role.UncachedInputTokens
	}
	return summary
}

func usageNumbersFor(u task.TokenUsage, provider string) usageNumbers {
	uncached := u.InputTokens
	// OpenAI/Copilot report cached input as a subset of input. Claude and
	// Antigravity report cache reads as a separate counter.
	if provider != "claude" && provider != "antigravity" {
		uncached -= u.CachedInputTokens
		if uncached < 0 {
			uncached = 0
		}
	}
	return usageNumbers{TokenUsage: u, UncachedInputTokens: uncached}
}

func usageLabel(u task.TokenUsage) string {
	if u.UnreportedRequests > 0 && u.IncompleteRequests == 0 && u.UnreportedRequests >= u.Requests {
		return "tokens are unreported"
	}
	if u.UnreportedRequests > 0 || u.IncompleteRequests > 0 {
		return "incomplete reported totals"
	}
	return ""
}

type errorOutput struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	TaskID    string `json:"task_id,omitempty"`
}

type commandOutput struct {
	OK         bool              `json:"ok"`
	Command    string            `json:"command,omitempty"`
	Task       *taskSummary      `json:"task,omitempty"`
	Result     any               `json:"result,omitempty"`
	Advisories []task.Diagnostic `json:"advisories,omitempty"`
	Error      *errorOutput      `json:"error,omitempty"`
}

func summarize(st task.State) *taskSummary {
	if st.ID == "" {
		return nil
	}
	return &taskSummary{ID: st.ID, Phase: st.Phase, Round: st.Round, MaxRounds: workflow.ControlFor(st).MaxRounds, PlanRound: st.PlanRound, CodeRound: st.CodeRound, Scope: st.Scope, PendingQuestion: st.PendingQuestion, PendingQuestionSource: st.PendingQuestionSource, ParentTaskID: st.ParentTaskID, WorkUnitID: st.WorkUnitID, IntegrationReview: st.IntegrationReview, WorkGraph: st.WorkGraph, Complexity: st.Complexity, DirectImplementation: st.DirectImplementation}
}

func (a *app) workflowResult(command string, result workflow.Result, err error, jsonMode bool) int {
	if err != nil {
		return a.fail(command, err, jsonMode, result)
	}
	if jsonMode {
		return a.success(command, compactWorkflowResult(result, nil), &result.State, result.State.Advisories, true)
	}
	fmt.Fprintf(a.out, "%s\t%s\t%s\n", result.State.ID, result.State.Phase, result.Status)
	for _, advisory := range result.State.Advisories {
		fmt.Fprintf(a.errOut, "warning: %s: %s\n", advisory.Code, advisory.Message)
	}
	return workflow.ExitOK
}

func (a *app) success(command string, result any, st *task.State, advisories []task.Diagnostic, jsonMode bool) int {
	if !jsonMode {
		return workflow.ExitOK
	}
	if advisories == nil {
		advisories = []task.Diagnostic{}
	}
	payload := commandOutput{OK: true, Command: command, Result: result, Advisories: advisories}
	if st != nil {
		payload.Task = summarize(*st)
	}
	if err := encodeOne(a.out, payload); err != nil {
		fmt.Fprintf(a.errOut, "rolemux: encode result: %v\n", err)
		return workflow.ExitAction
	}
	return workflow.ExitOK
}

func (a *app) fail(command string, err error, jsonMode bool, result workflow.Result) int {
	if err == nil {
		err = action("INTERNAL", "unknown error")
	}
	code, message, retryable, taskID, exit := classifyError(err)
	if taskID == "" {
		taskID = result.State.ID
	}
	if result.State.ID == "" && taskID != "" {
		result = a.loadFailureState(result, taskID)
	}
	if jsonMode {
		payload := commandOutput{OK: false, Command: command, Error: &errorOutput{Code: code, Message: message, Retryable: retryable, TaskID: taskID}}
		if result.State.ID != "" {
			payload.Task = summarize(result.State)
			payload.Advisories = result.State.Advisories
			payload.Result = compactWorkflowResult(result, err)
		}
		if encodeErr := encodeOne(a.out, payload); encodeErr != nil {
			fmt.Fprintf(a.errOut, "rolemux: encode error: %v\n", encodeErr)
			return workflow.ExitAction
		}
	} else {
		fmt.Fprintf(a.errOut, "rolemux: %s: %s\n", code, message)
		if code == "NEEDS_INPUT" && result.State.ID != "" {
			fmt.Fprintf(a.out, "%s\t%s\n", result.State.ID, result.State.PendingQuestion)
		}
	}
	return exit
}

func compactWorkflowResult(result workflow.Result, err error) workflow.Control {
	control := workflow.ControlFor(result.State)
	if err != nil {
		status := statusForError(err)
		if status == "" {
			status = "failed"
		}
		control.Status = status
		return control
	}
	if result.Status != "" {
		control.Status = result.Status
	}
	return control
}

func statusForError(err error) string {
	code, _, _, _, exit := classifyError(err)
	switch code {
	case "NEEDS_INPUT":
		return "needs_input"
	case "REVIEW_NEEDED":
		return "review_needed"
	case "REVIEW_NO_PROGRESS":
		return "no_progress"
	case "REVIEW_EXHAUSTED":
		return "exhausted"
	case "OPERATION_IN_FLIGHT":
		return "in_flight"
	}
	switch exit {
	case workflow.ExitNeedsInput:
		return "needs_input"
	case workflow.ExitReviewNeeded:
		return "review_needed"
	case workflow.ExitInFlight:
		return "in_flight"
	case workflow.ExitExhausted:
		return "exhausted"
	case workflow.ExitAction:
		return "failed"
	default:
		return ""
	}
}

func (a *app) loadFailureState(result workflow.Result, taskID string) workflow.Result {
	root, err := task.DiscoverRepository(a.cwd)
	if err != nil {
		return result
	}
	state, err := task.NewStore(root).Load(taskID)
	if err != nil {
		return result
	}
	result.State = state
	return result
}

func classifyError(err error) (code, message string, retryable bool, taskID string, exit int) {
	var taskErr *taskIDError
	if errors.As(err, &taskErr) {
		code, message, retryable, _, exit := classifyError(taskErr.err)
		return code, message, retryable, taskErr.taskID, exit
	}
	var workflowErr *workflow.Error
	if errors.As(err, &workflowErr) {
		return workflowErr.Code, workflowErr.Error(), workflowErr.Retryable, workflowErr.TaskID, workflow.ExitCode(err)
	}
	var commandErr *commandError
	if errors.As(err, &commandErr) {
		return commandErr.code, commandErr.text, false, "", commandErr.exit
	}
	return "INTERNAL", err.Error(), false, "", workflow.ExitAction
}

func encodeOne(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func printState(out io.Writer, st task.State) {
	control := workflow.ControlFor(st)
	maxRounds := strconv.Itoa(control.MaxRounds)
	if control.MaxRounds == 0 {
		maxRounds = "unlimited"
	}
	fmt.Fprintf(out, "task: %s\nphase: %s\nplan rounds: %d/%s\ncode rounds: %d/%s\nnext action: %s\n", st.ID, st.Phase, st.PlanRound, maxRounds, st.CodeRound, maxRounds, control.NextAction)
	if st.Complexity != "" {
		fmt.Fprintf(out, "complexity: %s\n", st.Complexity)
	}
	if st.ParentTaskID != "" {
		fmt.Fprintf(out, "parent: %s\n", st.ParentTaskID)
	}
	if st.WorkUnitID != "" {
		fmt.Fprintf(out, "work unit: %s\n", st.WorkUnitID)
	}
	if st.IntegrationReview {
		fmt.Fprintln(out, "integration review: true")
	}
	if st.Scope != "" {
		fmt.Fprintf(out, "scope: %s\n", st.Scope)
	}
	if st.PendingQuestion != "" {
		fmt.Fprintf(out, "question (%s): %s\n", st.PendingQuestionSource, st.PendingQuestion)
	}
	if st.InFlight != nil {
		fmt.Fprintf(out, "in flight: %s (%s)", st.InFlight.Operation, st.InFlight.Role)
		if st.InFlight.OwnerPID > 0 {
			fmt.Fprintf(out, " owner_pid=%d", st.InFlight.OwnerPID)
		}
		fmt.Fprintln(out)
	}
	if st.Retry != nil {
		fmt.Fprintf(out, "retry: %s (%s)\n", st.Retry.Operation, st.Retry.Role)
	}
	roles := make([]string, 0, len(st.Usage))
	for role := range st.Usage {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		u := st.Usage[role]
		fmt.Fprintf(out, "usage %s: requests=%d prompt_bytes=%d input=%d cached=%d cache_write=%d output=%d reasoning=%d total=%d unreported=%d incomplete=%d", role, u.Requests, u.PromptBytes, u.InputTokens, u.CachedInputTokens, u.CacheWriteTokens, u.OutputTokens, u.ReasoningTokens, u.TotalTokens, u.UnreportedRequests, u.IncompleteRequests)
		if label := usageLabel(u); label != "" {
			fmt.Fprintf(out, " (%s)", label)
		}
		fmt.Fprintln(out)
	}
}

func printWorkGraph(out io.Writer, graph workflow.WorkGraph) {
	fmt.Fprintf(out, "task: %s\nphase: %s\n", graph.TaskID, graph.Phase)
	if graph.Complexity != "" {
		fmt.Fprintf(out, "complexity: %s\n", graph.Complexity)
	}
	for index, wave := range graph.Waves {
		fmt.Fprintf(out, "wave %d: %s\n", index+1, strings.Join(wave, ", "))
	}
	for _, node := range graph.Nodes {
		fmt.Fprintf(out, "%s\t%s\t%s\tscope=%s", node.ID, node.Status, node.TaskID, node.Scope)
		if len(node.BlockedBy) > 0 {
			fmt.Fprintf(out, "\tblocked_by=%s", strings.Join(node.BlockedBy, ","))
		}
		fmt.Fprintln(out)
	}
}

func printUsage(out io.Writer, summary usageSummary) {
	fmt.Fprintf(out, "task: %s\nphase: %s\n", summary.TaskID, summary.Phase)
	for _, role := range summary.Roles {
		profile := role.Provider + "/" + role.Model
		if role.Provider == "" && role.Model == "" {
			profile = "unknown"
		}
		if role.Effort != "" {
			profile += " (" + role.Effort + ")"
		}
		if role.Speed != "" {
			profile += " [" + role.Speed + "]"
		}
		fmt.Fprintf(out, "%s [%s]: requests=%d input=%d cached=%d uncached=%d output=%d reasoning=%d total=%d prompt_bytes=%d unreported=%d incomplete=%d",
			role.Role, profile, role.Requests, role.InputTokens, role.CachedInputTokens,
			role.UncachedInputTokens, role.OutputTokens, role.ReasoningTokens,
			role.TotalTokens, role.PromptBytes, role.UnreportedRequests, role.IncompleteRequests)
		if label := usageLabel(role.TokenUsage); label != "" {
			fmt.Fprintf(out, " (%s)", label)
		}
		fmt.Fprintln(out)
	}
	t := summary.Totals
	fmt.Fprintf(out, "total: requests=%d input=%d cached=%d uncached=%d output=%d reasoning=%d total=%d prompt_bytes=%d unreported=%d incomplete=%d",
		t.Requests, t.InputTokens, t.CachedInputTokens, t.UncachedInputTokens,
		t.OutputTokens, t.ReasoningTokens, t.TotalTokens, t.PromptBytes, t.UnreportedRequests, t.IncompleteRequests)
	if label := usageLabel(t.TokenUsage); label != "" {
		fmt.Fprintf(out, " (%s)", label)
	}
	fmt.Fprintln(out)
}

func (a *app) optionalRepo() string {
	root, err := task.DiscoverRepository(a.cwd)
	if err != nil {
		return ""
	}
	return root
}

func configTarget(root string, global, project bool, environ []string) (string, error) {
	if explicit := config.ExplicitConfigPath(environ); explicit != "" {
		return explicit, nil
	}
	globalPath, projectPath := config.ConfigPaths(root, environ)
	if global {
		if globalPath == "" {
			return "", errors.New("global configuration path is unavailable; HOME is required")
		}
		return globalPath, nil
	}
	if project {
		if projectPath == "" {
			return "", errors.New("project configuration requires a Git worktree")
		}
		return projectPath, nil
	}
	return "", errors.New("configuration target is required")
}

func (a *app) readConfigSource(name string) ([]byte, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("--from path must not be empty")
	}
	var reader io.Reader
	if name == "-" {
		reader = a.in
	} else {
		file, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		reader = file
	}
	limited := io.LimitReader(reader, 4<<20+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(data) > 4<<20 {
		return nil, errors.New("configuration fragment exceeds 4 MiB")
	}
	return data, nil
}

func containsFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name || strings.HasPrefix(arg, name+"=") {
			return true
		}
	}
	return false
}

func containsHelpFlag(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func (a *app) runnerRegistry() *runner.Registry {
	if a.runners == nil {
		a.runners = runner.BuiltinRegistry()
	}
	return a.runners
}

func isInteractive(reader io.Reader) bool {
	file, ok := reader.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

type doctorCheck struct {
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Required bool   `json:"required,omitempty"`
	Path     string `json:"path,omitempty"`
	Version  string `json:"version,omitempty"`
	Message  string `json:"message,omitempty"`
}

func (a *app) doctorResult(checks []doctorCheck, ready bool, exit int, jsonMode bool) int {
	if jsonMode {
		payload := commandOutput{OK: ready, Command: "doctor", Result: map[string]any{"ready": ready, "checks": checks}, Advisories: []task.Diagnostic{}}
		if !ready {
			code := "DOCTOR_FAILED"
			if exit == workflow.ExitUsage {
				code = "CONFIGURATION"
			}
			payload.Error = &errorOutput{Code: code, Message: "RoleMux is not ready for the configured workflow", Retryable: exit == workflow.ExitAction}
		}
		if err := encodeOne(a.out, payload); err != nil {
			fmt.Fprintf(a.errOut, "rolemux: encode doctor result: %v\n", err)
			return workflow.ExitAction
		}
		return exit
	}
	for _, check := range checks {
		status := "ok"
		if !check.OK {
			status = "fail"
		}
		detail := check.Version
		if detail == "" {
			detail = check.Path
		}
		if check.Message != "" {
			if detail != "" {
				detail += ": "
			}
			detail += check.Message
		}
		fmt.Fprintf(a.out, "%-5s %-24s %s\n", status, check.Name, detail)
	}
	return exit
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func skillMessage(err error, data []byte) string {
	if errors.Is(err, os.ErrNotExist) {
		return "not installed; run rolemux install --global --hosts all"
	}
	if err != nil {
		return err.Error()
	}
	if !bytes.Equal(data, install.Content()) {
		return "installed skill differs; reinstall with --force after reviewing it"
	}
	return ""
}
