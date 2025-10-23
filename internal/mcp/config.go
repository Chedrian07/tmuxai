package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/alvinunreal/tmuxai/config"
)

// ConfigFilename is the expected name of the MCP configuration file.
const ConfigFilename = ".mcp.json"

// EnvConfigPath is the environment variable that can be used to point to a custom MCP config file.
const EnvConfigPath = "TMUXAI_MCP_CONFIG"

// Config represents the contents of the MCP configuration file.
type Config struct {
	Servers map[string]ServerConfig `json:"mcpServers"`
}

// ServerConfig represents the configuration for an individual MCP server.
type ServerConfig struct {
	Command  string            `json:"command"`
	Args     []string          `json:"args"`
	Env      map[string]string `json:"env"`
	Timeout  int               `json:"timeout"`
	Cwd      string            `json:"cwd"`
	Disabled bool              `json:"disabled"`
}

// ErrConfigNotFound indicates that no MCP configuration file was found.
var ErrConfigNotFound = errors.New("mcp config not found")

// loadConfigFile reads and unmarshals the MCP configuration from the supplied path.
func loadConfigFile(path string) (*Config, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := &Config{}
	if err := json.Unmarshal(content, cfg); err != nil {
		return nil, fmt.Errorf("failed to decode MCP config %s: %w", path, err)
	}

	if len(cfg.Servers) == 0 {
		return nil, fmt.Errorf("MCP config %s does not contain any servers", path)
	}

	return cfg, nil
}

// findConfigPath resolves the configuration file path based on environment variables and defaults.
func findConfigPath() (string, error) {
	if envPath := os.Getenv(EnvConfigPath); envPath != "" {
		if _, err := os.Stat(envPath); err == nil {
			return envPath, nil
		}
		return "", fmt.Errorf("environment variable %s points to %s which is not accessible: %w", EnvConfigPath, envPath, ErrConfigNotFound)
	}

	paths := []string{}

	if wd, err := os.Getwd(); err == nil {
		paths = append(paths, filepath.Join(wd, ConfigFilename))
	}

	if cfgDir, err := config.GetConfigDir(); err == nil {
		paths = append(paths, filepath.Join(cfgDir, ConfigFilename))
	}

	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ConfigFilename))
	}

	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	return "", ErrConfigNotFound
}

// LoadConfig loads the MCP configuration from the available locations.
// Returns ErrConfigNotFound if the file cannot be located.
func LoadConfig() (*Config, string, error) {
	path, err := findConfigPath()
	if err != nil {
		if errors.Is(err, ErrConfigNotFound) {
			return nil, "", ErrConfigNotFound
		}
		return nil, "", err
	}

	cfg, err := loadConfigFile(path)
	if err != nil {
		return nil, "", err
	}

	return cfg, path, nil
}
