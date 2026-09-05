package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/basant-kumar/rolemux/internal/task"
)

type codexRPC struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

type codexInitializeParams struct {
	ClientInfo struct {
		Name    string `json:"name"`
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"clientInfo"`
}

type codexModelListParams struct {
	Limit         int    `json:"limit"`
	IncludeHidden bool   `json:"includeHidden"`
	Cursor        string `json:"cursor,omitempty"`
}

type codexModelListResult struct {
	Data []struct {
		ID                        string `json:"id"`
		Model                     string `json:"model"`
		DisplayName               string `json:"displayName"`
		DefaultReasoningEffort    string `json:"defaultReasoningEffort"`
		SupportedReasoningEfforts []struct {
			ReasoningEffort string `json:"reasoningEffort"`
		} `json:"supportedReasoningEfforts"`
		Hidden    bool `json:"hidden"`
		IsDefault bool `json:"isDefault"`
	} `json:"data"`
	NextCursor *string `json:"nextCursor"`
}

func (c *Codex) listModelsAppServer(ctx context.Context, runtime task.RuntimeSnapshot) (ModelPage, error) {
	path, pathErr := executableForRequest(c.Path, runtime.CLIPath)
	if pathErr != nil {
		return ModelPage{}, providerError("CODEX_UNAVAILABLE", pathErr.Error(), false, false, "", pathErr)
	}
	env, err := runtimeEnvironment(c.Env, runtime.AuthEnvRefs, "", "")
	if err != nil {
		return ModelPage{}, providerError("CODEX_AUTH", err.Error(), false, false, "", err)
	}
	routing, routeErr := CodexConfigOverrides(runtime)
	if routeErr != nil {
		return ModelPage{}, providerError("CODEX_ROUTING", routeErr.Error(), false, false, "", routeErr)
	}
	args := append(append([]string(nil), routing...), "app-server")
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return ModelPage{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return ModelPage{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return ModelPage{}, err
	}
	if err := cmd.Start(); err != nil {
		return ModelPage{}, err
	}
	// Drain stderr from the start. App-server can block on a full stderr pipe
	// even while stdout is carrying protocol messages.
	stderrDone := make(chan error, 1)
	go func() { _, e := io.Copy(io.Discard, io.LimitReader(stderr, 8<<20)); stderrDone <- e }()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	send := func(message any) error {
		b, e := json.Marshal(message)
		if e != nil {
			return e
		}
		_, e = io.WriteString(stdin, string(b)+"\n")
		return e
	}
	waitID := func(want int) (codexRPC, error) {
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			var message codexRPC
			if e := json.Unmarshal(line, &message); e != nil {
				return codexRPC{}, fmt.Errorf("decode codex app-server message: %w", e)
			}
			var id int
			if len(message.ID) > 0 {
				_ = json.Unmarshal(message.ID, &id)
			}
			if id == want {
				if len(message.Error) > 0 && string(message.Error) != "null" {
					return message, fmt.Errorf("codex app-server request %d failed: %s", want, message.Error)
				}
				return message, nil
			}
		}
		if err := scanner.Err(); err != nil {
			return codexRPC{}, err
		}
		return codexRPC{}, io.EOF
	}
	var initParams codexInitializeParams
	initParams.ClientInfo.Name, initParams.ClientInfo.Title, initParams.ClientInfo.Version = "rolemux", "RoleMux", "dev"
	if err := send(struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{0, "initialize", initParams}); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return ModelPage{}, err
	}
	initResponse, err := waitID(0)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return ModelPage{}, err
	}
	account := extractAccount(initResponse.Result)
	if err := send(struct {
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}{"initialized", map[string]any{}}); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return ModelPage{}, err
	}
	// Bind the cache to the authenticated account without persisting the raw
	// identifier. Older compatible servers may omit this method; discovery can
	// still proceed, but no account-specific identifier will be claimed.
	if err := send(struct {
		ID     int            `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}{1, "account/read", map[string]any{"refreshToken": false}}); err == nil {
		if response, accountErr := waitID(1); accountErr == nil {
			if found := extractAccount(response.Result); found != "" {
				account = found
			}
		}
	}
	var all []ModelInfo
	cursor := ""
	endpoint := runtime.Endpoint
	for id := 2; ; id++ {
		params := codexModelListParams{Limit: 100, IncludeHidden: false, Cursor: cursor}
		if err := send(struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
			Params any    `json:"params"`
		}{id, "model/list", params}); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return ModelPage{Account: account, Endpoint: endpoint}, err
		}
		response, e := waitID(id)
		if e != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return ModelPage{Account: account, Endpoint: endpoint}, e
		}
		var page codexModelListResult
		if e := json.Unmarshal(response.Result, &page); e != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return ModelPage{Account: account, Endpoint: endpoint}, e
		}
		for _, model := range page.Data {
			if model.Hidden {
				continue
			}
			info := ModelInfo{ID: model.ID, Label: model.DisplayName, Provider: "codex", Origin: "live", Availability: "available", Efforts: make([]string, 0, len(model.SupportedReasoningEfforts))}
			if info.ID == "" {
				info.ID = model.Model
			}
			for _, effort := range model.SupportedReasoningEfforts {
				info.Efforts = append(info.Efforts, effort.ReasoningEffort)
			}
			info.DefaultEffort = model.DefaultReasoningEffort
			all = append(all, info)
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		if *page.NextCursor == cursor {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return ModelPage{Account: account, Endpoint: endpoint}, errors.New("codex app-server cursor did not advance")
		}
		cursor = *page.NextCursor
	}
	_ = stdin.Close()
	waitErr := cmd.Wait()
	<-stderrDone
	if waitErr != nil {
		return ModelPage{Account: account, Endpoint: endpoint}, waitErr
	}
	return ModelPage{Models: all, Account: account, Endpoint: endpoint}, nil
}

func extractAccount(raw json.RawMessage) string {
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	for _, key := range []string{"account_id", "accountId", "user_id", "userId", "login"} {
		if result, ok := value[key].(string); ok {
			return result
		}
	}
	if account, ok := value["account"].(map[string]any); ok {
		for _, key := range []string{"id", "login", "email", "name", "type"} {
			if result, ok := account[key].(string); ok {
				return result
			}
		}
	}
	return ""
}

var _ = strings.TrimSpace
