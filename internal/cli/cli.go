// Package cli implements RoleMux's small, stable command surface. Provider
// output never reaches stdout: JSON mode emits exactly one object and human
// mode keeps diagnostics on stderr.
package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/basant-kumar/rolemux/internal/catalog"
	"github.com/basant-kumar/rolemux/internal/config"
	"github.com/basant-kumar/rolemux/internal/install"
	"github.com/basant-kumar/rolemux/internal/picker"
	"github.com/basant-kumar/rolemux/internal/runner"
	"github.com/basant-kumar/rolemux/internal/task"
	"github.com/basant-kumar/rolemux/internal/workflow"
)

const usageText = `RoleMux — Right model. Right role. Same thread.

Usage:
  rolemux version [--json]
  rolemux models [--refresh] [--runner codex|claude|copilot] [--json]
  rolemux configure [--global|--project] [--from PATH|-]
                    [--role planner|implementer|reviewer|plan-reviewer|code-reviewer]
                    [--runner PROVIDER] [--model MODEL] [--effort EFFORT] [--json]
  rolemux plan start --task TEXT [--id TASK-ID] [--json]
  rolemux plan answer TASK-ID --answer TEXT [--json]
  rolemux plan review TASK-ID [--json]
  rolemux implement TASK-ID [--scope PATH[,PATH...]] [--json]
  rolemux implement answer TASK-ID --answer TEXT [--json]
  rolemux code review TASK-ID [--json]
  rolemux status TASK-ID [--json]
  rolemux usage TASK-ID [--json]
  rolemux retry TASK-ID [--json]
  rolemux list [--json]
  rolemux doctor [--json]
  rolemux install --global --hosts all|claude,codex,copilot [--force] [--json]
`

type app struct {
	ctx     context.Context
	in      io.Reader
	out     io.Writer
	errOut  io.Writer
	version string
	cwd     string
	environ []string
	runners *runner.Registry
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
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		if jsonMode {
			return a.success("help", map[string]any{"usage": usageText}, nil, nil, true)
		}
		fmt.Fprint(a.out, usageText)
		return workflow.ExitOK
	}
	switch args[0] {
	case "version":
		return a.runVersion(args[1:])
	case "models":
		return a.runModels(args[1:])
	case "configure":
		return a.runConfigure(args[1:])
	case "plan":
		return a.runPlan(args[1:])
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
	cat := catalog.New(adapters, cfg, "")
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
	opts, err := parse(args, map[string]bool{
		"--json": false, "--global": false, "--project": false, "--from": true,
		"--role": true, "--runner": true, "--model": true, "--effort": true,
	})
	if err != nil || len(opts.positionals) != 0 {
		if err == nil {
			err = usage("configure accepts no positional arguments")
		}
		return a.fail("configure", err, opts.json(), workflow.Result{})
	}
	if opts.bool("--global") && opts.bool("--project") {
		return a.fail("configure", usage("--global and --project are mutually exclusive"), opts.json(), workflow.Result{})
	}
	direct := opts.present("--role") || opts.present("--runner") || opts.present("--model") || opts.present("--effort")
	from := opts.present("--from")
	if from && direct {
		return a.fail("configure", usage("--from is mutually exclusive with role selection flags"), opts.json(), workflow.Result{})
	}
	interactive := !from && !direct
	if opts.json() && interactive {
		return a.fail("configure", usage("--json requires --from or explicit role selection"), true, workflow.Result{})
	}
	if !opts.bool("--global") && !opts.bool("--project") && !interactive {
		return a.fail("configure", usage("select exactly one of --global or --project"), opts.json(), workflow.Result{})
	}
	if interactive && !isInteractive(a.in) {
		return a.fail("configure", usage("interactive configure requires a terminal, or pass --role/--from and a target"), opts.json(), workflow.Result{})
	}
	root := a.optionalRepo()
	if interactive && !opts.bool("--global") && !opts.bool("--project") {
		if root == "" {
			return a.fail("configure", usage("interactive project configuration requires a Git worktree"), false, workflow.Result{})
		}
		ok, askErr := a.confirm(fmt.Sprintf("Configure this project (%s)?", root))
		if askErr != nil {
			return a.fail("configure", askErr, false, workflow.Result{})
		}
		if !ok {
			return a.fail("configure", usage("configuration cancelled"), false, workflow.Result{})
		}
		opts.bools["--project"] = true
	}
	target, err := configTarget(root, opts.bool("--global"), opts.bool("--project"), a.environ)
	if err != nil {
		return a.fail("configure", usage("%v", err), opts.json(), workflow.Result{})
	}
	before, err := config.FileHash(target)
	if err != nil {
		return a.fail("configure", configProblem(err), opts.json(), workflow.Result{})
	}
	if from {
		data, readErr := a.readConfigSource(opts.value("--from"))
		if readErr != nil {
			return a.fail("configure", configProblem(readErr), opts.json(), workflow.Result{})
		}
		if err := config.ImportConfigAtomic(target, data, before); err != nil {
			return a.fail("configure", configProblem(err), opts.json(), workflow.Result{})
		}
		return a.configureSuccess(target, "imported", opts.json())
	}
	if direct {
		role, roleErr := config.CanonicalRole(opts.value("--role"))
		if roleErr != nil || !opts.present("--runner") || !opts.present("--model") {
			if roleErr == nil {
				roleErr = usage("--role, --runner, and --model are required together")
			}
			return a.fail("configure", roleErr, opts.json(), workflow.Result{})
		}
		profile := config.Profile{Provider: strings.ToLower(opts.value("--runner")), Model: opts.value("--model"), Effort: opts.value("--effort")}
		if err := config.ConfigureProfile(target, role, profile, before); err != nil {
			return a.fail("configure", configProblem(err), opts.json(), workflow.Result{})
		}
		return a.configureSuccess(target, "updated", opts.json())
	}
	profiles, pickErr := a.pickProfiles(root)
	if pickErr != nil {
		return a.fail("configure", pickErr, false, workflow.Result{})
	}
	if err := config.ConfigureProfilesAtomic(target, profiles, before); err != nil {
		return a.fail("configure", configProblem(err), false, workflow.Result{})
	}
	return a.configureSuccess(target, "updated", false)
}

func (a *app) configureSuccess(path, status string, jsonMode bool) int {
	result := map[string]string{"status": status, "path": path}
	if jsonMode {
		return a.success("configure", result, nil, nil, true)
	}
	fmt.Fprintf(a.out, "%s %s\n", status, path)
	return workflow.ExitOK
}

func (a *app) pickProfiles(root string) (map[string]config.Profile, error) {
	cfg, err := config.LoadWithEnv(root, a.environ)
	if err != nil {
		return nil, configProblem(err)
	}
	cfg, adapters, _ := prepareAdapters(cfg, root, a.runnerRegistry())
	cat := catalog.New(adapters, cfg, "")
	profiles := map[string]config.Profile{}
	roles := []string{config.RolePlanner, config.RoleImplementer, config.RoleReviewer}
	for _, role := range roles {
		profile, pickErr := a.pickProfile(role, cfg, cat)
		if pickErr != nil {
			return nil, pickErr
		}
		profiles[role] = profile
	}
	for _, role := range []string{config.RolePlanReviewer, config.RoleCodeReviewer} {
		ok, askErr := a.confirm("Configure a separate " + strings.ReplaceAll(role, "_", " ") + "?")
		if askErr != nil {
			return nil, askErr
		}
		if !ok {
			continue
		}
		profile, pickErr := a.pickProfile(role, cfg, cat)
		if pickErr != nil {
			return nil, pickErr
		}
		profiles[role] = profile
	}
	return profiles, nil
}

func (a *app) pickProfile(role string, cfg config.Config, cat *catalog.Catalog) (config.Profile, error) {
	fmt.Fprintf(a.out, "\n%s provider\n", strings.ReplaceAll(role, "_", " "))
	providerChoice, cancelled, err := picker.Pick(a.ctx, a.in, a.out, providerOptions(a.runnerRegistry().Names()))
	fmt.Fprintln(a.out)
	if err != nil {
		return config.Profile{}, err
	}
	if cancelled {
		return config.Profile{}, usage("configuration cancelled")
	}
	runtime := workflow.RuntimeSnapshot(providerChoice.ID, cfg.Provider(providerChoice.ID))
	models, err := cat.Models(a.ctx, providerChoice.ID, false, runner.ModelListRequest{Runtime: runtime})
	if err != nil {
		return config.Profile{}, action("MODEL_DISCOVERY_FAILED", err.Error())
	}
	fmt.Fprintf(a.out, "%s model\n", strings.ReplaceAll(role, "_", " "))
	modelChoice, cancelled, err := picker.Pick(a.ctx, a.in, a.out, picker.ModelOptions(models))
	fmt.Fprintln(a.out)
	if err != nil || cancelled {
		if err != nil {
			return config.Profile{}, err
		}
		return config.Profile{}, usage("configuration cancelled")
	}
	var selected runner.ModelInfo
	for _, model := range models {
		if model.ID == modelChoice.ID {
			selected = model
			break
		}
	}
	if warning := picker.UnknownAvailabilityWarning(selected); warning != "" {
		ok, askErr := a.confirm(warning + ". Select it anyway?")
		if askErr != nil {
			return config.Profile{}, askErr
		}
		if !ok {
			return config.Profile{}, usage("configuration cancelled")
		}
	}
	effort := ""
	if len(selected.Efforts) > 0 {
		fmt.Fprintf(a.out, "%s effort\n", strings.ReplaceAll(role, "_", " "))
		effortChoice, cancelled, pickErr := picker.Pick(a.ctx, a.in, a.out, picker.EffortOptions(selected))
		fmt.Fprintln(a.out)
		if pickErr != nil {
			return config.Profile{}, pickErr
		}
		if cancelled {
			return config.Profile{}, usage("configuration cancelled")
		}
		effort = effortChoice.ID
	}
	return config.Profile{Provider: providerChoice.ID, Model: modelChoice.ID, Effort: effort}, nil
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
		service, err := a.workflowService()
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
		service, err := a.workflowService()
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
		service, err := a.workflowService()
		if err != nil {
			return a.fail("plan-review", err, opts.json(), workflow.Result{})
		}
		result, callErr := service.ReviewPlan(a.ctx, opts.positionals[0])
		return a.workflowResult("plan-review", result, callErr, opts.json())
	default:
		return a.fail("plan", usage("unknown plan command %q", args[0]), jsonMode, workflow.Result{})
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
		service, err := a.workflowService()
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
	service, err := a.workflowService()
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
	service, err := a.workflowService()
	if err != nil {
		return a.fail("code-review", err, opts.json(), workflow.Result{})
	}
	result, callErr := service.ReviewCode(a.ctx, opts.positionals[0])
	return a.workflowResult("code-review", result, callErr, opts.json())
}

func (a *app) runStatus(args []string) int {
	opts, err := parse(args, map[string]bool{"--json": false})
	if err != nil || len(opts.positionals) != 1 {
		if err == nil {
			err = usage("status requires TASK-ID")
		}
		return a.fail("status", err, opts.json(), workflow.Result{})
	}
	service, err := a.workflowService()
	if err != nil {
		return a.fail("status", err, opts.json(), workflow.Result{})
	}
	state, loadErr := service.Status(opts.positionals[0])
	if loadErr != nil {
		return a.fail("status", loadErr, opts.json(), workflow.Result{})
	}
	if opts.json() {
		return a.success("status", state, &state, state.Advisories, true)
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
	service, err := a.workflowService()
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
	service, err := a.workflowService()
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
		return a.success("list", map[string]any{"tasks": states}, nil, nil, true)
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
	if root != "" {
		store := task.NewStore(root)
		_, stateErr := store.List()
		checks = append(checks, doctorCheck{Name: "private_state", OK: stateErr == nil, Path: store.Dir, Message: errorText(stateErr)})
		if stateErr != nil {
			ready = false
		}
	}
	home, homeErr := os.UserHomeDir()
	if homeErr == nil {
		for _, name := range []string{"claude", "codex", "copilot"} {
			path := filepath.Join(home, "."+name, "skills", "rolemux", "SKILL.md")
			data, readErr := os.ReadFile(path)
			checks = append(checks, doctorCheck{Name: "skill_" + name, OK: readErr == nil && bytes.Equal(data, install.Content()), Required: false, Path: path, Message: skillMessage(readErr, data)})
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
	return workflow.New(root, cfg, adapters), nil
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
	PlanRound             int    `json:"plan_round,omitempty"`
	CodeRound             int    `json:"code_round,omitempty"`
	Scope                 string `json:"scope,omitempty"`
	PendingQuestion       string `json:"pending_question,omitempty"`
	PendingQuestionSource string `json:"pending_question_source,omitempty"`
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
			Role: role, Provider: profile.Provider, Model: profile.Model, Effort: profile.Effort,
			usageNumbers: usageNumbersFor(u),
		})
		totals.Add(u)
	}
	summary.Totals = usageNumbersFor(totals)
	return summary
}

func usageNumbersFor(u task.TokenUsage) usageNumbers {
	uncached := u.InputTokens - u.CachedInputTokens
	if uncached < 0 {
		uncached = 0
	}
	return usageNumbers{TokenUsage: u, UncachedInputTokens: uncached}
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
	return &taskSummary{ID: st.ID, Phase: st.Phase, Round: st.Round, PlanRound: st.PlanRound, CodeRound: st.CodeRound, Scope: st.Scope, PendingQuestion: st.PendingQuestion, PendingQuestionSource: st.PendingQuestionSource}
}

func (a *app) workflowResult(command string, result workflow.Result, err error, jsonMode bool) int {
	if err != nil {
		return a.fail(command, err, jsonMode, result)
	}
	if jsonMode {
		return a.success(command, map[string]string{"status": result.Status}, &result.State, result.State.Advisories, true)
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
	if jsonMode {
		payload := commandOutput{OK: false, Command: command, Error: &errorOutput{Code: code, Message: message, Retryable: retryable, TaskID: taskID}}
		if result.State.ID != "" {
			payload.Task = summarize(result.State)
			payload.Advisories = result.State.Advisories
			if code == "NEEDS_INPUT" {
				payload.Result = map[string]string{"status": "needs_input", "question": result.State.PendingQuestion, "source": result.State.PendingQuestionSource}
			}
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

func classifyError(err error) (code, message string, retryable bool, taskID string, exit int) {
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
	fmt.Fprintf(out, "task: %s\nphase: %s\nplan rounds: %d/%d\ncode rounds: %d/%d\n", st.ID, st.Phase, st.PlanRound, st.MaxRounds, st.CodeRound, st.MaxRounds)
	if st.Scope != "" {
		fmt.Fprintf(out, "scope: %s\n", st.Scope)
	}
	if st.PendingQuestion != "" {
		fmt.Fprintf(out, "question (%s): %s\n", st.PendingQuestionSource, st.PendingQuestion)
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
		fmt.Fprintf(out, "usage %s: requests=%d prompt_bytes=%d input=%d cached=%d cache_write=%d output=%d reasoning=%d total=%d\n", role, u.Requests, u.PromptBytes, u.InputTokens, u.CachedInputTokens, u.CacheWriteTokens, u.OutputTokens, u.ReasoningTokens, u.TotalTokens)
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
		fmt.Fprintf(out, "%s [%s]: requests=%d input=%d cached=%d uncached=%d output=%d reasoning=%d total=%d prompt_bytes=%d\n",
			role.Role, profile, role.Requests, role.InputTokens, role.CachedInputTokens,
			role.UncachedInputTokens, role.OutputTokens, role.ReasoningTokens,
			role.TotalTokens, role.PromptBytes)
	}
	t := summary.Totals
	fmt.Fprintf(out, "total: requests=%d input=%d cached=%d uncached=%d output=%d reasoning=%d total=%d prompt_bytes=%d\n",
		t.Requests, t.InputTokens, t.CachedInputTokens, t.UncachedInputTokens,
		t.OutputTokens, t.ReasoningTokens, t.TotalTokens, t.PromptBytes)
}

func (a *app) optionalRepo() string {
	root, err := task.DiscoverRepository(a.cwd)
	if err != nil {
		return ""
	}
	return root
}

func configTarget(root string, global, project bool, environ []string) (string, error) {
	globalPath, projectPath := config.ConfigPaths(root, environ)
	if global {
		if globalPath == "" {
			return "", errors.New("global configuration path is unavailable; HOME or XDG_CONFIG_HOME is required")
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

func (a *app) runnerRegistry() *runner.Registry {
	if a.runners == nil {
		a.runners = runner.BuiltinRegistry()
	}
	return a.runners
}

func providerOptions(names []string) []picker.Option {
	labels := map[string]string{"codex": "Codex", "claude": "Claude Code", "copilot": "GitHub Copilot"}
	options := make([]picker.Option, 0, len(names))
	for _, name := range names {
		label := labels[name]
		if label == "" {
			label = name
		}
		options = append(options, picker.Option{ID: name, Label: label})
	}
	return options
}

func isInteractive(reader io.Reader) bool {
	file, ok := reader.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func (a *app) confirm(prompt string) (bool, error) {
	fmt.Fprintf(a.out, "%s [y/N] ", prompt)
	line, err := bufio.NewReader(a.in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
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
