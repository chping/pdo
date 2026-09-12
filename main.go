package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	supportedSchemaVersion = 1
	maxAPIResponse         = 4 << 20
	maxUpdateBinary        = 100 << 20
	usage                  = `Usage:
  pdo download dotfiles [--<name> ...]
  pdo upload dotfiles [--<name> ...]
  pdo copy-ssh-id --identity=<path> [--config=<path>]
  pdo update [--check]
  pdo version`
)

var (
	version              = "devel"
	githubAPIBase        = "https://api.github.com"
	releaseAPIBase       = "https://api.github.com/repos/chping/pdo"
	releaseDownloadBase  = "https://github.com/chping/pdo/releases/download"
	dotfileNameRegexp    = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	versionRegexp        = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	configMigrationSteps = map[int]jsonMigration{}
	findCommand          = exec.LookPath
	runCommand           = func(name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
		command := exec.Command(name, args...)
		command.Stdin = stdin
		command.Stdout = stdout
		command.Stderr = stderr
		return command.Run()
	}
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

type semanticVersion [3]uint64

type release struct {
	TagName    string         `json:"tag_name"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name string `json:"name"`
}

type updater struct {
	http         *http.Client
	apiBase      string
	downloadBase string
	version      string
	goos         string
	goarch       string
	executable   string
	home         string
}

type jsonMigration func(map[string]json.RawMessage) error

type transactionFile struct {
	Path    string `json:"path"`
	Backup  string `json:"backup,omitempty"`
	Existed bool   `json:"existed"`
	Mode    uint32 `json:"mode,omitempty"`
}

type transactionManifest struct {
	Files []transactionFile `json:"files"`
}

type migrationTransaction struct {
	dir      string
	manifest transactionManifest
}

type copySSHIDOptions struct {
	config   string
	identity string
}

func main() {
	args := os.Args[1:]
	if !(len(args) == 2 && args[0] == "update" && args[1] == "--check") {
		if executable, err := os.Executable(); err == nil {
			if target, _, err := inspectExecutable(executable); err == nil {
				if err := cleanupOldExecutable(target); err != nil {
					fmt.Fprintf(os.Stderr, "pdo: %v\n", err)
				}
			}
		}
	}
	os.Exit(run(args, os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 3 && args[0] == "__pdo-migrate" {
		count, err := runInternalMigrations(args[1], args[2])
		if err != nil {
			fmt.Fprintf(stderr, "pdo: migrate: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, count)
		return 0
	}
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintf(stdout, "pdo %s\n", version)
		return 0
	}
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	if len(args) >= 1 && args[0] == "copy-ssh-id" {
		options, err := parseCopySSHIDArgs(args[1:])
		if err != nil {
			fmt.Fprintf(stderr, "pdo: %v\n%s\n", err, usage)
			return 2
		}
		failed, err := copySSHID(options, stdout, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "pdo: copy-ssh-id: %v\n", err)
			return 1
		}
		if failed {
			return 1
		}
		return 0
	}
	if len(args) >= 1 && args[0] == "update" {
		if len(args) > 2 || (len(args) == 2 && args[1] != "--check") {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		if version == "devel" {
			fmt.Fprintln(stderr, "pdo: development builds cannot update")
			return 1
		}
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(stderr, "pdo: find home directory: %v\n", err)
			return 1
		}
		executable, err := os.Executable()
		if err != nil {
			fmt.Fprintf(stderr, "pdo: find executable: %v\n", err)
			return 1
		}
		u := updater{
			http:         &http.Client{Timeout: 30 * time.Second},
			apiBase:      releaseAPIBase,
			downloadBase: releaseDownloadBase,
			version:      version,
			goos:         runtime.GOOS,
			goarch:       runtime.GOARCH,
			executable:   executable,
			home:         home,
		}
		if err := u.run(len(args) == 2, stdout); err != nil {
			fmt.Fprintf(stderr, "pdo: update: %v\n", err)
			return 1
		}
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

func parseCopySSHIDArgs(args []string) (copySSHIDOptions, error) {
	var options copySSHIDOptions
	seen := make(map[string]bool)
	for index := 0; index < len(args); index++ {
		arg := args[index]
		name, value, hasValue := strings.Cut(arg, "=")
		if name != "--config" && name != "--identity" {
			return options, fmt.Errorf("invalid copy-ssh-id argument %q", arg)
		}
		if seen[name] {
			return options, fmt.Errorf("duplicate copy-ssh-id argument %s", name)
		}
		seen[name] = true
		if !hasValue {
			index++
			if index >= len(args) || strings.HasPrefix(args[index], "--") {
				return options, fmt.Errorf("%s requires a path", name)
			}
			value = args[index]
		}
		if value == "" {
			return options, fmt.Errorf("%s requires a path", name)
		}
		if name == "--config" {
			options.config = value
		} else {
			options.identity = value
		}
	}
	if options.identity == "" {
		return options, fmt.Errorf("--identity is required")
	}
	return options, nil
}

func copySSHID(options copySSHIDOptions, stdout, stderr io.Writer) (bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, fmt.Errorf("find home directory: %w", err)
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		return false, fmt.Errorf("find working directory: %w", err)
	}
	if options.config == "" {
		options.config = filepath.Join(home, ".ssh", "config")
	} else {
		options.config, err = resolveCommandPath(home, workingDirectory, options.config)
		if err != nil {
			return false, fmt.Errorf("config path: %w", err)
		}
	}
	options.identity, err = resolveCommandPath(home, workingDirectory, options.identity)
	if err != nil {
		return false, fmt.Errorf("identity path: %w", err)
	}
	if strings.HasSuffix(options.identity, ".pub") {
		options.identity = options.identity[:len(options.identity)-4]
	}
	files := []struct{ label, path string }{
		{"config", options.config},
		{"identity", options.identity},
		{"public identity key", options.identity + ".pub"},
	}
	for _, file := range files {
		if err := validateReadableFile(file.path); err != nil {
			return false, fmt.Errorf("%s %s: %w", file.label, file.path, err)
		}
	}

	hosts, err := parseSSHHosts(options.config, home)
	if err != nil {
		return false, err
	}
	if len(hosts) == 0 {
		return false, fmt.Errorf("no literal Host aliases found in %s", options.config)
	}
	ssh, err := findCommand("ssh")
	if err != nil {
		return false, fmt.Errorf("ssh was not found in PATH")
	}
	sshCopyID, err := findCommand("ssh-copy-id")
	if err != nil {
		return false, fmt.Errorf("ssh-copy-id was not found in PATH")
	}
	for _, host := range hosts {
		var commandError bytes.Buffer
		if err := runCommand(ssh, []string{"-G", "-F", options.config, host}, nil, io.Discard, &commandError); err != nil {
			message := strings.TrimSpace(commandError.String())
			if message == "" {
				message = err.Error()
			}
			return false, fmt.Errorf("validate Host %s: %s", host, message)
		}
	}

	failed := 0
	for _, host := range hosts {
		fmt.Fprintf(stdout, "pdo: %s: running ssh-copy-id\n", host)
		if err := runCommand(sshCopyID, []string{"-i", options.identity, "-F", options.config, host}, os.Stdin, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "pdo: %s: ssh-copy-id failed: %v\n", host, err)
			failed++
			continue
		}
		fmt.Fprintf(stdout, "pdo: %s: complete\n", host)
	}
	fmt.Fprintf(stdout, "pdo: %d succeeded, %d failed\n", len(hosts)-failed, failed)
	return failed != 0, nil
}

func resolveCommandPath(home, workingDirectory, value string) (string, error) {
	if value == "~" {
		return filepath.Clean(home), nil
	}
	if strings.HasPrefix(value, "~/") || (filepath.Separator == '\\' && strings.HasPrefix(value, `~\`)) {
		return filepath.Clean(filepath.Join(home, filepath.FromSlash(strings.TrimLeft(value[1:], `/\`)))), nil
	}
	if strings.HasPrefix(value, "~") {
		return "", fmt.Errorf("~user paths are not supported")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(workingDirectory, value)
	}
	return filepath.Clean(value), nil
}

func validateReadableFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	return file.Close()
}

func parseSSHHosts(configPath, home string) ([]string, error) {
	var hosts []string
	seenHosts := make(map[string]bool)
	activeFiles := make(map[string]bool)
	var parse func(string, int) error
	parse = func(path string, depth int) error {
		if depth > 32 {
			return fmt.Errorf("SSH config Include depth exceeds 32 at %s", path)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("resolve SSH config %s: %w", path, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return fmt.Errorf("inspect SSH config %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("SSH config %s is not a regular file", path)
		}
		if activeFiles[resolved] {
			return fmt.Errorf("SSH config Include cycle at %s", path)
		}
		activeFiles[resolved] = true
		defer delete(activeFiles, resolved)

		file, err := os.Open(resolved)
		if err != nil {
			return fmt.Errorf("read SSH config %s: %w", path, err)
		}
		defer file.Close()
		global := true
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		lineNumber := 0
		for scanner.Scan() {
			lineNumber++
			keyword, values, relevant, err := parseSSHDirective(scanner.Text())
			if err != nil {
				return fmt.Errorf("parse SSH config %s:%d: %w", path, lineNumber, err)
			}
			if !relevant {
				continue
			}
			switch keyword {
			case "host":
				global = false
				for _, host := range values {
					if strings.HasPrefix(host, "!") || strings.ContainsAny(host, "*?[") {
						continue
					}
					if strings.HasPrefix(host, "-") {
						return fmt.Errorf("unsafe Host alias %q", host)
					}
					key := strings.ToLower(host)
					if !seenHosts[key] {
						seenHosts[key] = true
						hosts = append(hosts, host)
					}
				}
			case "match":
				global = false
			case "include":
				if !global {
					continue
				}
				for _, pattern := range values {
					matches, err := resolveInclude(pattern, home)
					if err != nil {
						return fmt.Errorf("SSH config %s:%d: %w", path, lineNumber, err)
					}
					for _, match := range matches {
						if err := parse(match, depth+1); err != nil {
							return err
						}
					}
				}
			}
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("read SSH config %s: %w", path, err)
		}
		return nil
	}
	if err := parse(configPath, 0); err != nil {
		return nil, err
	}
	return hosts, nil
}

func parseSSHDirective(line string) (string, []string, bool, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", nil, false, nil
	}
	end := strings.IndexAny(line, " \t=")
	if end < 0 {
		keyword := strings.ToLower(line)
		if keyword == "host" || keyword == "match" || keyword == "include" {
			return "", nil, true, fmt.Errorf("%s requires an argument", keyword)
		}
		return "", nil, false, nil
	}
	keyword := strings.ToLower(line[:end])
	if keyword != "host" && keyword != "match" && keyword != "include" {
		return "", nil, false, nil
	}
	rest := strings.TrimSpace(line[end:])
	if strings.HasPrefix(rest, "=") {
		rest = strings.TrimSpace(rest[1:])
	}
	values, err := splitSSHArguments(rest)
	if err != nil {
		return "", nil, true, err
	}
	if len(values) == 0 {
		return "", nil, true, fmt.Errorf("%s requires an argument", keyword)
	}
	return keyword, values, true, nil
}

func splitSSHArguments(value string) ([]string, error) {
	var values []string
	var current strings.Builder
	quote := byte(0)
	escaped := false
	flush := func() {
		if current.Len() != 0 {
			values = append(values, current.String())
			current.Reset()
		}
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if escaped {
			current.WriteByte(character)
			escaped = false
			continue
		}
		if character == '\\' {
			if index+1 < len(value) && strings.ContainsRune(" \t#'\"\\", rune(value[index+1])) {
				escaped = true
			} else {
				current.WriteByte(character)
			}
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			} else {
				current.WriteByte(character)
			}
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			continue
		}
		if character == '#' {
			break
		}
		if character == ' ' || character == '\t' {
			flush()
			continue
		}
		current.WriteByte(character)
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote or escape")
	}
	flush()
	return values, nil
}

func resolveInclude(pattern, home string) ([]string, error) {
	if strings.ContainsAny(pattern, "$%") {
		return nil, fmt.Errorf("Include environment variables and tokens are not supported: %s", pattern)
	}
	resolved, err := resolveCommandPath(home, filepath.Join(home, ".ssh"), pattern)
	if err != nil {
		return nil, err
	}
	hasGlob := strings.ContainsAny(pattern, "*?[")
	if !hasGlob {
		if _, err := os.Stat(resolved); err != nil {
			return nil, fmt.Errorf("Include file %s: %w", resolved, err)
		}
		return []string{resolved}, nil
	}
	matches, err := filepath.Glob(resolved)
	if err != nil {
		return nil, fmt.Errorf("invalid Include glob %s: %w", pattern, err)
	}
	sort.Strings(matches)
	return matches, nil
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
	if remote.Scheme != "https" || remote.Host != "github.com" || remote.User != nil || remote.RawQuery != "" || remote.Fragment != "" {
		return "", "", "", fmt.Errorf("must be an https://github.com file URL without credentials, query, or fragment")
	}
	parts := strings.Split(strings.TrimPrefix(remote.Path, "/"), "/")
	if len(parts) < 5 || parts[2] != "blob" || invalidPathPart(parts[0]) || invalidPathPart(parts[1]) || invalidPathPart(parts[3]) {
		return "", "", "", fmt.Errorf("must match /OWNER/REPOSITORY/blob/BRANCH/PATH")
	}
	for _, part := range parts[4:] {
		if invalidPathPart(part) {
			return "", "", "", fmt.Errorf("content path must not contain empty, . or .. segments")
		}
	}
	return parts[0] + "/" + parts[1], parts[3], strings.Join(parts[4:], "/"), nil
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

func parseVersion(value string) (semanticVersion, error) {
	match := versionRegexp.FindStringSubmatch(value)
	if match == nil {
		return semanticVersion{}, fmt.Errorf("invalid stable release version %q", value)
	}
	var parsed semanticVersion
	for index := range parsed {
		part, err := strconv.ParseUint(match[index+1], 10, 64)
		if err != nil {
			return semanticVersion{}, fmt.Errorf("invalid stable release version %q", value)
		}
		parsed[index] = part
	}
	return parsed, nil
}

func compareVersions(left, right semanticVersion) int {
	for index := range left {
		if left[index] < right[index] {
			return -1
		}
		if left[index] > right[index] {
			return 1
		}
	}
	return 0
}

func (u updater) run(check bool, stdout io.Writer) error {
	current, err := parseVersion(u.version)
	if err != nil {
		return err
	}
	latestRelease, latest, err := u.latestRelease()
	if err != nil {
		return err
	}
	comparison := compareVersions(current, latest)
	if check {
		if comparison < 0 {
			fmt.Fprintf(stdout, "Update available: %s -> %s\n", u.version, latestRelease.TagName)
		} else {
			fmt.Fprintf(stdout, "pdo %s is up to date\n", u.version)
		}
		return nil
	}
	if comparison < 0 {
		return u.install(latestRelease, stdout)
	}

	tx, err := startTransaction(u.home)
	if err != nil {
		return err
	}
	count, err := runOwnedMigrations(u.home, tx)
	if err != nil {
		return finishFailedTransaction(tx, err)
	}
	if err := tx.cleanup(); err != nil {
		return fmt.Errorf("clean transaction %s: %w", tx.dir, err)
	}
	if comparison > 0 {
		fmt.Fprintf(stdout, "pdo %s is newer than latest %s\n", u.version, latestRelease.TagName)
	} else {
		fmt.Fprintf(stdout, "pdo %s is up to date\n", u.version)
	}
	if count != 0 {
		fmt.Fprintf(stdout, "Migrated %d pdo file(s)\n", count)
	}
	return nil
}

func (u updater) latestRelease() (release, semanticVersion, error) {
	body, err := u.download(strings.TrimRight(u.apiBase, "/")+"/releases/latest", maxAPIResponse)
	if err != nil {
		return release{}, semanticVersion{}, err
	}
	var found release
	if err := json.Unmarshal(body, &found); err != nil {
		return release{}, semanticVersion{}, fmt.Errorf("decode latest release: %w", err)
	}
	parsed, err := parseVersion(found.TagName)
	if err != nil {
		return release{}, semanticVersion{}, err
	}
	if found.Draft || found.Prerelease {
		return release{}, semanticVersion{}, fmt.Errorf("latest release %s is not stable", found.TagName)
	}
	return found, parsed, nil
}

func (u updater) download(address string, limit int64) ([]byte, error) {
	request, err := http.NewRequest(http.MethodGet, address, nil)
	if err != nil {
		return nil, fmt.Errorf("create update request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "pdo")
	response, err := u.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("update request failed: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read update response: %w", err)
	}
	if len(body) > int(limit) {
		return nil, fmt.Errorf("update response is too large")
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update server returned HTTP %d", response.StatusCode)
	}
	return body, nil
}

func (u updater) install(found release, stdout io.Writer) error {
	asset, err := updateAssetName(u.goos, u.goarch)
	if err != nil {
		return err
	}
	if !releaseHasAsset(found, asset) || !releaseHasAsset(found, "SHA256SUMS") {
		return fmt.Errorf("release %s is missing %s or SHA256SUMS", found.TagName, asset)
	}
	base := strings.TrimRight(u.downloadBase, "/") + "/" + url.PathEscape(found.TagName)
	checksums, err := u.download(base+"/SHA256SUMS", maxAPIResponse)
	if err != nil {
		return err
	}
	expected, err := checksumFor(checksums, asset)
	if err != nil {
		return err
	}
	binary, err := u.download(base+"/"+asset, maxUpdateBinary)
	if err != nil {
		return err
	}
	actual := fmt.Sprintf("%x", sha256.Sum256(binary))
	if !strings.EqualFold(expected, actual) {
		return fmt.Errorf("checksum verification failed for %s", asset)
	}

	target, info, err := inspectExecutable(u.executable)
	if err != nil {
		return err
	}
	if err := cleanupOldExecutable(target); err != nil {
		return err
	}
	pattern := ".pdo-update-*"
	if u.goos == "windows" {
		pattern += ".exe"
	}
	staged, err := os.CreateTemp(filepath.Dir(target), pattern)
	if err != nil {
		return fmt.Errorf("create staged executable: %w", err)
	}
	stagedPath := staged.Name()
	keepStaged := true
	defer func() {
		staged.Close()
		if keepStaged {
			os.Remove(stagedPath)
		}
	}()
	if err := staged.Chmod(info.Mode().Perm()); err != nil {
		return fmt.Errorf("set staged executable permissions: %w", err)
	}
	if _, err := staged.Write(binary); err != nil {
		return fmt.Errorf("write staged executable: %w", err)
	}
	if err := staged.Sync(); err != nil {
		return fmt.Errorf("sync staged executable: %w", err)
	}
	if err := staged.Close(); err != nil {
		return fmt.Errorf("close staged executable: %w", err)
	}
	if err := verifyExecutableVersion(stagedPath, found.TagName); err != nil {
		return err
	}

	tx, err := startTransaction(u.home)
	if err != nil {
		return err
	}
	output, commandErr := exec.Command(stagedPath, "__pdo-migrate", u.home, tx.dir).CombinedOutput()
	transactionDir := tx.dir
	tx, err = openTransaction(transactionDir)
	if err != nil {
		return fmt.Errorf("read migration transaction %s: %w; transaction retained", transactionDir, err)
	}
	if commandErr != nil {
		return finishFailedTransaction(tx, fmt.Errorf("candidate migration failed: %v: %s", commandErr, strings.TrimSpace(string(output))))
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return finishFailedTransaction(tx, fmt.Errorf("candidate returned invalid migration result"))
	}
	backup, err := replaceExecutable(target, stagedPath, info.Mode().Perm(), u.goos)
	if err != nil {
		return finishFailedTransaction(tx, err)
	}
	keepStaged = false
	if err := tx.cleanup(); err != nil {
		return rollbackUpdate(tx, target, backup, u.goos, fmt.Errorf("clean transaction %s: %w", tx.dir, err))
	}
	if u.goos != "windows" {
		if err := os.Remove(backup); err != nil {
			return fmt.Errorf("updated, but could not remove old executable %s: %w", backup, err)
		}
	}
	fmt.Fprintf(stdout, "Updated pdo from %s to %s\n", u.version, found.TagName)
	if count != 0 {
		fmt.Fprintf(stdout, "Migrated %d pdo file(s)\n", count)
	}
	return nil
}

func updateAssetName(goos, goarch string) (string, error) {
	if (goos != "darwin" && goos != "linux" && goos != "windows") || (goarch != "amd64" && goarch != "arm64") {
		return "", fmt.Errorf("unsupported update platform %s/%s", goos, goarch)
	}
	name := "pdo_" + goos + "_" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name, nil
}

func releaseHasAsset(found release, name string) bool {
	for _, asset := range found.Assets {
		if asset.Name == name {
			return true
		}
	}
	return false
}

func checksumFor(data []byte, name string) (string, error) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			if len(fields[0]) != sha256.Size*2 {
				break
			}
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("checksum not found for %s", name)
}

func inspectExecutable(path string) (string, os.FileInfo, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, fmt.Errorf("resolve executable: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", nil, fmt.Errorf("inspect executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("executable is not a regular file: %s", resolved)
	}
	return resolved, info, nil
}

func verifyExecutableVersion(path, want string) error {
	output, err := exec.Command(path, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("verify candidate version: %v: %s", err, strings.TrimSpace(string(output)))
	}
	if string(output) != "pdo "+want+"\n" {
		return fmt.Errorf("candidate version mismatch: got %q, want %q", strings.TrimSpace(string(output)), "pdo "+want)
	}
	return nil
}

func cleanupOldExecutable(target string) error {
	backup := target + ".pdo-update-old"
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove previous executable backup %s: %w", backup, err)
	}
	return nil
}

func replaceExecutable(target, staged string, mode os.FileMode, goos string) (string, error) {
	backup := target + ".pdo-update-old"
	if err := cleanupOldExecutable(target); err != nil {
		return "", err
	}
	if goos == "windows" {
		if err := os.Rename(target, backup); err != nil {
			return "", fmt.Errorf("back up current executable: %w", err)
		}
		if err := os.Rename(staged, target); err != nil {
			if restoreErr := os.Rename(backup, target); restoreErr != nil {
				return backup, fmt.Errorf("install executable: %v; restore failed: %v; backup: %s", err, restoreErr, backup)
			}
			return "", fmt.Errorf("install executable: %w", err)
		}
		return backup, nil
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return "", fmt.Errorf("read current executable: %w", err)
	}
	if err := writeExclusive(backup, data, mode); err != nil {
		return "", fmt.Errorf("back up current executable: %w", err)
	}
	if err := os.Rename(staged, target); err != nil {
		os.Remove(backup)
		return "", fmt.Errorf("install executable: %w", err)
	}
	return backup, nil
}

func restoreExecutable(target, backup string, goos string) error {
	if goos == "windows" {
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.Rename(backup, target)
}

func rollbackUpdate(tx *migrationTransaction, target, backup, goos string, cause error) error {
	binaryErr := restoreExecutable(target, backup, goos)
	migrationErr := tx.rollback()
	if binaryErr != nil || migrationErr != nil {
		return fmt.Errorf("%v; rollback failed (binary: %v, files: %v); transaction retained at %s", cause, binaryErr, migrationErr, tx.dir)
	}
	if err := tx.cleanup(); err != nil {
		return fmt.Errorf("%v; rollback completed but transaction remains at %s: %w", cause, tx.dir, err)
	}
	return cause
}

func startTransaction(home string) (*migrationTransaction, error) {
	root := filepath.Join(home, ".config", "pdo")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create update directory: %w", err)
	}
	dir := filepath.Join(root, ".update-transaction")
	if err := os.Mkdir(dir, 0o700); err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("another update or retained transaction exists at %s", dir)
		}
		return nil, fmt.Errorf("create update transaction: %w", err)
	}
	tx := &migrationTransaction{dir: dir}
	if err := tx.save(); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return tx, nil
}

func openTransaction(dir string) (*migrationTransaction, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	tx := &migrationTransaction{dir: dir}
	if err := json.Unmarshal(data, &tx.manifest); err != nil {
		return nil, err
	}
	return tx, nil
}

func (tx *migrationTransaction) save() error {
	data, err := json.Marshal(tx.manifest)
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(tx.dir, "manifest.json"), append(data, '\n'), 0o600)
}

func (tx *migrationTransaction) backup(path string) error {
	target, info, err := inspectLocalFile(path)
	if err != nil {
		return err
	}
	entry := transactionFile{Path: target}
	if info != nil {
		data, err := os.ReadFile(target)
		if err != nil {
			return err
		}
		entry.Existed = true
		entry.Mode = uint32(info.Mode().Perm())
		entry.Backup = fmt.Sprintf("file-%d", len(tx.manifest.Files))
		if err := writeExclusive(filepath.Join(tx.dir, entry.Backup), data, 0o600); err != nil {
			return err
		}
	}
	tx.manifest.Files = append(tx.manifest.Files, entry)
	return tx.save()
}

func (tx *migrationTransaction) rollback() error {
	for index := len(tx.manifest.Files) - 1; index >= 0; index-- {
		entry := tx.manifest.Files[index]
		if !entry.Existed {
			if err := os.Remove(entry.Path); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		data, err := os.ReadFile(filepath.Join(tx.dir, entry.Backup))
		if err != nil {
			return err
		}
		if err := atomicWriteFile(entry.Path, data, os.FileMode(entry.Mode)); err != nil {
			return err
		}
	}
	return nil
}

func (tx *migrationTransaction) cleanup() error {
	return os.RemoveAll(tx.dir)
}

func finishFailedTransaction(tx *migrationTransaction, cause error) error {
	if rollbackErr := tx.rollback(); rollbackErr != nil {
		return fmt.Errorf("%v; rollback failed: %v; transaction retained at %s", cause, rollbackErr, tx.dir)
	}
	if cleanupErr := tx.cleanup(); cleanupErr != nil {
		return fmt.Errorf("%v; rollback completed but transaction remains at %s: %w", cause, tx.dir, cleanupErr)
	}
	return cause
}

func runInternalMigrations(home, transactionDir string) (int, error) {
	expected := filepath.Join(filepath.Clean(home), ".config", "pdo", ".update-transaction")
	if !filepath.IsAbs(home) || filepath.Clean(transactionDir) != expected {
		return 0, fmt.Errorf("invalid internal migration paths")
	}
	tx, err := openTransaction(transactionDir)
	if err != nil {
		return 0, err
	}
	count, err := runOwnedMigrations(home, tx)
	if err != nil {
		return 0, err
	}
	return count, nil
}

func runOwnedMigrations(home string, tx *migrationTransaction) (int, error) {
	changed, err := migrateJSONFile(filepath.Join(home, ".config", "pdo", "config.json"), supportedSchemaVersion, configMigrationSteps, tx)
	if err != nil {
		return 0, err
	}
	if changed {
		return 1, nil
	}
	return 0, nil
}

func migrateJSONFile(path string, targetVersion int, steps map[int]jsonMigration, tx *migrationTransaction) (bool, error) {
	resolved, info, err := inspectLocalFile(path)
	if err != nil {
		return false, err
	}
	if info == nil {
		return false, nil
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return false, err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return false, fmt.Errorf("parse migration file %s: %w", path, err)
	}
	var schemaVersion int
	if err := json.Unmarshal(document["schema_version"], &schemaVersion); err != nil {
		return false, fmt.Errorf("parse schema_version in %s: %w", path, err)
	}
	if schemaVersion > targetVersion {
		return false, fmt.Errorf("schema_version %d in %s is newer than supported %d", schemaVersion, path, targetVersion)
	}
	for current := schemaVersion; current < targetVersion; current++ {
		if steps[current] == nil {
			return false, fmt.Errorf("missing migration from schema_version %d", current)
		}
	}
	if schemaVersion == targetVersion {
		return false, nil
	}
	if err := tx.backup(path); err != nil {
		return false, fmt.Errorf("back up %s: %w", path, err)
	}
	for schemaVersion < targetVersion {
		if err := steps[schemaVersion](document); err != nil {
			return false, fmt.Errorf("migrate %s from schema_version %d: %w", path, schemaVersion, err)
		}
		schemaVersion++
		document["schema_version"] = json.RawMessage(strconv.Itoa(schemaVersion))
	}
	updated, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return false, err
	}
	if err := atomicWriteFile(resolved, append(updated, '\n'), info.Mode().Perm()); err != nil {
		return false, err
	}
	return true, nil
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".pdo-update-file-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return os.Rename(tempPath, path)
	}
	swap := path + ".pdo-update-swap"
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return os.Rename(tempPath, path)
	}
	if err := os.Rename(path, swap); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		if restoreErr := os.Rename(swap, path); restoreErr != nil {
			return fmt.Errorf("%v; restore failed: %v; backup: %s", err, restoreErr, swap)
		}
		return err
	}
	return os.Remove(swap)
}
