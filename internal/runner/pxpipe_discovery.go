package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const pxpipeInstallCommand = "npm install --global pxpipe-proxy"

func missingPXPipeDiagnostic(provider string) string {
	return fmt.Sprintf("pxpipe: optional helper is not installed; running %s directly. To enable token-saving transport, run `%s` once and ensure `pxpipe` is on PATH", provider, pxpipeInstallCommand)
}

// DetectPXPipePath discovers only an executable. Discovery is not evidence of
// provider authentication, a supported route, or model eligibility.
func DetectPXPipePath(environ []string) string {
	path := environmentValue(environ, "PXPIPE_CLI_PATH")
	if path != "" {
		if executableFile(path) {
			return path
		}
		return ""
	}
	for _, directory := range filepath.SplitList(environmentValue(environ, "PATH")) {
		if directory == "" {
			continue
		}
		candidate := filepath.Join(directory, "pxpipe")
		if executableFile(candidate) {
			return candidate
		}
	}
	return ""
}

func executableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0
}

func environmentValue(environ []string, key string) string {
	for index := len(environ) - 1; index >= 0; index-- {
		name, value, ok := strings.Cut(environ[index], "=")
		if ok && name == key {
			return value
		}
	}
	return ""
}
