// Package config holds process-level configuration read from the environment.
// Everything that should change without a restart lives in the settings table instead.
package config

import (
	"os"
	"strings"
)

const (
	// ServiceName is the display name. The spec lists it as a placeholder.
	ServiceName = "GithubPushBuilder"
	// DefaultRepoScript is the only contract with a built repo. The spec lists
	// the path as a placeholder; override with GPB_REPO_SCRIPT.
	DefaultRepoScript = ".our_container_dir/our_container_script.sh"
)

type Config struct {
	DataDir        string // GPB_DATA_DIR, default /data
	Listen         string // GPB_LISTEN, default :8080
	BaseURL        string // GPB_BASE_URL, public URL used in links (dashboard setting wins)
	AdminPassword  string // GPB_ADMIN_PASSWORD, used only when no password is stored yet
	RepoScript     string // GPB_REPO_SCRIPT
	GitHubAPI      string // GPB_GITHUB_API_URL, default https://api.github.com
	InsecureCookie bool   // GPB_INSECURE_COOKIE=1 drops the Secure flag (plain-http testing)
	TrustProxy     bool   // GPB_TRUST_PROXY=1 takes the client IP from X-Forwarded-For
}

func FromEnv() Config {
	return Config{
		DataDir:        env("GPB_DATA_DIR", "/data"),
		Listen:         env("GPB_LISTEN", ":8080"),
		BaseURL:        strings.TrimRight(os.Getenv("GPB_BASE_URL"), "/"),
		AdminPassword:  os.Getenv("GPB_ADMIN_PASSWORD"),
		RepoScript:     env("GPB_REPO_SCRIPT", DefaultRepoScript),
		GitHubAPI:      strings.TrimRight(env("GPB_GITHUB_API_URL", "https://api.github.com"), "/"),
		InsecureCookie: truthy(os.Getenv("GPB_INSECURE_COOKIE")),
		TrustProxy:     truthy(os.Getenv("GPB_TRUST_PROXY")),
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func truthy(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
