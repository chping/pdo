package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	supportedSchemaVersion = 1
	maxAPIResponse         = 4 << 20
	usage                  = `Usage:
  pdo download ssh-config
  pdo upload ssh-config`
)

var githubAPIBase = "https://api.github.com"

type config struct {
	SchemaVersion int             `json:"schema_version"`
	GitHub        githubConfig    `json:"github"`
	SSHConfig     sshConfigConfig `json:"ssh_config"`
}

type githubConfig struct {
	PATEnv string `json:"pat_env"`
}

type sshConfigConfig struct {
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	Path       string `json:"path"`
}

type githubClient struct {
	http       *http.Client
	token      string
	repository string
	branch     string
	path       string
}

type githubFile struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
	SHA      string `json:"sha"`
	Type     string `json:"type"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	if len(args) != 2 || (args[0] != "download" && args[0] != "upload") || args[1] != "ssh-config" {
		fmt.Fprintln(stderr, usage)
		return 2
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "pdo: find home directory: %v\n", err)
		return 1
	}
	cfg, err := loadConfig(filepath.Join(home, ".config", "pdo", "config.json"))
	if err != nil {
		fmt.Fprintf(stderr, "pdo: %v\n", err)
		return 1
	}
	if err := cfg.validateSSHConfig(); err != nil {
		fmt.Fprintf(stderr, "pdo: %v\n", err)
		return 1
	}
	token := os.Getenv(cfg.GitHub.PATEnv)
	if token == "" {
		fmt.Fprintf(stderr, "pdo: environment variable %s is empty\n", cfg.GitHub.PATEnv)
		return 1
	}

	client := githubClient{
		http:       &http.Client{Timeout: 30 * time.Second},
		token:      token,
		repository: cfg.SSHConfig.Repository,
		branch:     cfg.SSHConfig.Branch,
		path:       cfg.SSHConfig.Path,
	}

	var message string
	if args[0] == "download" {
		message, err = downloadSSHConfig(home, &client)
	} else {
		message, err = uploadSSHConfig(home, &client)
	}
	if err != nil {
		fmt.Fprintf(stderr, "pdo: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, message)
	return 0
}

func loadConfig(path string) (config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.SchemaVersion != supportedSchemaVersion {
		return config{}, fmt.Errorf("unsupported schema_version %d (expected %d)", cfg.SchemaVersion, supportedSchemaVersion)
	}
	return cfg, nil
}

func (cfg config) validateSSHConfig() error {
	if cfg.GitHub.PATEnv == "" {
		return fmt.Errorf("github.pat_env is required")
	}
	if cfg.SSHConfig.Repository == "" {
		return fmt.Errorf("ssh_config.repository is required")
	}
	parts := strings.Split(cfg.SSHConfig.Repository, "/")
	if len(parts) != 2 || invalidPathPart(parts[0]) || invalidPathPart(parts[1]) {
		return fmt.Errorf("ssh_config.repository must be OWNER/REPOSITORY")
	}
	if cfg.SSHConfig.Branch == "" {
		return fmt.Errorf("ssh_config.branch is required")
	}
	if cfg.SSHConfig.Path == "" || strings.HasPrefix(cfg.SSHConfig.Path, "/") {
		return fmt.Errorf("ssh_config.path must be a relative repository path")
	}
	for _, part := range strings.Split(cfg.SSHConfig.Path, "/") {
		if invalidPathPart(part) {
			return fmt.Errorf("ssh_config.path must not contain empty, . or .. segments")
		}
	}
	return nil
}

func invalidPathPart(part string) bool {
	return part == "" || part == "." || part == ".."
}

func downloadSSHConfig(home string, client *githubClient) (string, error) {
	remote, _, exists, err := client.get()
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("remote ssh_config was not found or is inaccessible")
	}

	requestedPath := filepath.Join(home, ".ssh", "config")
	targetPath, localExists, err := inspectLocalFile(requestedPath)
	if err != nil {
		return "", err
	}
	var current []byte
	if localExists {
		current, err = os.ReadFile(targetPath)
		if err != nil {
			return "", fmt.Errorf("read local ssh config: %w", err)
		}
		if bytes.Equal(current, remote) {
			return "ssh_config is already up to date", nil
		}
	} else if err := os.MkdirAll(filepath.Dir(requestedPath), 0o700); err != nil {
		return "", fmt.Errorf("create .ssh directory: %w", err)
	}

	backup, err := replaceLocalFile(targetPath, current, remote, localExists)
	if err != nil {
		return "", err
	}
	if backup != "" {
		return fmt.Sprintf("Downloaded ssh_config to %s (backup: %s)", requestedPath, backup), nil
	}
	return fmt.Sprintf("Downloaded ssh_config to %s", requestedPath), nil
}

func uploadSSHConfig(home string, client *githubClient) (string, error) {
	requestedPath := filepath.Join(home, ".ssh", "config")
	targetPath, exists, err := inspectLocalFile(requestedPath)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("local ssh config does not exist: %s", requestedPath)
	}
	local, err := os.ReadFile(targetPath)
	if err != nil {
		return "", fmt.Errorf("read local ssh config: %w", err)
	}
	remote, sha, remoteExists, err := client.get()
	if err != nil {
		return "", err
	}
	if remoteExists && bytes.Equal(local, remote) {
		return "ssh_config is already up to date", nil
	}
	if err := client.put(local, sha); err != nil {
		return "", err
	}
	return "Uploaded ssh_config", nil
}

func inspectLocalFile(path string) (string, bool, error) {
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return path, false, nil
		}
		return "", false, fmt.Errorf("inspect local ssh config: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false, fmt.Errorf("resolve local ssh config: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", false, fmt.Errorf("inspect local ssh config target: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("local ssh config is not a regular file: %s", path)
	}
	return resolved, true, nil
}

func replaceLocalFile(path string, oldData, newData []byte, existed bool) (string, error) {
	backup := ""
	if existed {
		backup = path + ".pdo-backup-" + time.Now().UTC().Format("20060102T150405.000000000Z")
		if err := writeExclusive(backup, oldData); err != nil {
			return "", fmt.Errorf("create backup: %w", err)
		}
	}

	temp, err := os.CreateTemp(filepath.Dir(path), ".pdo-config-*")
	if err != nil {
		return backup, fmt.Errorf("create temporary ssh config: %w", err)
	}
	tempPath := temp.Name()
	keepTemp := true
	defer func() {
		temp.Close()
		if keepTemp {
			os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return backup, fmt.Errorf("set temporary ssh config permissions: %w", err)
	}
	if _, err := temp.Write(newData); err != nil {
		return backup, fmt.Errorf("write temporary ssh config: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return backup, fmt.Errorf("sync temporary ssh config: %w", err)
	}
	if err := temp.Close(); err != nil {
		return backup, fmt.Errorf("close temporary ssh config: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		if runtime.GOOS != "windows" || !existed {
			return backup, fmt.Errorf("replace local ssh config: %w", err)
		}
		if removeErr := os.Remove(path); removeErr != nil {
			return backup, fmt.Errorf("replace local ssh config: %w", err)
		}
		if renameErr := os.Rename(tempPath, path); renameErr != nil {
			if restoreErr := os.WriteFile(path, oldData, 0o600); restoreErr != nil {
				return backup, fmt.Errorf("replace local ssh config: %v; restore failed: %v; backup: %s", renameErr, restoreErr, backup)
			}
			return backup, fmt.Errorf("replace local ssh config: %w", renameErr)
		}
	}
	keepTemp = false
	return backup, nil
}

func writeExclusive(path string, data []byte) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		file.Close()
		if !ok {
			os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func (client *githubClient) get() ([]byte, string, bool, error) {
	request, err := client.request(http.MethodGet, nil, true)
	if err != nil {
		return nil, "", false, err
	}
	response, err := client.http.Do(request)
	if err != nil {
		return nil, "", false, fmt.Errorf("GitHub request failed: %w", err)
	}
	defer response.Body.Close()
	body, err := readAPIResponse(response.Body)
	if err != nil {
		return nil, "", false, err
	}
	if response.StatusCode == http.StatusNotFound {
		return nil, "", false, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, "", false, client.apiError(response.StatusCode, body)
	}
	var file githubFile
	if err := json.Unmarshal(body, &file); err != nil {
		return nil, "", false, fmt.Errorf("decode GitHub response: %w", err)
	}
	if file.Type != "file" || file.Encoding != "base64" || file.SHA == "" {
		return nil, "", false, fmt.Errorf("GitHub response is not a base64 file")
	}
	content, err := base64.StdEncoding.DecodeString(file.Content)
	if err != nil {
		return nil, "", false, fmt.Errorf("decode remote ssh_config: %w", err)
	}
	return content, file.SHA, true, nil
}

func (client *githubClient) put(content []byte, sha string) error {
	payload := struct {
		Message string `json:"message"`
		Content string `json:"content"`
		SHA     string `json:"sha,omitempty"`
		Branch  string `json:"branch"`
	}{
		Message: "pdo: upload ssh_config",
		Content: base64.StdEncoding.EncodeToString(content),
		SHA:     sha,
		Branch:  client.branch,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode GitHub request: %w", err)
	}
	request, err := client.request(http.MethodPut, body, false)
	if err != nil {
		return err
	}
	response, err := client.http.Do(request)
	if err != nil {
		return fmt.Errorf("GitHub request failed: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := readAPIResponse(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusOK || response.StatusCode == http.StatusCreated {
		return nil
	}
	if response.StatusCode == http.StatusConflict {
		return fmt.Errorf("remote ssh_config changed during upload")
	}
	return client.apiError(response.StatusCode, responseBody)
}

func (client *githubClient) request(method string, body []byte, includeRef bool) (*http.Request, error) {
	endpoint, err := url.Parse(strings.TrimRight(githubAPIBase, "/"))
	if err != nil {
		return nil, fmt.Errorf("build GitHub URL: %w", err)
	}
	parts := strings.Split(client.repository, "/")
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/repos/" + parts[0] + "/" + parts[1] + "/contents/" + client.path
	if includeRef {
		query := endpoint.Query()
		query.Set("ref", client.branch)
		endpoint.RawQuery = query.Encode()
	}
	request, err := http.NewRequest(method, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create GitHub request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	request.Header.Set("User-Agent", "pdo")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request, nil
}

func readAPIResponse(reader io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maxAPIResponse+1))
	if err != nil {
		return nil, fmt.Errorf("read GitHub response: %w", err)
	}
	if len(body) > maxAPIResponse {
		return nil, fmt.Errorf("GitHub response is too large")
	}
	return body, nil
}

func (client *githubClient) apiError(status int, body []byte) error {
	var response struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &response) != nil || response.Message == "" {
		return fmt.Errorf("GitHub API returned HTTP %d", status)
	}
	message := strings.ReplaceAll(response.Message, client.token, "[redacted]")
	return fmt.Errorf("GitHub API returned HTTP %d: %s", status, message)
}
