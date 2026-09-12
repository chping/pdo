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
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	supportedSchemaVersion = 1
	maxAPIResponse         = 4 << 20
	usage                  = `Usage:
  pdo download dotfiles [--<name> ...]
  pdo upload dotfiles [--<name> ...]`
)

var (
	githubAPIBase     = "https://api.github.com"
	dotfileNameRegexp = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

type config struct {
	SchemaVersion int                      `json:"schema_version"`
	GitHub        githubConfig             `json:"github"`
	Dotfiles      map[string]dotfileConfig `json:"dotfiles"`
}

type githubConfig struct {
	PATEnv string `json:"pat_env"`
}

type dotfileConfig struct {
	Remote string `json:"remote"`
	Local  string `json:"local"`
}

type preparedDotfile struct {
	local      string
	repository string
	branch     string
	path       string
}

type githubClient struct {
	http       *http.Client
	token      string
	repository string
	branch     string
	path       string
	message    string
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
	if len(args) < 2 || (args[0] != "download" && args[0] != "upload") || args[1] != "dotfiles" {
		fmt.Fprintln(stderr, usage)
		return 2
	}

	selectors := make([]string, 0, len(args)-2)
	seen := make(map[string]bool)
	for _, arg := range args[2:] {
		if !strings.HasPrefix(arg, "--") || len(arg) == 2 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		name := strings.TrimPrefix(arg, "--")
		if !seen[name] {
			selectors = append(selectors, name)
			seen[name] = true
		}
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
	prepared, err := cfg.prepareDotfiles(home)
	if err != nil {
		fmt.Fprintf(stderr, "pdo: %v\n", err)
		return 1
	}
	if len(selectors) == 0 {
		for name := range prepared {
			selectors = append(selectors, name)
		}
		sort.Strings(selectors)
	} else {
		for _, name := range selectors {
			if _, ok := prepared[name]; !ok {
				fmt.Fprintf(stderr, "pdo: unknown dotfile selector --%s\n", name)
				return 2
			}
		}
	}

	token := os.Getenv(cfg.GitHub.PATEnv)
	if token == "" {
		fmt.Fprintf(stderr, "pdo: environment variable %s is empty\n", cfg.GitHub.PATEnv)
		return 1
	}

	failed := 0
	for _, name := range selectors {
		item := prepared[name]
		client := githubClient{
			http:       &http.Client{Timeout: 30 * time.Second},
			token:      token,
			repository: item.repository,
			branch:     item.branch,
			path:       item.path,
			message:    "pdo: upload dotfiles --" + name,
		}
		var message string
		if args[0] == "download" {
			message, err = downloadDotfile(item.local, &client)
		} else {
			message, err = uploadDotfile(item.local, &client)
		}
		if err != nil {
			fmt.Fprintf(stderr, "pdo: %s: %v\n", name, err)
			failed++
			continue
		}
		fmt.Fprintf(stdout, "%s: %s\n", name, message)
	}
	fmt.Fprintf(stdout, "pdo: %d succeeded, %d failed\n", len(selectors)-failed, failed)
	if failed != 0 {
		return 1
	}
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

func (cfg config) prepareDotfiles(home string) (map[string]preparedDotfile, error) {
	if cfg.GitHub.PATEnv == "" {
		return nil, fmt.Errorf("github.pat_env is required")
	}
	if len(cfg.Dotfiles) == 0 {
		return nil, fmt.Errorf("dotfiles must contain at least one entry")
	}
	prepared := make(map[string]preparedDotfile, len(cfg.Dotfiles))
	for name, item := range cfg.Dotfiles {
		if name == "help" || !dotfileNameRegexp.MatchString(name) {
			return nil, fmt.Errorf("dotfiles key %q must be kebab-case and must not be help", name)
		}
		repository, branch, remotePath, err := parseRemote(item.Remote)
		if err != nil {
			return nil, fmt.Errorf("dotfiles.%s.remote: %w", name, err)
		}
		local, err := expandLocalPath(home, item.Local)
		if err != nil {
			return nil, fmt.Errorf("dotfiles.%s.local: %w", name, err)
		}
		prepared[name] = preparedDotfile{
			local:      local,
			repository: repository,
			branch:     branch,
			path:       remotePath,
		}
	}
	return prepared, nil
}

func parseRemote(value string) (string, string, string, error) {
	remote, err := url.Parse(value)
	if err != nil {
		return "", "", "", fmt.Errorf("invalid URL: %w", err)
	}
	if remote.Scheme != "https" || remote.Host != "api.github.com" || remote.User != nil || remote.Fragment != "" {
		return "", "", "", fmt.Errorf("must be an https://api.github.com Contents API URL without credentials or fragment")
	}
	query := remote.Query()
	refs, ok := query["ref"]
	if len(query) != 1 || !ok || len(refs) != 1 || refs[0] == "" {
		return "", "", "", fmt.Errorf("must contain exactly one non-empty ref query parameter")
	}
	parts := strings.Split(strings.TrimPrefix(remote.Path, "/"), "/")
	if len(parts) < 5 || parts[0] != "repos" || parts[3] != "contents" || invalidPathPart(parts[1]) || invalidPathPart(parts[2]) {
		return "", "", "", fmt.Errorf("must match /repos/OWNER/REPOSITORY/contents/PATH")
	}
	for _, part := range parts[4:] {
		if invalidPathPart(part) {
			return "", "", "", fmt.Errorf("content path must not contain empty, . or .. segments")
		}
	}
	return parts[1] + "/" + parts[2], refs[0], strings.Join(parts[4:], "/"), nil
}

func invalidPathPart(part string) bool {
	return part == "" || part == "." || part == ".."
}

func expandLocalPath(home, value string) (string, error) {
	if strings.HasPrefix(value, "~/") {
		return filepath.Clean(filepath.Join(home, filepath.FromSlash(value[2:]))), nil
	}
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("must start with ~/ or be an absolute path")
	}
	return filepath.Clean(value), nil
}

func downloadDotfile(localPath string, client *githubClient) (string, error) {
	remote, _, exists, err := client.get()
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("remote file was not found or is inaccessible")
	}

	targetPath, info, err := inspectLocalFile(localPath)
	if err != nil {
		return "", err
	}
	var current []byte
	mode := os.FileMode(0o600)
	if info != nil {
		current, err = os.ReadFile(targetPath)
		if err != nil {
			return "", fmt.Errorf("read local file: %w", err)
		}
		if bytes.Equal(current, remote) {
			return "already up to date", nil
		}
		mode = info.Mode().Perm()
	} else if err := os.MkdirAll(filepath.Dir(localPath), 0o700); err != nil {
		return "", fmt.Errorf("create local directory: %w", err)
	}

	backup, err := replaceLocalFile(targetPath, current, remote, info != nil, mode)
	if err != nil {
		return "", err
	}
	if backup != "" {
		return fmt.Sprintf("downloaded to %s (backup: %s)", localPath, backup), nil
	}
	return fmt.Sprintf("downloaded to %s", localPath), nil
}

func uploadDotfile(localPath string, client *githubClient) (string, error) {
	targetPath, info, err := inspectLocalFile(localPath)
	if err != nil {
		return "", err
	}
	if info == nil {
		return "", fmt.Errorf("local file does not exist: %s", localPath)
	}
	local, err := os.ReadFile(targetPath)
	if err != nil {
		return "", fmt.Errorf("read local file: %w", err)
	}
	remote, sha, remoteExists, err := client.get()
	if err != nil {
		return "", err
	}
	if remoteExists && bytes.Equal(local, remote) {
		return "already up to date", nil
	}
	if err := client.put(local, sha); err != nil {
		return "", err
	}
	return "uploaded", nil
}

func inspectLocalFile(path string) (string, os.FileInfo, error) {
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return path, nil, nil
		}
		return "", nil, fmt.Errorf("inspect local file: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, fmt.Errorf("resolve local file: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", nil, fmt.Errorf("inspect local file target: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("local path is not a regular file: %s", path)
	}
	return resolved, info, nil
}

func replaceLocalFile(path string, oldData, newData []byte, existed bool, mode os.FileMode) (string, error) {
	backup := ""
	if existed {
		backup = path + ".pdo-backup-" + time.Now().UTC().Format("20060102T150405.000000000Z")
		if err := writeExclusive(backup, oldData, mode); err != nil {
			return "", fmt.Errorf("create backup: %w", err)
		}
	}

	temp, err := os.CreateTemp(filepath.Dir(path), ".pdo-dotfile-*")
	if err != nil {
		return backup, fmt.Errorf("create temporary file: %w", err)
	}
	tempPath := temp.Name()
	keepTemp := true
	defer func() {
		temp.Close()
		if keepTemp {
			os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(mode); err != nil {
		return backup, fmt.Errorf("set temporary file permissions: %w", err)
	}
	if _, err := temp.Write(newData); err != nil {
		return backup, fmt.Errorf("write temporary file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return backup, fmt.Errorf("sync temporary file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return backup, fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		if runtime.GOOS != "windows" || !existed {
			return backup, fmt.Errorf("replace local file: %w", err)
		}
		if removeErr := os.Remove(path); removeErr != nil {
			return backup, fmt.Errorf("replace local file: %w", err)
		}
		if renameErr := os.Rename(tempPath, path); renameErr != nil {
			if restoreErr := os.WriteFile(path, oldData, mode); restoreErr != nil {
				return backup, fmt.Errorf("replace local file: %v; restore failed: %v; backup: %s", renameErr, restoreErr, backup)
			}
			return backup, fmt.Errorf("replace local file: %w", renameErr)
		}
	}
	keepTemp = false
	return backup, nil
}

func writeExclusive(path string, data []byte, mode os.FileMode) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		os.Remove(path)
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
		return nil, "", false, fmt.Errorf("decode remote file: %w", err)
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
		Message: client.message,
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
		return fmt.Errorf("remote file changed during upload")
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
