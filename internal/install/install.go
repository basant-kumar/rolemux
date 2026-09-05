// Package install installs RoleMux's portable host skill without overwriting
// user content unless replacement is explicitly requested.
package install

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed assets/SKILL.md
var skill []byte

var ErrConflict = errors.New("existing RoleMux skill differs")

type Result struct {
	Host   string `json:"host"`
	Path   string `json:"path"`
	Status string `json:"status"`
}

var destinations = map[string][]string{
	"claude":      {".claude", "skills", "rolemux", "SKILL.md"},
	"codex":       {".agents", "skills", "rolemux", "SKILL.md"},
	"copilot":     {".copilot", "skills", "rolemux", "SKILL.md"},
	"antigravity": {".gemini", "antigravity-cli", "skills", "rolemux", "SKILL.md"},
}

func Content() []byte { return append([]byte(nil), skill...) }

func ParseHosts(raw string) ([]string, error) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "all" {
		hosts := make([]string, 0, len(destinations))
		for host := range destinations {
			hosts = append(hosts, host)
		}
		sort.Strings(hosts)
		return hosts, nil
	}
	seen := map[string]bool{}
	for _, item := range strings.Split(raw, ",") {
		host := strings.TrimSpace(item)
		if _, ok := destinations[host]; !ok {
			return nil, fmt.Errorf("unknown host %q", host)
		}
		seen[host] = true
	}
	if len(seen) == 0 {
		return nil, errors.New("at least one host is required")
	}
	hosts := make([]string, 0, len(seen))
	for host := range seen {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts, nil
}

func InstallGlobal(home string, hosts []string, force bool) ([]Result, error) {
	if strings.TrimSpace(home) == "" || !filepath.IsAbs(home) {
		return nil, errors.New("absolute home directory is required")
	}
	results := make([]Result, 0, len(hosts))
	for _, host := range hosts {
		parts, ok := destinations[host]
		if !ok {
			return results, fmt.Errorf("unknown host %q", host)
		}
		destination := filepath.Join(append([]string{home}, parts...)...)
		status, err := installOne(destination, force)
		if err != nil {
			return results, fmt.Errorf("install %s skill: %w", host, err)
		}
		results = append(results, Result{Host: host, Path: destination, Status: status})
	}
	return results, nil
}

func installOne(destination string, force bool) (string, error) {
	mode := os.FileMode(0o600)
	existing, err := os.ReadFile(destination)
	switch {
	case err == nil && bytes.Equal(existing, skill):
		return "unchanged", nil
	case err == nil && !force:
		return "", fmt.Errorf("%w: %s", ErrConflict, destination)
	case err == nil:
		if info, statErr := os.Lstat(destination); statErr == nil && info.Mode().IsRegular() {
			mode = info.Mode().Perm()
		}
	case errors.Is(err, os.ErrNotExist):
		// Create below.
	default:
		return "", err
	}

	dir := filepath.Dir(destination)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".rolemux-skill-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(mode); err == nil {
		_, err = tmp.Write(skill)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, destination); err != nil {
		return "", err
	}
	if dirHandle, openErr := os.Open(dir); openErr == nil {
		_ = dirHandle.Sync()
		_ = dirHandle.Close()
	}
	if existing != nil {
		return "replaced", nil
	}
	return "installed", nil
}
