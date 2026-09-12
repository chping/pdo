package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

const (
	supportedSchemaVersion  = 1
	maxAPIResponse          = 4 << 20
	maxUpdateBinary         = 100 << 20
	maxClipboardPayload     = 64 << 20
	maxCloudControlResponse = 64 << 10
	maxCloudTextResponse    = 6*maxClipboardPayload + maxCloudControlResponse
	cloudUploadChunkSize    = 1 << 20
	maxSetupInputLength     = 8192
	automaticUpdateInterval = 24 * time.Hour
	automaticUpdateTimeout  = 3 * time.Second
	pdoEnvFileName          = ".env"
	legacyPDOEnvFileName    = "env.json"
	githubPATEnvName        = "PDO_GITHUB_PAT"
	clipboardPasswordEnv    = "PDO_CLOUD_CLIPBOARD_PASSWORD"
	defaultClipboardRoom    = "personal"
	defaultSSHConfigLocal   = "~/.ssh/config"
	usage                   = `Usage:
  pdo download dotfiles [--<name> ...]
  pdo upload dotfiles [--<name> ...]
  pdo setup
  pdo copy ["text"]
  pdo paste
  pdo copy-file <file_path>
  pdo paste-file [save_path]
  pdo copy-ssh-id --identity=<path> [--config=<path>] [--target-host=<host>]
  pdo update [--check]
  pdo version`
)

var (
	version              = "devel"
	githubAPIBase        = "https://api.github.com"
	releaseAPIBase       = "https://api.github.com/repos/chping/pdo"
	releaseDownloadBase  = "https://github.com/chping/pdo/releases/download"
	dotfileNameRegexp    = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	envNameRegexp        = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	versionRegexp        = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	uuidRegexp           = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	configMigrationSteps = map[int]jsonMigration{}
	copySSHIDGOOS        = runtime.GOOS
	findCommand          = exec.LookPath
	runCommand           = func(name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
		command := exec.Command(name, args...)
		command.Stdin = stdin
		command.Stdout = stdout
		command.Stderr = stderr
		return command.Run()
	}
	readClipboard     = systemClipboardRead
	writeClipboard    = systemClipboardWrite
	checkDependencies = ensureDependencies
	cloudHTTPClient   = func() *http.Client {
		return &http.Client{Timeout: 10 * time.Minute}
	}
	setupInput      = os.Stdin
	isSetupTerminal = func(input *os.File) bool {
		return term.IsTerminal(int(input.Fd()))
	}
	readSetupSecret = func(input *os.File) ([]byte, error) {
		return term.ReadPassword(int(input.Fd()))
	}
	isTerminal = func(writer io.Writer) bool {
		file, ok := writer.(*os.File)
		if !ok {
			return false
		}
		info, err := file.Stat()
		return err == nil && info.Mode()&os.ModeCharDevice != 0
	}
)

type config struct {
	SchemaVersion  int                      `json:"schema_version"`
	GitHub         githubConfig             `json:"github"`
	Dotfiles       map[string]dotfileConfig `json:"dotfiles"`
	CloudClipboard cloudClipboardConfig     `json:"cloud_clipboard"`
}

type cloudClipboardConfig struct {
	Host        string `json:"host"`
	Room        string `json:"room"`
	PasswordEnv string `json:"password_env"`
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

type cloudClipboardClient struct {
	http          *http.Client
	host          string
	password      string
	clipboardRoom string
	fileRoom      string
}

type cloudContent struct {
	Type    string  `json:"type"`
	Content *string `json:"content"`
	Name    string  `json:"name"`
	Size    *int64  `json:"size"`
	UUID    string  `json:"uuid"`
}

type transferProgress struct {
	output io.Writer
	total  int64
	done   int64
	start  time.Time
	last   time.Time
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
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
	config     string
	identity   string
	targetHost string
}

type dependencyCommand struct {
	name    string
	args    []string
	display string
}

type dependencyFeature uint8

const (
	dependencyAll dependencyFeature = iota
	dependencySSH
	dependencyClipboard
)

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
	if automaticUpdate(args, os.Stdin, isSetupTerminal(os.Stdin), os.Stdout, os.Stderr, updater.install) {
		os.Exit(0)
	}
	os.Exit(run(args, os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "__pdo-dependencies" {
		if err := checkDependencies(dependencyAll, os.Stdin, false, false, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "pdo: dependencies: %v\n", err)
			return 1
		}
		return 0
	}
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
	if len(args) > 0 && args[0] == "setup" {
		if len(args) != 1 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		if err := runSetup(stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "pdo: setup: %v\n", err)
			return 1
		}
		return 0
	}
	if len(args) > 0 && (args[0] == "copy" || args[0] == "paste" || args[0] == "copy-file" || args[0] == "paste-file") {
		valid := (args[0] == "copy" && len(args) <= 2) ||
			(args[0] == "paste" && len(args) == 1) ||
			(args[0] == "copy-file" && len(args) == 2) ||
			(args[0] == "paste-file" && len(args) <= 2)
		if !valid {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		if err := runCloudClipboard(args, os.Stdin, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "pdo: %s: %v\n", args[0], err)
			return 1
		}
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
		u, err := newUpdater(30 * time.Second)
		if err != nil {
			fmt.Fprintf(stderr, "pdo: %v\n", err)
			return 1
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
	if err := loadPDOEnv(home); err != nil {
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

func runCloudClipboard(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("find home directory: %w", err)
	}
	cfg, err := loadConfig(filepath.Join(home, ".config", "pdo", "config.json"))
	if err != nil {
		return err
	}
	if err := loadPDOEnv(home); err != nil {
		return err
	}
	client, err := cfg.prepareCloudClipboard()
	if err != nil {
		return err
	}

	switch args[0] {
	case "copy":
		kind := "text"
		var data []byte
		if len(args) == 2 {
			data = []byte(args[1])
		} else {
			readStandardInput := true
			if file, ok := stdin.(*os.File); ok {
				info, statErr := file.Stat()
				if statErr != nil {
					return fmt.Errorf("inspect standard input: %w", statErr)
				}
				readStandardInput = info.Mode()&os.ModeCharDevice == 0
			}
			if readStandardInput {
				data, err = io.ReadAll(io.LimitReader(stdin, maxClipboardPayload+1))
				if err != nil {
					return fmt.Errorf("read standard input: %w", err)
				}
			} else {
				input, _ := stdin.(*os.File)
				if err := checkDependencies(dependencyClipboard, stdin, input != nil && isSetupTerminal(input), true, stdout, stderr); err != nil {
					return err
				}
				kind, data, err = readClipboard()
				if err != nil {
					return err
				}
			}
		}
		if _, _, err := validateClipboardPayload(kind, data); err != nil {
			return err
		}
		if kind == "text" {
			return client.postText(data)
		}
		return client.uploadFile(client.clipboardRoom, "clipboard.png", bytes.NewReader(data), int64(len(data)), nil)

	case "paste":
		content, err := client.latest(client.clipboardRoom, maxCloudTextResponse)
		if err != nil {
			return err
		}
		var kind string
		var data []byte
		switch {
		case content.Content != nil && content.UUID == "":
			if content.Type != "text" {
				return fmt.Errorf("invalid cloud clipboard text response")
			}
			kind = "text"
			data = []byte(*content.Content)
		case content.Content == nil && content.UUID != "" && content.Size != nil:
			kind = "image-png"
			var buffer bytes.Buffer
			if *content.Size >= 0 && *content.Size <= maxClipboardPayload {
				buffer.Grow(int(*content.Size))
			}
			if _, err := client.downloadFile(client.clipboardRoom, content.UUID, *content.Size, &buffer); err != nil {
				return err
			}
			data = buffer.Bytes()
		default:
			return fmt.Errorf("invalid cloud clipboard response")
		}
		width, height, err := validateClipboardPayload(kind, data)
		if err != nil {
			return err
		}
		if err := writeClipboard(kind, data); err != nil {
			fmt.Fprintf(stderr, "pdo: paste: warning: system clipboard unavailable: %v; falling back to stdout\n", err)
		}
		if kind == "text" {
			_, err = stdout.Write(data)
			if err == nil && len(data) != 0 && data[len(data)-1] != '\n' && isTerminal(stdout) {
				_, err = fmt.Fprintln(stdout)
			}
		} else {
			_, err = fmt.Fprintf(stdout, "image/png %dx%d %d bytes\n", width, height, len(data))
		}
		return err

	case "copy-file":
		file, err := os.Open(args[1])
		if err != nil {
			return fmt.Errorf("open file: %w", err)
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return fmt.Errorf("inspect file: %w", err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("path is not a regular file: %s", args[1])
		}
		if info.Size() > maxClipboardPayload {
			return fmt.Errorf("file exceeds 64 MiB limit")
		}
		name := filepath.Base(args[1])
		if !utf8.ValidString(name) {
			return fmt.Errorf("file name is not valid UTF-8")
		}
		var progress *transferProgress
		if isTerminal(stderr) {
			progress = newTransferProgress(stderr, info.Size())
			defer progress.finish()
		}
		return client.uploadFile(client.fileRoom, name, file, info.Size(), progress)

	case "paste-file":
		directory := "."
		if len(args) == 2 {
			directory = args[1]
		}
		directory, err = filepath.Abs(directory)
		if err != nil {
			return fmt.Errorf("resolve save path: %w", err)
		}
		info, err := os.Stat(directory)
		if err != nil {
			return fmt.Errorf("inspect save path: %w", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("save path is not a directory: %s", directory)
		}
		content, err := client.latest(client.fileRoom, maxCloudControlResponse)
		if err != nil {
			return err
		}
		if content.Content != nil || content.Name == "" || content.UUID == "" || content.Size == nil {
			return fmt.Errorf("latest cloud file room entry is not a file")
		}
		if err := validateDownloadedFileName(content.Name); err != nil {
			return err
		}
		if *content.Size < 0 || *content.Size > maxClipboardPayload {
			return fmt.Errorf("cloud file exceeds 64 MiB limit")
		}
		file, savedPath, err := createDownloadFile(directory, content.Name)
		if err != nil {
			return err
		}
		ok := false
		defer func() {
			file.Close()
			if !ok {
				os.Remove(savedPath)
			}
		}()
		if runtime.GOOS != "windows" {
			if err := file.Chmod(0o600); err != nil {
				return fmt.Errorf("set file permissions: %w", err)
			}
		}
		var progress *transferProgress
		writer := io.Writer(file)
		if isTerminal(stderr) {
			progress = newTransferProgress(stderr, *content.Size)
			writer = io.MultiWriter(file, progress)
			defer progress.finish()
		}
		if _, err := client.downloadFile(client.fileRoom, content.UUID, *content.Size, writer); err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			return fmt.Errorf("sync downloaded file: %w", err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close downloaded file: %w", err)
		}
		ok = true
		_, err = fmt.Fprintln(stdout, savedPath)
		return err
	}
	return nil
}

func (cfg config) prepareCloudClipboard() (*cloudClipboardClient, error) {
	item := cfg.CloudClipboard
	room, err := validateCloudClipboardConfig(item)
	if err != nil {
		return nil, err
	}
	password := os.Getenv(item.PasswordEnv)
	if password == "" {
		return nil, fmt.Errorf("environment variable %s is empty", item.PasswordEnv)
	}
	httpClient := cloudHTTPClient()
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cloudClipboardClient{
		http:          httpClient,
		host:          strings.TrimRight(item.Host, "/"),
		password:      password,
		clipboardRoom: room + "-pdo-clipboard",
		fileRoom:      room + "-pdo-file",
	}, nil
}

func (client *cloudClipboardClient) endpoint(path string, query url.Values) string {
	endpoint := client.host + "/" + strings.TrimLeft(path, "/")
	if len(query) != 0 {
		endpoint += "?" + query.Encode()
	}
	return endpoint
}

func (client *cloudClipboardClient) do(method, path string, query url.Values, body io.Reader, size int64, contentType string) (*http.Response, error) {
	request, err := http.NewRequest(method, client.endpoint(path, query), body)
	if err != nil {
		return nil, fmt.Errorf("create cloud-clipboard-go request: %w", err)
	}
	if size >= 0 {
		request.ContentLength = size
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	request.Header.Set("User-Agent", "pdo")
	request.Header.Set("Authorization", "Bearer "+client.password)
	response, err := client.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("cloud-clipboard-go request failed: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 4097))
		message := strings.TrimSpace(string(responseBody))
		if len(responseBody) > 4096 {
			message = strings.TrimSpace(string(responseBody[:4096])) + "..."
		}
		if message == "" {
			return nil, fmt.Errorf("cloud-clipboard-go returned HTTP %d", response.StatusCode)
		}
		return nil, fmt.Errorf("cloud-clipboard-go returned HTTP %d: %s", response.StatusCode, message)
	}
	return response, nil
}

func (client *cloudClipboardClient) jsonRequest(method, path string, query url.Values, body io.Reader, size int64, contentType string, limit int64, target any) error {
	response, err := client.do(method, path, query, body, size, contentType)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.ContentLength > limit {
		return fmt.Errorf("cloud-clipboard-go response exceeds %d-byte limit", limit)
	}
	limited := &io.LimitedReader{R: response.Body, N: limit + 1}
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode cloud-clipboard-go response: %w", err)
	}
	var trailing json.RawMessage
	err = decoder.Decode(&trailing)
	if limited.N == 0 {
		return fmt.Errorf("cloud-clipboard-go response exceeds %d-byte limit", limit)
	}
	if err != io.EOF {
		if err == nil {
			return fmt.Errorf("invalid trailing data in cloud-clipboard-go response")
		}
		return fmt.Errorf("decode cloud-clipboard-go response: %w", err)
	}
	return nil
}

func (client *cloudClipboardClient) postText(data []byte) error {
	var result struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	err := client.jsonRequest(http.MethodPost, "text", url.Values{"room": {client.clipboardRoom}}, bytes.NewReader(data), int64(len(data)), "text/plain", maxCloudControlResponse, &result)
	if err != nil {
		return err
	}
	if result.ID == "" || result.Type != "text" {
		return fmt.Errorf("invalid cloud-clipboard-go text response")
	}
	return nil
}

func (client *cloudClipboardClient) latest(room string, limit int64) (cloudContent, error) {
	var content cloudContent
	err := client.jsonRequest(http.MethodGet, "content/latest", url.Values{"json": {"true"}, "room": {room}}, nil, -1, "", limit, &content)
	if err != nil {
		return cloudContent{}, err
	}
	return content, nil
}

func (client *cloudClipboardClient) uploadFile(room, name string, reader io.Reader, size int64, progress *transferProgress) error {
	if size < 0 || size > maxClipboardPayload {
		return fmt.Errorf("file exceeds 64 MiB limit")
	}
	var created struct {
		Result struct {
			UUID string `json:"uuid"`
		} `json:"result"`
	}
	if err := client.jsonRequest(http.MethodPost, "upload/chunk", url.Values{"room": {room}}, strings.NewReader(name), int64(len(name)), "text/plain", maxCloudControlResponse, &created); err != nil {
		return err
	}
	uuid := created.Result.UUID
	if !uuidRegexp.MatchString(uuid) {
		return fmt.Errorf("invalid cloud-clipboard-go upload UUID %q", uuid)
	}
	finishing := false
	published := false
	defer func() {
		if !finishing && !published {
			client.deleteFile(room, uuid)
		}
	}()

	buffer := make([]byte, cloudUploadChunkSize)
	remaining := size
	if remaining == 0 {
		if err := client.postChunk(uuid, nil); err != nil {
			return err
		}
	}
	for remaining > 0 {
		chunkSize := int64(len(buffer))
		if remaining < chunkSize {
			chunkSize = remaining
		}
		chunk := buffer[:int(chunkSize)]
		if _, err := io.ReadFull(reader, chunk); err != nil {
			return fmt.Errorf("read upload file: %w", err)
		}
		if err := client.postChunk(uuid, chunk); err != nil {
			return err
		}
		if progress != nil {
			progress.Write(chunk)
		}
		remaining -= chunkSize
	}
	var extra [1]byte
	if n, err := io.ReadFull(reader, extra[:]); n != 0 {
		return fmt.Errorf("file changed while uploading")
	} else if err != io.EOF {
		return fmt.Errorf("read upload file: %w", err)
	}

	finishing = true
	var result struct {
		ID string `json:"id"`
	}
	if err := client.jsonRequest(http.MethodPost, "upload/finish/"+uuid, url.Values{"room": {room}}, nil, 0, "", maxCloudControlResponse, &result); err != nil {
		return fmt.Errorf("finish upload %s: %w; upload state may already be published", uuid, err)
	}
	if result.ID == "" {
		return fmt.Errorf("finish upload %s: invalid cloud-clipboard-go response; upload state may already be published", uuid)
	}
	published = true
	return nil
}

func (client *cloudClipboardClient) postChunk(uuid string, data []byte) error {
	var result struct{}
	return client.jsonRequest(http.MethodPost, "upload/chunk/"+uuid, nil, bytes.NewReader(data), int64(len(data)), "application/octet-stream", maxCloudControlResponse, &result)
}

func (client *cloudClipboardClient) deleteFile(room, uuid string) {
	response, err := client.do(http.MethodDelete, "file/"+uuid, url.Values{"room": {room}}, nil, -1, "")
	if err == nil {
		io.Copy(io.Discard, io.LimitReader(response.Body, maxCloudControlResponse))
		response.Body.Close()
	}
}

func validateCloudClipboardConfig(item cloudClipboardConfig) (string, error) {
	room := strings.TrimSpace(item.Room)
	if item.Host == "" || room == "" || item.PasswordEnv == "" {
		return "", fmt.Errorf("cloud_clipboard.host, room, and password_env are required; Webdis prefix/username configuration is no longer supported")
	}
	parsed, err := url.Parse(item.Host)
	if err != nil {
		return "", fmt.Errorf("cloud_clipboard.host: invalid URL: %w", err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(item.Host, "#") {
		return "", fmt.Errorf("cloud_clipboard.host must be an HTTPS cloud-clipboard-go base URL without credentials, query, or fragment")
	}
	return room, nil
}

func (client *cloudClipboardClient) downloadFile(room, uuid string, size int64, writer io.Writer) (int64, error) {
	if !uuidRegexp.MatchString(uuid) {
		return 0, fmt.Errorf("invalid cloud file UUID %q", uuid)
	}
	if size < 0 || size > maxClipboardPayload {
		return 0, fmt.Errorf("cloud file exceeds 64 MiB limit")
	}
	response, err := client.do(http.MethodGet, "file/"+uuid, url.Values{"room": {room}}, nil, -1, "")
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.ContentLength > maxClipboardPayload {
		return 0, fmt.Errorf("cloud file exceeds 64 MiB limit")
	}
	if response.ContentLength >= 0 && response.ContentLength != size {
		return 0, fmt.Errorf("downloaded file length is %d, expected %d", response.ContentLength, size)
	}
	written, err := io.Copy(writer, io.LimitReader(response.Body, size+1))
	if err != nil {
		return written, fmt.Errorf("download cloud file: %w", err)
	}
	if written != size {
		return written, fmt.Errorf("downloaded file length is %d, expected %d", written, size)
	}
	return written, nil
}

func validateClipboardPayload(kind string, data []byte) (int, int, error) {
	if len(data) > maxClipboardPayload {
		return 0, 0, fmt.Errorf("clipboard payload exceeds 64 MiB limit")
	}
	if kind == "text" {
		if !utf8.Valid(data) {
			return 0, 0, fmt.Errorf("clipboard text is not valid UTF-8")
		}
		return 0, 0, nil
	}
	if kind != "image-png" {
		return 0, 0, fmt.Errorf("unsupported clipboard type %q", kind)
	}
	image, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid PNG image: %w", err)
	}
	bounds := image.Bounds()
	return bounds.Dx(), bounds.Dy(), nil
}

func validateDownloadedFileName(name string) error {
	if name == "" || name == "." || name == ".." || !utf8.ValidString(name) || strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("invalid remote file name %q", name)
	}
	if runtime.GOOS == "windows" {
		if strings.ContainsAny(name, "<>:\"|?*") || strings.HasSuffix(name, " ") || strings.HasSuffix(name, ".") {
			return fmt.Errorf("invalid remote file name %q", name)
		}
		for _, character := range name {
			if character < 32 {
				return fmt.Errorf("invalid remote file name %q", name)
			}
		}
		base := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" ||
			(len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9') {
			return fmt.Errorf("invalid remote file name %q", name)
		}
	}
	return nil
}

func createDownloadFile(directory, name string) (*os.File, string, error) {
	extension := filepath.Ext(name)
	if extension == name {
		extension = ""
	}
	stem := strings.TrimSuffix(name, extension)
	for index := 0; ; index++ {
		candidate := name
		if index > 0 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, index, extension)
		}
		path := filepath.Join(directory, candidate)
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return file, path, nil
		}
		if !os.IsExist(err) {
			return nil, "", fmt.Errorf("create destination file: %w", err)
		}
	}
}

func newTransferProgress(output io.Writer, total int64) *transferProgress {
	return &transferProgress{output: output, total: total, start: time.Now()}
}

func (progress *transferProgress) Write(data []byte) (int, error) {
	progress.done += int64(len(data))
	progress.render(false)
	return len(data), nil
}

func (progress *transferProgress) finish() {
	progress.render(true)
	fmt.Fprintln(progress.output)
}

func (progress *transferProgress) render(force bool) {
	now := time.Now()
	if !force && !progress.last.IsZero() && now.Sub(progress.last) < 100*time.Millisecond {
		return
	}
	percent := 100.0
	if progress.total > 0 {
		percent = float64(progress.done) * 100 / float64(progress.total)
	}
	elapsed := now.Sub(progress.start).Seconds()
	rate := float64(progress.done)
	if elapsed > 0 {
		rate /= elapsed
	}
	fmt.Fprintf(progress.output, "\r%6.2f%% %s/%s %s/s", percent, formatBytes(progress.done), formatBytes(progress.total), formatBytes(int64(rate)))
	progress.last = now
}

func formatBytes(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	if size < 1024*1024 {
		return fmt.Sprintf("%.1f KiB", float64(size)/1024)
	}
	return fmt.Sprintf("%.1f MiB", float64(size)/(1024*1024))
}

func systemClipboardRead() (string, []byte, error) {
	switch runtime.GOOS {
	case "darwin":
		return macOSClipboardRead()
	case "windows":
		return windowsClipboardRead()
	case "linux":
		return linuxClipboardRead()
	default:
		return "", nil, fmt.Errorf("system clipboard is not supported on %s", runtime.GOOS)
	}
}

func systemClipboardWrite(kind string, data []byte) error {
	switch runtime.GOOS {
	case "darwin":
		return macOSClipboardWrite(kind, data)
	case "windows":
		return windowsClipboardWrite(kind, data)
	case "linux":
		return linuxClipboardWrite(kind, data)
	default:
		return fmt.Errorf("system clipboard is not supported on %s", runtime.GOOS)
	}
}

func linuxClipboardRead() (string, []byte, error) {
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		command, err := findCommand("wl-paste")
		if err != nil {
			return "", nil, fmt.Errorf("wl-paste is required; install wl-clipboard")
		}
		types, err := clipboardCommandOutput(command, []string{"--list-types"}, nil)
		if err != nil {
			return "", nil, err
		}
		if hasClipboardType(types, "image/png") {
			data, err := clipboardCommandOutput(command, []string{"--no-newline", "--type", "image/png"}, nil)
			return "image-png", data, err
		}
		for _, mimeType := range []string{"text/plain;charset=utf-8", "text/plain"} {
			if hasClipboardType(types, mimeType) {
				data, err := clipboardCommandOutput(command, []string{"--no-newline", "--type", mimeType}, nil)
				return "text", data, err
			}
		}
		return "", nil, fmt.Errorf("system clipboard does not contain text or an image")
	}
	if os.Getenv("DISPLAY") == "" {
		return "", nil, fmt.Errorf("no graphical clipboard session (DISPLAY and WAYLAND_DISPLAY are unset); start a Wayland or X11 session")
	}

	command, err := findCommand("xclip")
	if err != nil {
		return "", nil, fmt.Errorf("xclip is required; install xclip")
	}
	types, err := clipboardCommandOutput(command, []string{"-selection", "clipboard", "-target", "TARGETS", "-out"}, nil)
	if err != nil {
		return "", nil, err
	}
	if hasClipboardType(types, "image/png") {
		data, err := clipboardCommandOutput(command, []string{"-selection", "clipboard", "-target", "image/png", "-out"}, nil)
		return "image-png", data, err
	}
	for _, mimeType := range []string{"text/plain;charset=utf-8", "UTF8_STRING", "text/plain", "STRING"} {
		if hasClipboardType(types, mimeType) {
			data, err := clipboardCommandOutput(command, []string{"-selection", "clipboard", "-target", mimeType, "-out"}, nil)
			return "text", data, err
		}
	}
	return "", nil, fmt.Errorf("system clipboard does not contain text or an image")
}

func linuxClipboardWrite(kind string, data []byte) error {
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		command, err := findCommand("wl-copy")
		if err != nil {
			return fmt.Errorf("wl-copy is required; install with: %s", linuxClipboardInstallCommand("wl-clipboard"))
		}
		mimeType := "text/plain;charset=utf-8"
		if kind == "image-png" {
			mimeType = "image/png"
		}
		_, err = clipboardCommandOutput(command, []string{"--type", mimeType}, bytes.NewReader(data))
		return err
	}
	if os.Getenv("DISPLAY") == "" {
		return fmt.Errorf("no graphical clipboard session (DISPLAY and WAYLAND_DISPLAY are unset); start a Wayland or X11 session and install clipboard support with: %s", linuxClipboardInstallCommand("wl-clipboard xclip"))
	}
	command, err := findCommand("xclip")
	if err != nil {
		return fmt.Errorf("xclip is required; install with: %s", linuxClipboardInstallCommand("xclip"))
	}
	mimeType := "text/plain;charset=utf-8"
	if kind == "image-png" {
		mimeType = "image/png"
	}
	_, err = clipboardCommandOutput(command, []string{"-selection", "clipboard", "-target", mimeType, "-in"}, bytes.NewReader(data))
	return err
}

func linuxClipboardInstallCommand(packageName string) string {
	if _, err := findCommand("apt"); err == nil {
		return "sudo apt install " + packageName
	}
	if _, err := findCommand("dnf"); err == nil {
		return "sudo dnf install " + packageName
	}
	if _, err := findCommand("pacman"); err == nil {
		return "sudo pacman -S " + packageName
	}
	return fmt.Sprintf("sudo apt install %s (or sudo dnf install %s, or sudo pacman -S %s)", packageName, packageName, packageName)
}

func macOSClipboardRead() (string, []byte, error) {
	command, err := findCommand("osascript")
	if err != nil {
		return "", nil, fmt.Errorf("osascript is required; repair macOS system components")
	}
	kindOutput, err := clipboardCommandOutput(command, []string{"-l", "JavaScript", "-e", macOSClipboardTypeScript}, nil)
	if err != nil {
		return "", nil, err
	}
	kind := strings.TrimSpace(string(kindOutput))
	var script string
	if kind == "image-png" {
		script = macOSClipboardReadImageScript
	} else if kind == "text" {
		script = macOSClipboardReadTextScript
	} else {
		return "", nil, fmt.Errorf("system clipboard returned unsupported type %q", kind)
	}
	data, err := clipboardCommandOutput(command, []string{"-l", "JavaScript", "-e", script}, nil)
	return kind, data, err
}

func macOSClipboardWrite(kind string, data []byte) error {
	command, err := findCommand("osascript")
	if err != nil {
		return fmt.Errorf("osascript is required; repair macOS system components")
	}
	script := macOSClipboardWriteTextScript
	if kind == "image-png" {
		script = macOSClipboardWriteImageScript
	}
	_, err = clipboardCommandOutput(command, []string{"-l", "JavaScript", "-e", script}, bytes.NewReader(data))
	return err
}

func windowsClipboardRead() (string, []byte, error) {
	command, err := findCommand("powershell.exe")
	if err != nil {
		return "", nil, fmt.Errorf("PowerShell is required; repair it in Windows optional features")
	}
	temporary, err := os.CreateTemp("", "pdo-clipboard-*")
	if err != nil {
		return "", nil, fmt.Errorf("create clipboard temporary file: %w", err)
	}
	path := temporary.Name()
	temporary.Close()
	defer os.Remove(path)
	kindOutput, err := clipboardCommandOutput(command, []string{"-NoProfile", "-NonInteractive", "-STA", "-Command", windowsClipboardReadScript, path}, nil)
	if err != nil {
		return "", nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", nil, fmt.Errorf("inspect clipboard temporary file: %w", err)
	}
	if info.Size() > maxClipboardPayload {
		return "", nil, fmt.Errorf("clipboard payload exceeds 64 MiB limit")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("read clipboard temporary file: %w", err)
	}
	return strings.TrimSpace(string(kindOutput)), data, nil
}

func windowsClipboardWrite(kind string, data []byte) error {
	command, err := findCommand("powershell.exe")
	if err != nil {
		return fmt.Errorf("PowerShell is required; repair it in Windows optional features")
	}
	temporary, err := os.CreateTemp("", "pdo-clipboard-*")
	if err != nil {
		return fmt.Errorf("create clipboard temporary file: %w", err)
	}
	path := temporary.Name()
	defer os.Remove(path)
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write clipboard temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close clipboard temporary file: %w", err)
	}
	_, err = clipboardCommandOutput(command, []string{"-NoProfile", "-NonInteractive", "-STA", "-Command", windowsClipboardWriteScript, path, kind}, nil)
	return err
}

func clipboardCommandOutput(name string, arguments []string, input io.Reader) ([]byte, error) {
	output := &limitedBuffer{limit: maxClipboardPayload + 1}
	var commandError bytes.Buffer
	err := runCommand(name, arguments, input, output, &commandError)
	if output.Len() > maxClipboardPayload {
		return nil, fmt.Errorf("clipboard payload exceeds 64 MiB limit")
	}
	if err != nil {
		message := strings.TrimSpace(commandError.String())
		if message != "" {
			return nil, fmt.Errorf("clipboard command failed: %s", message)
		}
		return nil, fmt.Errorf("clipboard command failed: %w", err)
	}
	return output.Bytes(), nil
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	remaining := buffer.limit - buffer.Len()
	if remaining <= 0 {
		return 0, fmt.Errorf("output limit exceeded")
	}
	if len(data) > remaining {
		written, _ := buffer.Buffer.Write(data[:remaining])
		return written, fmt.Errorf("output limit exceeded")
	}
	return buffer.Buffer.Write(data)
}

func hasClipboardType(output []byte, target string) bool {
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == target {
			return true
		}
	}
	return false
}

const macOSClipboardTypeScript = `ObjC.import('AppKit')
function run() {
    const pasteboard = $.NSPasteboard.generalPasteboard
    const imageType = ObjC.unwrap(pasteboard.availableTypeFromArray(['public.png', 'public.tiff']))
    if (imageType) return 'image-png'
    const textType = ObjC.unwrap(pasteboard.availableTypeFromArray(['public.utf8-plain-text', 'public.utf16-external-plain-text', 'public.plain-text']))
    if (textType) return 'text'
    throw new Error('clipboard does not contain text or an image')
}`

const macOSClipboardReadImageScript = `ObjC.import('AppKit')
ObjC.import('Foundation')
function run() {
    const pasteboard = $.NSPasteboard.generalPasteboard
    const pngType = ObjC.unwrap(pasteboard.availableTypeFromArray(['public.png']))
    let data
    if (pngType) {
        data = pasteboard.dataForType(pngType)
    } else {
        const image = $.NSImage.alloc.initWithPasteboard(pasteboard)
        const representation = $.NSBitmapImageRep.imageRepWithData(image.TIFFRepresentation)
        data = representation.representationUsingTypeProperties($.NSBitmapImageFileTypePNG, $.NSDictionary.dictionary)
    }
    $.NSFileHandle.fileHandleWithStandardOutput.writeData(data)
}`

const macOSClipboardReadTextScript = `ObjC.import('AppKit')
ObjC.import('Foundation')
function run() {
    const pasteboard = $.NSPasteboard.generalPasteboard
    const type = pasteboard.availableTypeFromArray(['public.utf8-plain-text', 'public.utf16-external-plain-text', 'public.plain-text'])
    const value = pasteboard.stringForType(type)
    if (!value) throw new Error('clipboard text cannot be read')
    $.NSFileHandle.fileHandleWithStandardOutput.writeData(value.dataUsingEncoding($.NSUTF8StringEncoding))
}`

const macOSClipboardWriteTextScript = `ObjC.import('AppKit')
ObjC.import('Foundation')
function run() {
    const data = $.NSFileHandle.fileHandleWithStandardInput.readDataToEndOfFile
    const value = $.NSString.alloc.initWithDataEncoding(data, $.NSUTF8StringEncoding)
    const pasteboard = $.NSPasteboard.generalPasteboard
    pasteboard.clearContents
    if (!pasteboard.setStringForType(value, 'public.utf8-plain-text')) throw new Error('clipboard text cannot be written')
}`

const macOSClipboardWriteImageScript = `ObjC.import('AppKit')
ObjC.import('Foundation')
function run() {
    const data = $.NSFileHandle.fileHandleWithStandardInput.readDataToEndOfFile
    const pasteboard = $.NSPasteboard.generalPasteboard
    pasteboard.clearContents
    if (!pasteboard.setDataForType(data, 'public.png')) throw new Error('clipboard image cannot be written')
}`

const windowsClipboardReadScript = `Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$path = $args[0]
if ([Windows.Forms.Clipboard]::ContainsImage()) {
    $image = [Windows.Forms.Clipboard]::GetImage()
    try { $image.Save($path, [Drawing.Imaging.ImageFormat]::Png) } finally { $image.Dispose() }
    [Console]::Out.Write('image-png')
} elseif ([Windows.Forms.Clipboard]::ContainsText()) {
    [IO.File]::WriteAllText($path, [Windows.Forms.Clipboard]::GetText(), (New-Object Text.UTF8Encoding($false)))
    [Console]::Out.Write('text')
} else {
    throw 'clipboard does not contain text or an image'
}`

const windowsClipboardWriteScript = `Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$path = $args[0]
if ($args[1] -eq 'image-png') {
    $image = [Drawing.Image]::FromFile($path)
    try { [Windows.Forms.Clipboard]::SetImage($image) } finally { $image.Dispose() }
} else {
    $text = [IO.File]::ReadAllText($path, (New-Object Text.UTF8Encoding($false, $true)))
    [Windows.Forms.Clipboard]::SetDataObject($text, $true)
}`

const directSSHCopyIDCommand = `umask 077; mkdir -p "$HOME/.ssh" && touch "$HOME/.ssh/authorized_keys" && chmod 700 "$HOME/.ssh" && chmod 600 "$HOME/.ssh/authorized_keys" && IFS= read -r key && key_data=$(printf '%s\n' "$key" | awk '{print $2}') && [ -n "$key_data" ] && { awk -v key="$key_data" '{ for (i = 1; i <= NF; i++) if ($i == key) found = 1 } END { exit !found }' "$HOME/.ssh/authorized_keys" || printf '%s\n' "$key" >> "$HOME/.ssh/authorized_keys"; }`

func parseCopySSHIDArgs(args []string) (copySSHIDOptions, error) {
	var options copySSHIDOptions
	seen := make(map[string]bool)
	for index := 0; index < len(args); index++ {
		arg := args[index]
		name, value, hasValue := strings.Cut(arg, "=")
		if name != "--config" && name != "--identity" && name != "--target-host" {
			return options, fmt.Errorf("invalid copy-ssh-id argument %q", arg)
		}
		if seen[name] {
			return options, fmt.Errorf("duplicate copy-ssh-id argument %s", name)
		}
		seen[name] = true
		required := "a path"
		if name == "--target-host" {
			required = "a Host alias"
		}
		if !hasValue {
			index++
			if index >= len(args) || strings.HasPrefix(args[index], "--") {
				return options, fmt.Errorf("%s requires %s", name, required)
			}
			value = args[index]
		}
		if value == "" {
			return options, fmt.Errorf("%s requires %s", name, required)
		}
		switch name {
		case "--config":
			options.config = value
		case "--identity":
			options.identity = value
		case "--target-host":
			options.targetHost = value
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
	if options.targetHost != "" {
		selected := ""
		for _, host := range hosts {
			if strings.EqualFold(host, options.targetHost) {
				selected = host
				break
			}
		}
		if selected == "" {
			return false, fmt.Errorf("target Host %q was not found in %s", options.targetHost, options.config)
		}
		hosts = []string{selected}
	}
	if err := checkDependencies(dependencySSH, os.Stdin, isSetupTerminal(os.Stdin), true, stdout, stderr); err != nil {
		return false, err
	}
	ssh, err := findCommand("ssh")
	if err != nil {
		return false, fmt.Errorf("ssh was not found in PATH")
	}
	sshCopyID := ""
	var publicKey []byte
	directCopy := copySSHIDGOOS == "windows"
	if !directCopy {
		sshCopyID, err = findCommand("ssh-copy-id")
		directCopy = err != nil
	}
	if directCopy {
		publicKey, err = os.ReadFile(options.identity + ".pub")
		if err != nil {
			return false, fmt.Errorf("read public identity key: %w", err)
		}
		publicKey = bytes.TrimSpace(publicKey)
		if len(publicKey) == 0 {
			return false, fmt.Errorf("public identity key is empty")
		}
		publicKey = append(publicKey, '\n')
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
		command := sshCopyID
		arguments := []string{"-i", options.identity, "-F", options.config, host}
		var stdin io.Reader = os.Stdin
		label := "ssh-copy-id"
		if directCopy {
			command = ssh
			arguments = []string{"-F", options.config, host, directSSHCopyIDCommand}
			stdin = bytes.NewReader(publicKey)
			label = "ssh"
		}
		fmt.Fprintf(stdout, "pdo: %s: running %s\n", host, label)
		if err := runCommand(command, arguments, stdin, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "pdo: %s: %s failed: %v\n", host, label, err)
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

func runSetup(stdout, stderr io.Writer) error {
	if !isSetupTerminal(setupInput) {
		return fmt.Errorf("an interactive terminal is required")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("find home directory: %w", err)
	}
	configPath := filepath.Join(home, ".config", "pdo", "config.json")
	document, configTarget, configMode, err := readSetupDocument(configPath, "config")
	if err != nil {
		return err
	}
	if raw, ok := document["schema_version"]; ok {
		var schemaVersion int
		if err := json.Unmarshal(raw, &schemaVersion); err != nil || schemaVersion != supportedSchemaVersion {
			return fmt.Errorf("unsupported schema_version in %s (expected %d)", configPath, supportedSchemaVersion)
		}
	} else {
		document["schema_version"] = json.RawMessage(strconv.Itoa(supportedSchemaVersion))
	}

	environment, envTarget, err := readPDOEnv(home)
	if err != nil {
		return err
	}
	github, err := setupJSONObject(document, "github")
	if err != nil {
		return err
	}
	dotfiles, err := setupJSONObject(document, "dotfiles")
	if err != nil {
		return err
	}
	sshConfig, err := setupJSONObject(dotfiles, "ssh-config")
	if err != nil {
		return err
	}
	existingRemote, err := setupJSONString(sshConfig, "remote")
	if err != nil {
		return err
	}
	local, err := setupJSONString(sshConfig, "local")
	if err != nil {
		return err
	}
	if local == "" {
		local = defaultSSHConfigLocal
	}
	if _, err := expandLocalPath(home, local); err != nil {
		return fmt.Errorf("dotfiles.ssh-config.local: %w", err)
	}

	pat, err := readSetupSecretValue(setupInput, stderr, "GitHub personal access token (leave blank to keep existing): ")
	if err != nil {
		return err
	}
	if pat == "" {
		pat = environment[githubPATEnvName]
	}
	if pat == "" {
		return fmt.Errorf("a GitHub personal access token is required")
	}
	if err := validateSetupSecret(pat); err != nil {
		return fmt.Errorf("GitHub personal access token: %w", err)
	}

	remote, err := readSetupLine(setupInput, stderr, "SSH config GitHub file URL", existingRemote)
	if err != nil {
		return err
	}
	if remote == "" {
		remote = existingRemote
	}
	if _, _, _, err := parseRemote(remote); err != nil {
		return fmt.Errorf("SSH config GitHub file URL: %w", err)
	}

	cloud, err := setupJSONObject(document, "cloud_clipboard")
	if err != nil {
		return err
	}
	existingHost, err := setupJSONString(cloud, "host")
	if err != nil {
		return err
	}
	host, err := readSetupLine(setupInput, stderr, "Cloud clipboard service URL (leave blank to keep existing or skip)", existingHost)
	if err != nil {
		return err
	}
	if host == "" {
		host = existingHost
	}
	if host != "" {
		existingRoom, err := setupJSONString(cloud, "room")
		if err != nil {
			return err
		}
		if existingRoom == "" {
			existingRoom = defaultClipboardRoom
		}
		room, err := readSetupLine(setupInput, stderr, "Cloud clipboard room", existingRoom)
		if err != nil {
			return err
		}
		if room == "" {
			room = existingRoom
		}
		password, err := readSetupSecretValue(setupInput, stderr, "Cloud clipboard password (leave blank to keep existing): ")
		if err != nil {
			return err
		}
		if password == "" {
			password = environment[clipboardPasswordEnv]
		}
		if password == "" {
			return fmt.Errorf("a cloud clipboard password is required")
		}
		if err := validateSetupSecret(password); err != nil {
			return fmt.Errorf("cloud clipboard password: %w", err)
		}
		normalizedHost := strings.TrimRight(host, "/")
		if _, err := validateCloudClipboardConfig(cloudClipboardConfig{Host: normalizedHost, Room: room, PasswordEnv: clipboardPasswordEnv}); err != nil {
			return err
		}
		if err := setSetupJSON(cloud, "host", normalizedHost); err != nil {
			return err
		}
		if err := setSetupJSON(cloud, "room", room); err != nil {
			return err
		}
		if err := setSetupJSON(cloud, "password_env", clipboardPasswordEnv); err != nil {
			return err
		}
		if err := setSetupJSON(document, "cloud_clipboard", cloud); err != nil {
			return err
		}
		environment[clipboardPasswordEnv] = password
	}

	if err := setSetupJSON(github, "pat_env", githubPATEnvName); err != nil {
		return err
	}
	if err := setSetupJSON(document, "github", github); err != nil {
		return err
	}
	if err := setSetupJSON(sshConfig, "remote", remote); err != nil {
		return err
	}
	if err := setSetupJSON(sshConfig, "local", local); err != nil {
		return err
	}
	if err := setSetupJSON(dotfiles, "ssh-config", sshConfig); err != nil {
		return err
	}
	if err := setSetupJSON(document, "dotfiles", dotfiles); err != nil {
		return err
	}
	environment[githubPATEnvName] = pat

	configData, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	envData, err := formatPDOEnv(environment)
	if err != nil {
		return fmt.Errorf("encode pdo environment: %w", err)
	}
	if err := atomicWriteFile(envTarget, envData, 0o600); err != nil {
		return fmt.Errorf("write pdo environment: %w", err)
	}
	if err := atomicWriteFile(configTarget, append(configData, '\n'), configMode); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	fmt.Fprintf(stdout, "Configured pdo in %s.\nCredentials saved to %s.\n", configPath, filepath.Join(filepath.Dir(configPath), pdoEnvFileName))
	return nil
}

func readSetupDocument(path, label string) (map[string]json.RawMessage, string, os.FileMode, error) {
	target, info, err := inspectLocalFile(path)
	if err != nil {
		return nil, "", 0, fmt.Errorf("inspect %s: %w", label, err)
	}
	if info == nil {
		return make(map[string]json.RawMessage), target, 0o600, nil
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return nil, "", 0, fmt.Errorf("read %s: %w", label, err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil || document == nil {
		if err == nil {
			err = fmt.Errorf("must be a JSON object")
		}
		return nil, "", 0, fmt.Errorf("parse %s: %w", label, err)
	}
	return document, target, info.Mode().Perm(), nil
}

func readPDOEnv(home string) (map[string]string, string, error) {
	path := filepath.Join(home, ".config", "pdo", pdoEnvFileName)
	target, info, err := inspectLocalFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("inspect pdo environment: %w", err)
	}
	if info == nil {
		environment, err := readLegacyPDOEnv(home)
		return environment, target, err
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return nil, "", fmt.Errorf("read pdo environment: %w", err)
	}
	environment, err := parsePDOEnv(data)
	if err != nil {
		return nil, "", err
	}
	return environment, target, nil
}

func readLegacyPDOEnv(home string) (map[string]string, error) {
	document, _, _, err := readSetupDocument(filepath.Join(home, ".config", "pdo", legacyPDOEnvFileName), "legacy pdo environment")
	if err != nil {
		return nil, err
	}
	environment := make(map[string]string, len(document))
	for name, raw := range document {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("parse legacy pdo environment %s: %w", name, err)
		}
		environment[name] = value
	}
	return environment, nil
}

func parsePDOEnv(data []byte) (map[string]string, error) {
	environment := make(map[string]string)
	for lineNumber, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimPrefix(line, "export ")
		}
		name, value, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !ok || !envNameRegexp.MatchString(name) || strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("parse pdo environment line %d", lineNumber+1)
		}
		environment[name] = value
	}
	return environment, nil
}

func formatPDOEnv(environment map[string]string) ([]byte, error) {
	names := make([]string, 0, len(environment))
	for name, value := range environment {
		if !envNameRegexp.MatchString(name) || strings.ContainsAny(value, "\x00\r\n") {
			return nil, fmt.Errorf("invalid environment variable %s", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	var data strings.Builder
	for _, name := range names {
		fmt.Fprintf(&data, "%s=%s\n", name, environment[name])
	}
	return []byte(data.String()), nil
}

func loadPDOEnv(home string) error {
	environment, _, err := readPDOEnv(home)
	if err != nil {
		return err
	}
	for _, name := range []string{githubPATEnvName, clipboardPasswordEnv} {
		if value, ok := environment[name]; ok {
			if _, exists := os.LookupEnv(name); !exists {
				if err := os.Setenv(name, value); err != nil {
					return fmt.Errorf("set %s: %w", name, err)
				}
			}
		}
	}
	return nil
}

func setupJSONObject(document map[string]json.RawMessage, name string) (map[string]json.RawMessage, error) {
	raw, ok := document[name]
	if !ok {
		return make(map[string]json.RawMessage), nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		if err == nil {
			err = fmt.Errorf("must be a JSON object")
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return object, nil
}

func setupJSONString(document map[string]json.RawMessage, name string) (string, error) {
	raw, ok := document[name]
	if !ok {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return value, nil
}

func setSetupJSON(document map[string]json.RawMessage, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	document[name] = data
	return nil
}

func readSetupLine(input *os.File, output io.Writer, prompt, defaultValue string) (string, error) {
	if defaultValue == "" {
		fmt.Fprintf(output, "%s: ", prompt)
	} else {
		fmt.Fprintf(output, "%s [%s]: ", prompt, defaultValue)
	}
	line := make([]byte, 0, 64)
	var single [1]byte
	for {
		n, err := input.Read(single[:])
		if n != 0 {
			switch single[0] {
			case '\n':
				return strings.TrimSpace(string(line)), nil
			case '\r':
			default:
				line = append(line, single[0])
				if len(line) > maxSetupInputLength {
					return "", fmt.Errorf("input is too long")
				}
			}
		}
		if err != nil {
			if err == io.EOF && len(line) != 0 {
				return strings.TrimSpace(string(line)), nil
			}
			return "", fmt.Errorf("read input: %w", err)
		}
	}
}

func readSetupSecretValue(input *os.File, output io.Writer, prompt string) (string, error) {
	fmt.Fprint(output, prompt)
	value, err := readSetupSecret(input)
	fmt.Fprintln(output)
	if err != nil {
		return "", fmt.Errorf("read secret: %w", err)
	}
	return string(value), nil
}

func validateSetupSecret(value string) error {
	if strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("must not contain NUL or newlines")
	}
	return nil
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

const windowsOpenSSHInstallScript = `$process = Start-Process powershell.exe -Verb RunAs -Wait -PassThru -ArgumentList '-NoProfile -NonInteractive -Command "Add-WindowsCapability -Online -Name OpenSSH.Client~~~~0.0.1.0 | Out-Null"'; exit $process.ExitCode`

func commandAvailable(name string) bool {
	_, err := findCommand(name)
	return err == nil
}

func openSSHAvailable() bool {
	ssh, err := findCommand("ssh")
	if err != nil {
		return false
	}
	var output bytes.Buffer
	return runCommand(ssh, []string{"-V"}, nil, &output, &output) == nil && strings.Contains(output.String(), "OpenSSH")
}

func openWrtHost() bool {
	if _, err := os.Stat("/etc/openwrt_release"); err == nil {
		return true
	}
	data, err := os.ReadFile("/etc/os-release")
	return err == nil && regexp.MustCompile(`(?m)^ID=["']?openwrt["']?$`).Match(data)
}

func rootUser() bool {
	current, err := user.Current()
	return err == nil && current.Uid == "0"
}

func linuxPackageManager(openWrt bool) string {
	candidates := []string{"apt-get", "dnf", "yum", "pacman", "apk", "zypper"}
	if openWrt {
		candidates = []string{"opkg", "apk"}
	}
	for _, candidate := range candidates {
		if commandAvailable(candidate) {
			return candidate
		}
	}
	return ""
}

func linuxDependencyPackages(manager string, openWrt, needSSH, needWayland, needX11 bool) []string {
	packages := make([]string, 0, 3)
	if needSSH {
		switch {
		case openWrt:
			packages = append(packages, "openssh-client")
		case manager == "apt-get":
			packages = append(packages, "openssh-client")
		case manager == "dnf" || manager == "yum" || manager == "zypper":
			packages = append(packages, "openssh-clients")
		case manager == "pacman":
			packages = append(packages, "openssh")
		case manager == "apk":
			packages = append(packages, "openssh-client-default")
		}
	}
	if needWayland {
		packages = append(packages, "wl-clipboard")
	} else if needX11 {
		packages = append(packages, "xclip")
	}
	return packages
}

func linuxDependencyCommands(manager string, packages []string, root bool) ([]dependencyCommand, error) {
	if len(packages) == 0 {
		return nil, nil
	}
	commands := []dependencyCommand{}
	add := func(args ...string) {
		commands = append(commands, dependencyCommand{name: manager, args: args, display: strings.Join(append([]string{manager}, args...), " ")})
	}
	switch manager {
	case "apt-get":
		add("update")
		add(append([]string{"install", "-y"}, packages...)...)
	case "dnf", "yum":
		add(append([]string{"install", "-y"}, packages...)...)
	case "pacman":
		add(append([]string{"-Sy", "--needed", "--noconfirm"}, packages...)...)
	case "apk":
		add(append([]string{"add"}, packages...)...)
	case "zypper":
		add(append([]string{"--non-interactive", "install"}, packages...)...)
	case "opkg":
		add("update")
		add(append([]string{"install"}, packages...)...)
	default:
		return nil, fmt.Errorf("no supported package manager was found")
	}
	if root {
		return commands, nil
	}
	if !commandAvailable("sudo") {
		return commands, fmt.Errorf("sudo was not found; run the commands above as root")
	}
	for index := range commands {
		commands[index].args = append([]string{commands[index].name}, commands[index].args...)
		commands[index].name = "sudo"
		commands[index].display = "sudo " + commands[index].display
	}
	return commands, nil
}

func dependencyPlan(goos string, openWrt, root bool, feature dependencyFeature) ([]string, []dependencyCommand, error) {
	missing := []string{}
	checkSSH := feature == dependencyAll || feature == dependencySSH
	checkClipboard := feature == dependencyAll || feature == dependencyClipboard
	needSSH := checkSSH && !openSSHAvailable()
	if needSSH {
		missing = append(missing, "OpenSSH client (copy-ssh-id)")
	}
	switch goos {
	case "darwin":
		if checkClipboard && !commandAvailable("osascript") {
			missing = append(missing, "osascript (system clipboard)")
		}
		if len(missing) != 0 {
			return missing, nil, fmt.Errorf("required macOS system components are missing; repair macOS system components")
		}
	case "windows":
		powershellAvailable := commandAvailable("powershell.exe")
		if checkClipboard && !powershellAvailable {
			missing = append(missing, "Windows PowerShell (system clipboard)")
			return missing, nil, fmt.Errorf("Windows PowerShell is missing; repair it in Windows optional features")
		}
		if needSSH {
			if !powershellAvailable {
				return missing, nil, fmt.Errorf("Windows PowerShell is required to install OpenSSH Client; repair it in Windows optional features")
			}
			return missing, []dependencyCommand{{
				name:    "powershell.exe",
				args:    []string{"-NoProfile", "-NonInteractive", "-Command", windowsOpenSSHInstallScript},
				display: "Add-WindowsCapability -Online -Name OpenSSH.Client~~~~0.0.1.0 (Administrator)",
			}}, nil
		}
	case "linux":
		wayland := os.Getenv("WAYLAND_DISPLAY") != ""
		x11 := os.Getenv("DISPLAY") != ""
		needWayland := checkClipboard && !openWrt && wayland && (!commandAvailable("wl-copy") || !commandAvailable("wl-paste"))
		needX11 := checkClipboard && !openWrt && !wayland && x11 && !commandAvailable("xclip")
		if needWayland {
			missing = append(missing, "wl-clipboard (system clipboard)")
		} else if needX11 {
			missing = append(missing, "xclip (system clipboard)")
		} else if feature == dependencyClipboard && (openWrt || (!wayland && !x11)) {
			missing = append(missing, "graphical clipboard session")
			return missing, nil, fmt.Errorf("no graphical clipboard session is available")
		}
		if len(missing) != 0 {
			manager := linuxPackageManager(openWrt)
			if manager == "" {
				return missing, nil, fmt.Errorf("no supported package manager was found")
			}
			commands, err := linuxDependencyCommands(manager, linuxDependencyPackages(manager, openWrt, needSSH, needWayland, needX11), root)
			return missing, commands, err
		}
	default:
		return nil, nil, fmt.Errorf("unsupported operating system: %s", goos)
	}
	return missing, nil, nil
}

func ensureDependencies(feature dependencyFeature, stdin io.Reader, interactive, install bool, stdout, stderr io.Writer) error {
	missing, commands, planErr := dependencyPlan(runtime.GOOS, openWrtHost(), rootUser(), feature)
	if len(missing) == 0 {
		return nil
	}
	fmt.Fprintf(stderr, "pdo: missing optional dependencies: %s\n", strings.Join(missing, ", "))
	if len(commands) != 0 {
		fmt.Fprintln(stderr, "pdo: install with:")
		for _, command := range commands {
			fmt.Fprintf(stderr, "  %s\n", command.display)
		}
	}
	if !install {
		if planErr != nil {
			fmt.Fprintf(stderr, "pdo: dependency note: %v\n", planErr)
		}
		return nil
	}
	if planErr != nil {
		return planErr
	}
	if !interactive {
		return fmt.Errorf("install the missing dependencies and retry")
	}
	fmt.Fprint(stderr, "Install missing optional dependencies now? [y/N] ")
	answer, _ := bufio.NewReader(stdin).ReadString('\n')
	answer = strings.TrimSpace(answer)
	if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
		return fmt.Errorf("dependency installation declined")
	}
	for _, command := range commands {
		if err := runCommand(command.name, command.args, nil, stdout, stderr); err != nil {
			return fmt.Errorf("run %s: %w", command.display, err)
		}
	}
	missing, _, err := dependencyPlan(runtime.GOOS, openWrtHost(), rootUser(), feature)
	if err != nil || len(missing) != 0 {
		return fmt.Errorf("dependencies are still missing after installation")
	}
	return nil
}

func newUpdater(timeout time.Duration) (updater, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return updater{}, fmt.Errorf("find home directory: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return updater{}, fmt.Errorf("find executable: %w", err)
	}
	return updater{
		http:         &http.Client{Timeout: timeout},
		apiBase:      releaseAPIBase,
		downloadBase: releaseDownloadBase,
		version:      version,
		goos:         runtime.GOOS,
		goarch:       runtime.GOARCH,
		executable:   executable,
		home:         home,
	}, nil
}

func automaticUpdate(args []string, stdin io.Reader, interactive bool, stdout, stderr io.Writer, install func(updater, release, io.Writer) error) bool {
	if version == "devel" || (len(args) > 0 && (args[0] == "update" || strings.HasPrefix(args[0], "__pdo-"))) {
		return false
	}
	u, err := newUpdater(automaticUpdateTimeout)
	if err != nil {
		return false
	}
	marker := filepath.Join(u.home, ".config", "pdo", ".update-check")
	if info, err := os.Stat(marker); err == nil {
		if time.Since(info.ModTime()) < automaticUpdateInterval {
			return false
		}
	} else if !os.IsNotExist(err) {
		return false
	}

	found, latest, checkErr := u.latestRelease()
	_ = atomicWriteFile(marker, nil, 0o600)
	current, err := parseVersion(u.version)
	if checkErr != nil || err != nil || compareVersions(current, latest) >= 0 {
		return false
	}
	if !interactive {
		fmt.Fprintf(stderr, "pdo: update available: %s -> %s; run 'pdo update' to upgrade\n", u.version, found.TagName)
		return false
	}

	fmt.Fprintf(stderr, "pdo: update available: %s -> %s. Upgrade now? [y/N] ", u.version, found.TagName)
	answer, _ := bufio.NewReader(stdin).ReadString('\n')
	answer = strings.TrimSpace(answer)
	if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
		return false
	}
	if err := install(u, found, stdout); err != nil {
		fmt.Fprintf(stderr, "pdo: automatic update failed: %v\n", err)
		return false
	}
	fmt.Fprintln(stderr, "pdo: update installed; rerun the command")
	return true
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
	dependencyCheck := exec.Command(stagedPath, "__pdo-dependencies")
	dependencyCheck.Stdin = os.Stdin
	dependencyCheck.Stdout = stdout
	dependencyCheck.Stderr = os.Stderr
	if err := dependencyCheck.Run(); err != nil {
		return fmt.Errorf("candidate dependency check failed: %w", err)
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
