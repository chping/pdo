package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConfigValidation(t *testing.T) {
	home := t.TempDir()
	valid := config{
		SchemaVersion: 1,
		GitHub:        githubConfig{PATEnv: "PDO_GITHUB_PAT"},
		Dotfiles: map[string]dotfileConfig{
			"ssh-config": {
				Remote: "https://github.com/owner/repo/blob/main/ssh/config",
				Local:  "~/.ssh/config",
			},
		},
	}
	tests := []struct {
		name    string
		mutate  func(*config)
		wantErr string
	}{
		{name: "valid"},
		{name: "missing PAT env", mutate: func(cfg *config) { cfg.GitHub.PATEnv = "" }, wantErr: "github.pat_env"},
		{name: "empty dotfiles", mutate: func(cfg *config) { cfg.Dotfiles = nil }, wantErr: "at least one"},
		{name: "invalid key", mutate: func(cfg *config) { cfg.Dotfiles["ssh_config"] = cfg.Dotfiles["ssh-config"] }, wantErr: "kebab-case"},
		{name: "reserved key", mutate: func(cfg *config) { cfg.Dotfiles = map[string]dotfileConfig{"help": cfg.Dotfiles["ssh-config"]} }, wantErr: "must not be help"},
		{name: "invalid remote", mutate: func(cfg *config) {
			item := cfg.Dotfiles["ssh-config"]
			item.Remote = "https://api.github.com/repos/owner/repo/contents/config?ref=main"
			cfg.Dotfiles["ssh-config"] = item
		}, wantErr: "github.com file URL"},
		{name: "relative local", mutate: func(cfg *config) {
			item := cfg.Dotfiles["ssh-config"]
			item.Local = ".ssh/config"
			cfg.Dotfiles["ssh-config"] = item
		}, wantErr: "absolute path"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid
			cfg.Dotfiles = make(map[string]dotfileConfig, len(valid.Dotfiles))
			for key, value := range valid.Dotfiles {
				cfg.Dotfiles[key] = value
			}
			if test.mutate != nil {
				test.mutate(&cfg)
			}
			prepared, err := cfg.prepareDotfiles(home)
			if test.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if prepared["ssh-config"].local != filepath.Join(home, ".ssh", "config") || prepared["ssh-config"].branch != "main" {
					t.Fatalf("prepared = %+v", prepared["ssh-config"])
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadConfigSchemaAndUnknownSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"future":{"enabled":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schema_version":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "unsupported schema_version") {
		t.Fatalf("error = %v", err)
	}
}

func TestPDOEnvFormat(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".config", "pdo", pdoEnvFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# pdo credentials\nPDO_GITHUB_PAT=token=with#characters\nexport OTHER=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	environment, _, err := readPDOEnv(home)
	if err != nil || environment[githubPATEnvName] != "token=with#characters" || environment["OTHER"] != "value" {
		t.Fatalf("environment=%v error=%v", environment, err)
	}

	legacyHome := t.TempDir()
	legacyPath := filepath.Join(legacyHome, ".config", "pdo", legacyPDOEnvFileName)
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte(`{"PDO_GITHUB_PAT":"legacy-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	environment, target, err := readPDOEnv(legacyHome)
	if err != nil || environment[githubPATEnvName] != "legacy-token" || target != filepath.Join(legacyHome, ".config", "pdo", pdoEnvFileName) {
		t.Fatalf("legacy environment=%v target=%q error=%v", environment, target, err)
	}
}

func TestSetupWritesConfigAndPrivateEnvironment(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	remote := remoteURL("ssh/config")
	stdout, stderr, code := runSetupForTest(t, remote+"\nhttps://clipboard.example.com/api/\nteam\n", "github-secret-value", "clipboard-secret-value")
	if code != 0 || stderr.String() == "" || !strings.Contains(stdout.String(), "Configured pdo") {
		t.Fatalf("setup code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	configPath := filepath.Join(home, ".config", "pdo", "config.json")
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(configData), "github-secret-value") || strings.Contains(string(configData), "clipboard-secret-value") {
		t.Fatalf("config contains a secret: %s", configData)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(configData, &document); err != nil {
		t.Fatal(err)
	}
	var cfg config
	if err := json.Unmarshal(configData, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.GitHub.PATEnv != githubPATEnvName || cfg.Dotfiles["ssh-config"].Remote != remote || cfg.Dotfiles["ssh-config"].Local != defaultSSHConfigLocal || cfg.CloudClipboard.Host != "https://clipboard.example.com/api" || cfg.CloudClipboard.Room != "team" || cfg.CloudClipboard.PasswordEnv != clipboardPasswordEnv {
		t.Fatalf("config=%+v", cfg)
	}
	if string(document["schema_version"]) != "1" {
		t.Fatalf("schema_version=%s", document["schema_version"])
	}
	envPath := filepath.Join(home, ".config", "pdo", pdoEnvFileName)
	if !strings.Contains(stdout.String(), envPath) {
		t.Fatalf("setup did not report environment path: %q", stdout.String())
	}
	environment, _, err := readPDOEnv(home)
	if err != nil {
		t.Fatal(err)
	}
	if environment[githubPATEnvName] != "github-secret-value" || environment[clipboardPasswordEnv] != "clipboard-secret-value" {
		t.Fatalf("environment=%v", environment)
	}
	envData, err := os.ReadFile(envPath)
	if err != nil || !strings.Contains(string(envData), githubPATEnvName+"=github-secret-value\n") || strings.HasPrefix(strings.TrimSpace(string(envData)), "{") {
		t.Fatalf("environment file=%q error=%v", envData, err)
	}
	if runtime.GOOS != "windows" {
		assertMode(t, configPath, 0o600)
		assertMode(t, envPath, 0o600)
	}

	for _, name := range []string{githubPATEnvName, clipboardPasswordEnv} {
		previous, exists := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if exists {
				os.Setenv(name, previous)
			} else {
				os.Unsetenv(name)
			}
		})
	}
	if err := loadPDOEnv(home); err != nil || os.Getenv(githubPATEnvName) != "github-secret-value" || os.Getenv(clipboardPasswordEnv) != "clipboard-secret-value" {
		t.Fatalf("load pdo environment error=%v PAT=%q password=%q", err, os.Getenv(githubPATEnvName), os.Getenv(clipboardPasswordEnv))
	}
	if err := os.Setenv(githubPATEnvName, "external-value"); err != nil {
		t.Fatal(err)
	}
	if err := loadPDOEnv(home); err != nil || os.Getenv(githubPATEnvName) != "external-value" {
		t.Fatalf("external environment override error=%v PAT=%q", err, os.Getenv(githubPATEnvName))
	}
}

func TestSetupPreservesExistingConfigAndSecrets(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	configDir := filepath.Join(home, ".config", "pdo")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "actual-config.json")
	original := []byte(`{"schema_version":1,"future":{"keep":true},"github":{"pat_env":"OLD_PAT","custom":true},"dotfiles":{"git-config":{"remote":"https://github.com/owner/repo/blob/main/gitconfig","local":"~/.gitconfig"},"ssh-config":{"remote":"https://github.com/owner/repo/blob/main/ssh/config","local":"~/.ssh/custom","custom":true}},"cloud_clipboard":{"host":"https://clipboard.example.com/","room":"old-room","password_env":"OLD_PASSWORD","custom":true}}`)
	if err := os.WriteFile(target, original, 0o640); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "config.json")
	if err := os.Symlink(target, configPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	legacyEnvPath := filepath.Join(configDir, legacyPDOEnvFileName)
	if err := os.WriteFile(legacyEnvPath, []byte(`{"PDO_GITHUB_PAT":"old-pat","PDO_CLOUD_CLIPBOARD_PASSWORD":"old-password","OTHER":"keep"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runSetupForTest(t, "\n\n\n", "", "")
	if code != 0 || !strings.Contains(stderr.String(), "Cloud clipboard password") || !strings.Contains(stdout.String(), "Configured pdo") {
		t.Fatalf("setup code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	var future struct {
		Keep bool `json:"keep"`
	}
	if err := json.Unmarshal(document["future"], &future); err != nil || !future.Keep {
		t.Fatalf("future=%s error=%v", document["future"], err)
	}
	var github, dotfiles, sshConfig, cloud map[string]json.RawMessage
	if err := json.Unmarshal(document["github"], &github); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(document["dotfiles"], &dotfiles); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(dotfiles["ssh-config"], &sshConfig); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(document["cloud_clipboard"], &cloud); err != nil {
		t.Fatal(err)
	}
	if string(github["pat_env"]) != `"PDO_GITHUB_PAT"` || string(github["custom"]) != "true" || dotfiles["git-config"] == nil || string(sshConfig["local"]) != `"~/.ssh/custom"` || string(sshConfig["custom"]) != "true" || string(cloud["password_env"]) != `"PDO_CLOUD_CLIPBOARD_PASSWORD"` || string(cloud["custom"]) != "true" {
		t.Fatalf("document=%s", data)
	}
	envPath := filepath.Join(configDir, pdoEnvFileName)
	environment, _, err := readPDOEnv(home)
	if err != nil {
		t.Fatal(err)
	}
	if environment[githubPATEnvName] != "old-pat" || environment[clipboardPasswordEnv] != "old-password" || environment["OTHER"] != "keep" {
		t.Fatalf("environment=%v", environment)
	}
	if info, err := os.Lstat(configPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config link changed: info=%v error=%v", info, err)
	}
	if runtime.GOOS != "windows" {
		assertMode(t, target, 0o640)
		assertMode(t, envPath, 0o600)
	}
}

func TestSetupAllowsNoCloudClipboardAndRejectsInvalidInput(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	remote := remoteURL("ssh/config")
	stdout, stderr, code := runSetupForTest(t, remote+"\n\n", "secret")
	if code != 0 || stderr.String() == "" || !strings.Contains(stdout.String(), "Configured pdo") {
		t.Fatalf("setup code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "pdo", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if _, ok := document["cloud_clipboard"]; ok {
		t.Fatalf("unexpected cloud clipboard config: %s", data)
	}

	invalidHome := t.TempDir()
	setHome(t, invalidHome)
	stdout, stderr, code = runSetupForTest(t, "not-a-github-url\n", "secret")
	if code != 1 || !strings.Contains(stderr.String(), "GitHub file URL") {
		t.Fatalf("invalid setup code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(invalidHome, ".config", "pdo", "config.json")); !os.IsNotExist(err) {
		t.Fatalf("invalid setup wrote config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(invalidHome, ".config", "pdo", pdoEnvFileName)); !os.IsNotExist(err) {
		t.Fatalf("invalid setup wrote environment: %v", err)
	}

	previousInput, previousTerminal := setupInput, isSetupTerminal
	setupInput = os.Stdin
	isSetupTerminal = func(*os.File) bool { return false }
	t.Cleanup(func() { setupInput, isSetupTerminal = previousInput, previousTerminal })
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"setup"}, stdout, stderr); code != 1 || !strings.Contains(stderr.String(), "interactive terminal") {
		t.Fatalf("noninteractive setup code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if code := run([]string{"setup", "extra"}, stdout, stderr); code != 2 {
		t.Fatalf("setup args code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestCloudClipboardConfigValidation(t *testing.T) {
	t.Setenv("PDO_CLOUD_CLIPBOARD_PASSWORD", "secret")
	valid := config{CloudClipboard: cloudClipboardConfig{
		Host:        "https://clipboard.example.com/clipboard/",
		Room:        "personal",
		PasswordEnv: "PDO_CLOUD_CLIPBOARD_PASSWORD",
	}}
	if client, err := valid.prepareCloudClipboard(); err != nil || client.password != "secret" || client.host != "https://clipboard.example.com/clipboard" || client.clipboardRoom != "personal-pdo-clipboard" || client.fileRoom != "personal-pdo-file" || client.http.CheckRedirect == nil {
		t.Fatalf("client=%+v error=%v", client, err)
	}
	invalidHosts := []string{
		"http://clipboard.example.com/",
		"https://user@clipboard.example.com/",
		"https://clipboard.example.com/?query=1",
		"https://clipboard.example.com/#fragment",
	}
	for _, host := range invalidHosts {
		cfg := valid
		cfg.CloudClipboard.Host = host
		if _, err := cfg.prepareCloudClipboard(); err == nil {
			t.Errorf("host %q was accepted", host)
		}
	}
	for _, field := range []string{"host", "room", "password_env"} {
		cfg := valid
		switch field {
		case "host":
			cfg.CloudClipboard.Host = ""
		case "room":
			cfg.CloudClipboard.Room = ""
		case "password_env":
			cfg.CloudClipboard.PasswordEnv = ""
		}
		if _, err := cfg.prepareCloudClipboard(); err == nil {
			t.Errorf("missing %s was accepted", field)
		}
	}
	t.Setenv("PDO_CLOUD_CLIPBOARD_PASSWORD", "")
	if _, err := valid.prepareCloudClipboard(); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("empty password error=%v", err)
	}
	t.Setenv("PDO_CLOUD_CLIPBOARD_PASSWORD", "secret")
	old := valid
	old.CloudClipboard.Room = ""
	if _, err := old.prepareCloudClipboard(); err == nil || !strings.Contains(err.Error(), "Webdis") {
		t.Fatalf("old Webdis config error=%v", err)
	}
}

func TestCloudCommandsRejectMissingPasswordBeforeNetwork(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	writeCloudConfig(t, home, "https://clipboard.example.com")
	t.Setenv(clipboardPasswordEnv, "")
	previous := cloudHTTPClient
	cloudHTTPClient = func() *http.Client {
		t.Fatal("missing password reached the network client")
		return nil
	}
	t.Cleanup(func() { cloudHTTPClient = previous })

	for _, args := range [][]string{{"copy", "text"}, {"paste"}, {"copy-file", "missing"}, {"paste-file"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "run 'pdo setup'") {
			t.Fatalf("args=%v code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
}

func TestHTTPClientsLimitSilentWait(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer server.Close()
	defer close(release)
	client := newHTTPClient(time.Minute, 50*time.Millisecond)
	started := time.Now()
	if _, err := client.Get(server.URL); err == nil || time.Since(started) > time.Second {
		t.Fatalf("error=%v elapsed=%s", err, time.Since(started))
	}
}

func TestDotfileCommandsRejectMissingPATBeforeNetwork(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv(githubPATEnvName, "")
	writeConfig(t, home, map[string]dotfileConfig{
		"ssh-config": {Remote: remoteURL("ssh/config"), Local: "~/.ssh/config"},
	})
	previous := githubAPIBase
	githubAPIBase = "http://127.0.0.1:1"
	t.Cleanup(func() { githubAPIBase = previous })

	for _, command := range []string{"download", "upload"} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{command, "dotfiles"}, &stdout, &stderr); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "run 'pdo setup'") {
			t.Fatalf("command=%s code=%d stdout=%q stderr=%q", command, code, stdout.String(), stderr.String())
		}
	}
}

func TestCloudClipboardCommandArguments(t *testing.T) {
	invalid := [][]string{
		{"copy", "one", "two"},
		{"paste", "extra"},
		{"copy-file"},
		{"copy-file", "one", "two"},
		{"paste-file", "one", "two"},
	}
	for _, args := range invalid {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Errorf("args=%v code=%d stderr=%q", args, code, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "pdo copy-file") {
		t.Fatalf("help code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestCloudClipboardTextAndImageCommands(t *testing.T) {
	fixture := newCloudClipboardFixture(t)
	home := t.TempDir()
	setHome(t, home)
	writeCloudConfig(t, home, fixture.server.URL)
	setCloudServer(t, fixture.server)

	previousRead, previousWrite, previousDependencies := readClipboard, writeClipboard, checkDependencies
	t.Cleanup(func() {
		readClipboard, writeClipboard, checkDependencies = previousRead, previousWrite, previousDependencies
	})
	dependencyChecks := []dependencyFeature{}
	checkDependencies = func(feature dependencyFeature, _ io.Reader, _, _ bool, _, _ io.Writer) error {
		dependencyChecks = append(dependencyChecks, feature)
		return nil
	}
	var writtenKind string
	var writtenData []byte
	recordClipboardWrite := func(kind string, data []byte) error {
		writtenKind = kind
		writtenData = append([]byte(nil), data...)
		return nil
	}
	writeClipboard = recordClipboardWrite

	text := "中文\n\"quoted\"\x00"
	var stdout, stderr bytes.Buffer
	if code := run([]string{"copy", text}, &stdout, &stderr); code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("copy code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(dependencyChecks) != 0 {
		t.Fatalf("explicit copy checked dependencies: %v", dependencyChecks)
	}
	if code := run([]string{"paste"}, &stdout, &stderr); code != 0 || stdout.String() != text || writtenKind != "text" || string(writtenData) != text {
		t.Fatalf("paste code=%d stdout=%q stderr=%q kind=%q data=%q", code, stdout.String(), stderr.String(), writtenKind, writtenData)
	}
	pipeReader, pipeWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipeWriter.WriteString("piped text\n"); err != nil {
		t.Fatal(err)
	}
	if err := pipeWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runCloudClipboard([]string{"copy"}, pipeReader, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if err := pipeReader.Close(); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := run([]string{"paste"}, &stdout, &stderr); code != 0 || stdout.String() != "piped text\n" || writtenKind != "text" || string(writtenData) != "piped text\n" {
		t.Fatalf("piped paste code=%d stdout=%q stderr=%q kind=%q data=%q", code, stdout.String(), stderr.String(), writtenKind, writtenData)
	}
	if code := run([]string{"copy", text}, &stdout, &stderr); code != 0 {
		t.Fatalf("restore text copy code=%d stderr=%q", code, stderr.String())
	}
	previousTerminal := isTerminal
	isTerminal = func(io.Writer) bool { return true }
	t.Cleanup(func() { isTerminal = previousTerminal })
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"paste"}, &stdout, &stderr); code != 0 || stdout.String() != text+"\n" {
		t.Fatalf("terminal paste code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := run([]string{"copy", "already has a newline\n"}, &stdout, &stderr); code != 0 {
		t.Fatalf("newline copy code=%d stderr=%q", code, stderr.String())
	}
	if code := run([]string{"paste"}, &stdout, &stderr); code != 0 || stdout.String() != "already has a newline\n" {
		t.Fatalf("newline terminal paste code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	isTerminal = previousTerminal
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"copy", text}, &stdout, &stderr); code != 0 {
		t.Fatalf("restore text copy code=%d stderr=%q", code, stderr.String())
	}

	writeClipboard = func(string, []byte) error {
		return errors.New("clipboard command failed: Error: Can't open display: (null)")
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"paste"}, &stdout, &stderr); code != 0 || stdout.String() != text || !strings.Contains(stderr.String(), "warning: system clipboard unavailable") || !strings.Contains(stderr.String(), "falling back to stdout") {
		t.Fatalf("clipboard fallback code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	writeClipboard = recordClipboardWrite

	stdout.Reset()
	stderr.Reset()
	var imageData bytes.Buffer
	if err := png.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	readClipboard = func() (string, []byte, error) { return "image-png", imageData.Bytes(), nil }
	if code := run([]string{"copy"}, &stdout, &stderr); code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("image copy code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(dependencyChecks) != 1 || dependencyChecks[0] != dependencyClipboard {
		t.Fatalf("clipboard dependency checks=%v", dependencyChecks)
	}
	if code := run([]string{"paste"}, &stdout, &stderr); code != 0 || stdout.String() != fmt.Sprintf("image/png 2x3 %d bytes\n", imageData.Len()) || writtenKind != "image-png" || !bytes.Equal(writtenData, imageData.Bytes()) {
		t.Fatalf("image paste code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	writeClipboard = func(string, []byte) error { return errors.New("clipboard unavailable") }
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"paste"}, &stdout, &stderr); code != 0 || stdout.String() != fmt.Sprintf("image/png 2x3 %d bytes\n", imageData.Len()) || !strings.Contains(stderr.String(), "falling back to stdout") {
		t.Fatalf("image clipboard fallback code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	writeClipboard = recordClipboardWrite

	fixture.setFile("personal-pdo-clipboard", "clipboard.png", []byte("not a png"))
	writtenKind = "unchanged"
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"paste"}, &stdout, &stderr); code != 1 || writtenKind != "unchanged" || !strings.Contains(stderr.String(), "invalid PNG") {
		t.Fatalf("invalid image code=%d stdout=%q stderr=%q kind=%q", code, stdout.String(), stderr.String(), writtenKind)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"copy", ""}, &stdout, &stderr); code != 0 {
		t.Fatalf("empty copy code=%d stderr=%q", code, stderr.String())
	}
	writtenKind = ""
	if code := run([]string{"paste"}, &stdout, &stderr); code != 0 || stdout.Len() != 0 || writtenKind != "text" || len(writtenData) != 0 {
		t.Fatalf("empty paste code=%d stdout=%q stderr=%q kind=%q data=%q", code, stdout.String(), stderr.String(), writtenKind, writtenData)
	}
}

func TestCloudClipboardFileCommands(t *testing.T) {
	fixture := newCloudClipboardFixture(t)
	home := t.TempDir()
	setHome(t, home)
	writeCloudConfig(t, home, fixture.server.URL)
	setCloudServer(t, fixture.server)

	sourceDirectory := t.TempDir()
	target := filepath.Join(sourceDirectory, "target.bin")
	content := bytes.Repeat([]byte{0, 1, 2, 3, 255}, cloudUploadChunkSize/5+4)
	if err := os.WriteFile(target, content, 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(sourceDirectory, "資料 file.txt")
	if runtime.GOOS == "windows" {
		if err := os.Rename(target, source); err != nil {
			t.Fatal(err)
		}
	} else if err := os.Symlink(target, source); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"copy-file", source}, &stdout, &stderr); code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("copy-file code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if got := fixture.uploadChunkSizes(); len(got) != 2 || got[0] != cloudUploadChunkSize || got[1] != len(content)-cloudUploadChunkSize {
		t.Fatalf("upload chunks=%v", got)
	}
	destination := t.TempDir()
	existing := filepath.Join(destination, filepath.Base(source))
	if err := os.WriteFile(existing, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := run([]string{"paste-file", destination}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("paste-file code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	first := filepath.Join(destination, "資料 file (1).txt")
	if stdout.String() != first+"\n" {
		t.Fatalf("saved path=%q want=%q", stdout.String(), first+"\n")
	}
	assertFileContent(t, first, content)
	if runtime.GOOS != "windows" {
		assertMode(t, first, 0o600)
	}
	stdout.Reset()
	if code := run([]string{"paste-file", destination}, &stdout, &stderr); code != 0 {
		t.Fatalf("second paste-file code=%d stderr=%q", code, stderr.String())
	}
	second := filepath.Join(destination, "資料 file (2).txt")
	assertFileContent(t, second, content)

	empty := filepath.Join(sourceDirectory, "empty.bin")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"copy-file", empty}, &stdout, &stderr); code != 0 {
		t.Fatalf("empty copy-file code=%d stderr=%q", code, stderr.String())
	}
	chunks := fixture.uploadChunkSizes()
	if chunks[len(chunks)-1] != 0 {
		t.Fatalf("empty upload chunks=%v", chunks)
	}
	if code := run([]string{"paste-file", destination}, &stdout, &stderr); code != 0 {
		t.Fatalf("empty paste-file code=%d stderr=%q", code, stderr.String())
	}
	assertFileContent(t, filepath.Join(destination, "empty.bin"), nil)
	defaultDirectory := t.TempDir()
	t.Chdir(defaultDirectory)
	absoluteDefaultDirectory, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	defaultDirectory = absoluteDefaultDirectory
	stdout.Reset()
	if code := run([]string{"paste-file"}, &stdout, &stderr); code != 0 || stdout.String() != filepath.Join(defaultDirectory, "empty.bin")+"\n" {
		t.Fatalf("default paste-file code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	assertFileContent(t, filepath.Join(defaultDirectory, "empty.bin"), nil)

	fixture.setFile("personal-pdo-file", "../escape", []byte("bad"))
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"paste-file", destination}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "invalid remote file name") {
		t.Fatalf("invalid name code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	size := int64(3)
	fixture.mutex.Lock()
	fixture.rooms["personal-pdo-file"] = cloudContent{Type: "file", Name: "invalid-uuid.bin", Size: &size, UUID: "../escape"}
	fixture.mutex.Unlock()
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"paste-file", destination}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "invalid cloud file UUID") {
		t.Fatalf("invalid UUID code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(destination, "invalid-uuid.bin")); !os.IsNotExist(err) {
		t.Fatalf("invalid UUID left destination file: %v", err)
	}

	fixture.setFile("personal-pdo-file", "mismatch.bin", []byte("abc"))
	fixture.mutex.Lock()
	mismatch := fixture.rooms["personal-pdo-file"]
	wrongSize := int64(4)
	mismatch.Size = &wrongSize
	fixture.rooms["personal-pdo-file"] = mismatch
	fixture.mutex.Unlock()
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"paste-file", destination}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "length") {
		t.Fatalf("length mismatch code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(destination, "mismatch.bin")); !os.IsNotExist(err) {
		t.Fatalf("length mismatch left destination file: %v", err)
	}

	large := filepath.Join(sourceDirectory, "large.bin")
	largeFile, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := largeFile.Truncate(maxClipboardPayload + 1); err != nil {
		t.Fatal(err)
	}
	largeFile.Close()
	stderr.Reset()
	if code := run([]string{"copy-file", large}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "64 MiB") {
		t.Fatalf("large file code=%d stderr=%q", code, stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"copy-file", sourceDirectory}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "not a regular file") {
		t.Fatalf("directory code=%d stderr=%q", code, stderr.String())
	}
}

func TestCloudClipboardResponseValidationAndProgress(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, `{"type":"text","content":"too large"}`)
	}))
	defer server.Close()
	client := cloudClipboardClient{http: server.Client(), host: server.URL, password: "secret"}
	var content cloudContent
	err := client.jsonRequest(http.MethodGet, "content/latest", nil, nil, -1, "", 16, &content)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized response error=%v", err)
	}

	var output bytes.Buffer
	progress := newTransferProgress(&output, 4)
	if _, err := progress.Write([]byte("test")); err != nil {
		t.Fatal(err)
	}
	progress.finish()
	if !strings.Contains(output.String(), "100.00%") || !strings.Contains(output.String(), "4 B/4 B") || !strings.HasSuffix(output.String(), "\n") {
		t.Fatalf("progress=%q", output.String())
	}
}

func TestCloudClipboardPayloadLimitAndTrailingJSON(t *testing.T) {
	payload := make([]byte, maxClipboardPayload+1)
	if _, _, err := validateClipboardPayload("text", payload[:maxClipboardPayload]); err != nil {
		t.Fatalf("64 MiB payload rejected: %v", err)
	}
	if _, _, err := validateClipboardPayload("text", payload); err == nil || !strings.Contains(err.Error(), "64 MiB") {
		t.Fatalf("oversized payload error=%v", err)
	}

	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, `{} {}`)
	}))
	defer server.Close()
	client := cloudClipboardClient{http: server.Client(), host: server.URL, password: "secret"}
	var content cloudContent
	if err := client.jsonRequest(http.MethodGet, "content/latest", nil, nil, -1, "", maxCloudControlResponse, &content); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("trailing JSON error=%v", err)
	}
}

func TestCloudClipboardUploadFailureCleanup(t *testing.T) {
	fixture := newCloudClipboardFixture(t)
	client := cloudClipboardClient{http: fixture.server.Client(), host: fixture.server.URL + "/api", password: "secret"}

	fixture.mutex.Lock()
	fixture.failChunk = true
	fixture.mutex.Unlock()
	if err := client.uploadFile("room", "chunk.bin", strings.NewReader("data"), 4, nil); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("chunk failure error=%v", err)
	}
	fixture.mutex.Lock()
	deleted := fixture.deleted
	fixture.failChunk = false
	fixture.failFinish = true
	fixture.mutex.Unlock()
	if deleted != 1 {
		t.Fatalf("chunk failure cleanup count=%d", deleted)
	}
	if err := client.uploadFile("room", "finish.bin", strings.NewReader("data"), 4, nil); err == nil || !strings.Contains(err.Error(), "may already be published") {
		t.Fatalf("finish failure error=%v", err)
	}
	fixture.mutex.Lock()
	deleted, finishRequests := fixture.deleted, fixture.finishRequests
	fixture.mutex.Unlock()
	if deleted != 1 || finishRequests != 1 {
		t.Fatalf("finish failure deleted=%d requests=%d", deleted, finishRequests)
	}
}

func TestCloudClipboardDoesNotFollowRedirects(t *testing.T) {
	var mutex sync.Mutex
	hits := 0
	destination := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mutex.Lock()
		hits++
		mutex.Unlock()
	}))
	defer destination.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	previous := cloudHTTPClient
	cloudHTTPClient = func() *http.Client { return redirect.Client() }
	t.Cleanup(func() { cloudHTTPClient = previous })
	t.Setenv("PDO_CLOUD_CLIPBOARD_PASSWORD", "secret")
	client, err := (config{CloudClipboard: cloudClipboardConfig{Host: redirect.URL, Room: "room", PasswordEnv: "PDO_CLOUD_CLIPBOARD_PASSWORD"}}).prepareCloudClipboard()
	if err != nil {
		t.Fatal(err)
	}
	if err := client.postText([]byte("text")); err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("redirect error=%v", err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if hits != 0 {
		t.Fatalf("redirect destination hits=%d", hits)
	}
}

func TestClipboardPlatformCommands(t *testing.T) {
	previousFind, previousRun := findCommand, runCommand
	t.Cleanup(func() { findCommand, runCommand = previousFind, previousRun })
	findCommand = func(name string) (string, error) { return name, nil }

	t.Run("Wayland image priority and write MIME", func(t *testing.T) {
		t.Setenv("WAYLAND_DISPLAY", "wayland-0")
		var written []byte
		runCommand = func(name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
			if args[0] == "--list-types" {
				io.WriteString(stdout, "text/plain;charset=utf-8\nimage/png\n")
				return nil
			}
			if args[0] == "--no-newline" {
				stdout.Write([]byte("png"))
				return nil
			}
			written, _ = io.ReadAll(stdin)
			if strings.Join(args, " ") != "--type image/png" {
				t.Errorf("write args=%v", args)
			}
			return nil
		}
		kind, data, err := linuxClipboardRead()
		if err != nil || kind != "image-png" || string(data) != "png" {
			t.Fatalf("kind=%q data=%q error=%v", kind, data, err)
		}
		if err := linuxClipboardWrite("image-png", []byte("png")); err != nil || string(written) != "png" {
			t.Fatalf("written=%q error=%v", written, err)
		}
	})

	t.Run("Linux clipboard availability", func(t *testing.T) {
		findCommand = func(name string) (string, error) { return name, nil }
		t.Setenv("WAYLAND_DISPLAY", "")
		t.Setenv("DISPLAY", "")
		called := false
		runCommand = func(string, []string, io.Reader, io.Writer, io.Writer) error {
			called = true
			return nil
		}
		err := linuxClipboardWrite("text", []byte("text"))
		if err == nil || !strings.Contains(err.Error(), "no graphical clipboard session") || !strings.Contains(err.Error(), "sudo apt install wl-clipboard xclip") || called {
			t.Fatalf("error=%v command called=%t", err, called)
		}
	})

	t.Run("Linux missing clipboard tools show install commands", func(t *testing.T) {
		cases := []struct {
			name        string
			wayland     bool
			missing     string
			manager     string
			wantCommand string
		}{
			{"Wayland apt", true, "wl-copy", "apt", "sudo apt install wl-clipboard"},
			{"X11 dnf", false, "xclip", "dnf", "sudo dnf install xclip"},
			{"Wayland pacman", true, "wl-copy", "pacman", "sudo pacman -S wl-clipboard"},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				if test.wayland {
					t.Setenv("WAYLAND_DISPLAY", "wayland-0")
					t.Setenv("DISPLAY", "")
				} else {
					t.Setenv("WAYLAND_DISPLAY", "")
					t.Setenv("DISPLAY", ":0")
				}
				findCommand = func(name string) (string, error) {
					if name == test.manager {
						return name, nil
					}
					return "", errors.New("not found")
				}
				err := linuxClipboardWrite("text", []byte("text"))
				if err == nil || !strings.Contains(err.Error(), test.wantCommand) {
					t.Fatalf("error=%v", err)
				}
			})
		}

		findCommand = func(string) (string, error) { return "", errors.New("not found") }
		t.Setenv("WAYLAND_DISPLAY", "")
		t.Setenv("DISPLAY", ":0")
		err := linuxClipboardWrite("text", []byte("text"))
		if err == nil || !strings.Contains(err.Error(), "sudo apt install xclip") || !strings.Contains(err.Error(), "sudo dnf install xclip") || !strings.Contains(err.Error(), "sudo pacman -S xclip") {
			t.Fatalf("fallback install command error=%v", err)
		}
	})

	findCommand = func(name string) (string, error) { return name, nil }

	t.Run("macOS raw text", func(t *testing.T) {
		calls := 0
		runCommand = func(name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
			calls++
			if calls == 1 {
				io.WriteString(stdout, "text\n")
			} else {
				io.WriteString(stdout, "line one\nline two")
			}
			return nil
		}
		kind, data, err := macOSClipboardRead()
		if err != nil || kind != "text" || string(data) != "line one\nline two" {
			t.Fatalf("kind=%q data=%q error=%v", kind, data, err)
		}
	})

	t.Run("Windows temporary file", func(t *testing.T) {
		runCommand = func(name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
			path := args[len(args)-1]
			if err := os.WriteFile(path, []byte("windows text"), 0o600); err != nil {
				return err
			}
			io.WriteString(stdout, "text")
			return nil
		}
		kind, data, err := windowsClipboardRead()
		if err != nil || kind != "text" || string(data) != "windows text" {
			t.Fatalf("kind=%q data=%q error=%v", kind, data, err)
		}
	})
}

func TestParseRemoteRejectsInvalidURLs(t *testing.T) {
	invalid := []string{
		"http://github.com/o/r/blob/main/a",
		"https://token@github.com/o/r/blob/main/a",
		"https://github.com/o/r/main/a",
		"https://github.com/o/r/blob/main/a?plain=1",
		"https://github.com/o/r/blob/main/../a",
		"https://github.com/o/r/blob/main/",
		"https://github.com/o/r/blob/main/a#L1",
	}
	for _, value := range invalid {
		if _, _, _, err := parseRemote(value); err == nil {
			t.Errorf("parseRemote(%q) succeeded", value)
		}
	}
}

func TestRunUsageSelectorsAndExitCodes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "pdo download dotfiles") {
		t.Fatalf("help: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"download", "ssh-config"}, &stdout, &stderr); code != 2 {
		t.Fatalf("invalid command code = %d", code)
	}

	server := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeRemoteFile(t, w, []byte("remote\n"), "sha")
	})
	setGitHubServer(t, server)
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("PDO_GITHUB_PAT", "token")
	writeConfig(t, home, map[string]dotfileConfig{
		"ssh-config": {Remote: remoteURL("ssh/config"), Local: "~/.ssh/config"},
	})

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"download", "dotfiles", "--unknown"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "unknown dotfile selector") {
		t.Fatalf("unknown: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"download", "dotfiles", "--ssh-config", "--ssh-config"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "1 succeeded, 0 failed") {
		t.Fatalf("duplicate: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestCopySSHIDArgumentParsing(t *testing.T) {
	tests := []struct {
		args       []string
		config     string
		identity   string
		targetHost string
		wantErr    bool
	}{
		{args: []string{"--identity=key"}, identity: "key"},
		{args: []string{"--config", "config", "--identity", "key"}, config: "config", identity: "key"},
		{args: []string{"--identity", "key", "--config=config"}, config: "config", identity: "key"},
		{args: []string{"--identity=key", "--target-host=openwrt"}, identity: "key", targetHost: "openwrt"},
		{args: []string{"--target-host", "OpenWrt", "--identity", "key"}, identity: "key", targetHost: "OpenWrt"},
		{args: nil, wantErr: true},
		{args: []string{"--identity"}, wantErr: true},
		{args: []string{"--identity="}, wantErr: true},
		{args: []string{"--identity", "--config=config"}, wantErr: true},
		{args: []string{"--identity=key", "--target-host"}, wantErr: true},
		{args: []string{"--identity=key", "--target-host="}, wantErr: true},
		{args: []string{"--identity=key", "--target-host=one", "--target-host=two"}, wantErr: true},
		{args: []string{"--identity=one", "--identity", "two"}, wantErr: true},
		{args: []string{"--unknown=value"}, wantErr: true},
		{args: []string{"key"}, wantErr: true},
	}
	for _, test := range tests {
		options, err := parseCopySSHIDArgs(test.args)
		if test.wantErr {
			if err == nil {
				t.Errorf("args=%v succeeded", test.args)
			}
			continue
		}
		if err != nil || options.config != test.config || options.identity != test.identity || options.targetHost != test.targetHost {
			t.Errorf("args=%v options=%+v error=%v", test.args, options, err)
		}
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"copy-ssh-id", "--identity"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "requires a path") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestResolveCommandPath(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	working := filepath.Join(root, "work")
	absolute := filepath.Join(root, "absolute", "key")
	tests := []struct {
		value string
		want  string
	}{
		{value: "~/key", want: filepath.Join(home, "key")},
		{value: "relative/key", want: filepath.Join(working, "relative", "key")},
		{value: absolute, want: absolute},
	}
	for _, test := range tests {
		got, err := resolveCommandPath(home, working, test.value)
		if err != nil || got != test.want {
			t.Errorf("value=%q got=%q want=%q error=%v", test.value, got, test.want, err)
		}
	}
	if _, err := resolveCommandPath(home, working, "~other/key"); err == nil {
		t.Fatal("~user path accepted")
	}
}

func TestParseSSHHostsIncludesAndPatterns(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	includeDir := filepath.Join(sshDir, "config.d")
	if err := os.MkdirAll(includeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(sshDir, "nested.conf"), "Host Nested\n")
	writeTestFile(t, filepath.Join(includeDir, "a.conf"), "Include nested.conf\nHost Alpha Shared\n")
	writeTestFile(t, filepath.Join(includeDir, "b.conf"), "Host shared Bravo\n")
	config := filepath.Join(sshDir, "config")
	writeTestFile(t, config, `Include "config.d/*.conf"
Include missing-*.conf
Host Root root *.example !skip
Host = Beta
Match all
  Include ignored.conf
Host Last
`)
	hosts, err := parseSSHHosts(config, home)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Nested", "Alpha", "Shared", "Bravo", "Root", "Beta", "Last"}
	if strings.Join(hosts, ",") != strings.Join(want, ",") {
		t.Fatalf("hosts=%v want=%v", hosts, want)
	}
}

func TestParseSSHHostsRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		extra   map[string]string
		wantErr string
	}{
		{name: "missing literal include", config: "Include missing.conf\nHost ok\n", wantErr: "Include file"},
		{name: "environment include", config: "Include ${HOME}/config\nHost ok\n", wantErr: "not supported"},
		{name: "unsafe host", config: "Host -danger\n", wantErr: "unsafe Host"},
		{name: "empty host", config: "Host\n", wantErr: "requires an argument"},
		{name: "unterminated quote", config: "Host \"broken\n", wantErr: "unterminated"},
		{name: "cycle", config: "Include loop.conf\n", extra: map[string]string{"loop.conf": "Include config\n"}, wantErr: "cycle"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			sshDir := filepath.Join(home, ".ssh")
			if err := os.MkdirAll(sshDir, 0o700); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(sshDir, "config")
			writeTestFile(t, config, test.config)
			for name, content := range test.extra {
				writeTestFile(t, filepath.Join(sshDir, name), content)
			}
			if _, err := parseSSHHosts(config, home); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error=%v want=%q", err, test.wantErr)
			}
		})
	}
}

func TestRunCopySSHIDPreflightsThenContinues(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(sshDir, "config"), "Host Alpha Beta\n")
	writeTestFile(t, filepath.Join(sshDir, "id_test"), "private")
	writeTestFile(t, filepath.Join(sshDir, "id_test.pub"), "public")

	previousFind, previousRun, previousGOOS, previousDependencies := findCommand, runCommand, copySSHIDGOOS, checkDependencies
	t.Cleanup(func() {
		findCommand, runCommand, copySSHIDGOOS, checkDependencies = previousFind, previousRun, previousGOOS, previousDependencies
	})
	dependencyChecks := []dependencyFeature{}
	checkDependencies = func(feature dependencyFeature, _ io.Reader, _, _ bool, _, _ io.Writer) error {
		dependencyChecks = append(dependencyChecks, feature)
		return nil
	}
	copySSHIDGOOS = "linux"
	findCommand = func(name string) (string, error) { return "/mock/" + name, nil }
	type call struct {
		name string
		args []string
	}
	var calls []call
	runCommand = func(name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
		calls = append(calls, call{name: name, args: append([]string(nil), args...)})
		if strings.HasSuffix(name, "ssh-copy-id") {
			fmt.Fprintf(stdout, "tool: %s\n", args[len(args)-1])
			if args[len(args)-1] == "Beta" {
				return errors.New("exit status 1")
			}
			if stdin != os.Stdin {
				t.Fatal("ssh-copy-id did not inherit stdin")
			}
		}
		return nil
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"copy-ssh-id", "--identity", "~/.ssh/id_test.pub"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stdout.String(), "1 succeeded, 1 failed") || !strings.Contains(stderr.String(), "Beta: ssh-copy-id failed") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(calls) != 4 || calls[0].name != "/mock/ssh" || calls[1].name != "/mock/ssh" || calls[2].name != "/mock/ssh-copy-id" || calls[3].name != "/mock/ssh-copy-id" {
		t.Fatalf("calls=%+v", calls)
	}
	if len(dependencyChecks) != 1 || dependencyChecks[0] != dependencySSH {
		t.Fatalf("dependency checks=%v", dependencyChecks)
	}
	identity := filepath.Join(sshDir, "id_test")
	config := filepath.Join(sshDir, "config")
	if strings.Join(calls[2].args, "|") != strings.Join([]string{"-i", identity, "-F", config, "Alpha"}, "|") {
		t.Fatalf("copy args=%v", calls[2].args)
	}
}

func TestRunCopySSHIDTargetHost(t *testing.T) {
	bypassDependencyChecks(t)
	home := t.TempDir()
	setHome(t, home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(sshDir, "config"), "Host Alpha OpenWrt *.example !blocked\n")
	writeTestFile(t, filepath.Join(sshDir, "id"), "private")
	writeTestFile(t, filepath.Join(sshDir, "id.pub"), "public")

	previousFind, previousRun, previousGOOS := findCommand, runCommand, copySSHIDGOOS
	t.Cleanup(func() { findCommand, runCommand, copySSHIDGOOS = previousFind, previousRun, previousGOOS })
	copySSHIDGOOS = "linux"
	var lookups []string
	findCommand = func(name string) (string, error) {
		lookups = append(lookups, name)
		return "/mock/" + name, nil
	}
	type call struct {
		name string
		args []string
	}
	var calls []call
	runCommand = func(name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
		calls = append(calls, call{name: name, args: append([]string(nil), args...)})
		return nil
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"copy-ssh-id", "--identity=~/.ssh/id", "--target-host=openwrt"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "1 succeeded, 0 failed") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(calls) != 2 || calls[0].args[len(calls[0].args)-1] != "OpenWrt" || calls[1].args[len(calls[1].args)-1] != "OpenWrt" {
		t.Fatalf("calls=%+v", calls)
	}

	for _, target := range []string{"missing", "*.example", "blocked"} {
		lookups = nil
		calls = nil
		stdout.Reset()
		stderr.Reset()
		code := run([]string{"copy-ssh-id", "--identity=~/.ssh/id", "--target-host=" + target}, &stdout, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), "target Host") || len(lookups) != 0 || len(calls) != 0 {
			t.Fatalf("target=%q code=%d lookups=%v calls=%v stderr=%q", target, code, lookups, calls, stderr.String())
		}
	}
}

func TestRunCopySSHIDDirectFallbackUsesSSH(t *testing.T) {
	bypassDependencyChecks(t)
	home := t.TempDir()
	setHome(t, home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(sshDir, "config"), "Host Alpha\n")
	writeTestFile(t, filepath.Join(sshDir, "id"), "private")
	writeTestFile(t, filepath.Join(sshDir, "id.pub"), "ssh-ed25519 AAAA test\r\n")

	previousFind, previousRun, previousGOOS := findCommand, runCommand, copySSHIDGOOS
	t.Cleanup(func() { findCommand, runCommand, copySSHIDGOOS = previousFind, previousRun, previousGOOS })
	config := filepath.Join(sshDir, "config")
	for _, goos := range []string{"windows", "linux"} {
		t.Run(goos, func(t *testing.T) {
			copySSHIDGOOS = goos
			findCommand = func(name string) (string, error) {
				if name == "ssh-copy-id" {
					if goos == "windows" {
						t.Fatal("Windows looked for ssh-copy-id")
					}
					return "", errors.New("missing")
				}
				return "/mock/ssh", nil
			}
			var installArgs []string
			var installedKey string
			runCommand = func(name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
				if args[0] == "-G" {
					return nil
				}
				installArgs = append([]string(nil), args...)
				key, err := io.ReadAll(stdin)
				if err != nil {
					t.Fatal(err)
				}
				installedKey = string(key)
				return nil
			}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"copy-ssh-id", "--identity=~/.ssh/id"}, &stdout, &stderr); code != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			wantArgs := []string{"-F", config, "Alpha", directSSHCopyIDCommand}
			if strings.Join(installArgs, "|") != strings.Join(wantArgs, "|") || installedKey != "ssh-ed25519 AAAA test\n" {
				t.Fatalf("args=%v key=%q", installArgs, installedKey)
			}
		})
	}
}

func TestDirectSSHCopyIDCommandAvoidsDuplicateKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	home := t.TempDir()
	for _, key := range []string{"ssh-ed25519 AAAA first\n", "ssh-ed25519 AAAA changed-comment\n"} {
		command := exec.Command("sh", "-c", directSSHCopyIDCommand)
		command.Env = append(os.Environ(), "HOME="+home)
		command.Stdin = strings.NewReader(key)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("command failed: %v: %s", err, output)
		}
	}
	content, err := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "ssh-ed25519 AAAA first\n" {
		t.Fatalf("authorized_keys=%q", content)
	}
}

func TestRunCopySSHIDPreflightFailurePreventsCopies(t *testing.T) {
	bypassDependencyChecks(t)
	home := t.TempDir()
	setHome(t, home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(sshDir, "config"), "Host Alpha Beta\n")
	writeTestFile(t, filepath.Join(sshDir, "id"), "private")
	writeTestFile(t, filepath.Join(sshDir, "id.pub"), "public")

	previousFind, previousRun, previousGOOS := findCommand, runCommand, copySSHIDGOOS
	t.Cleanup(func() { findCommand, runCommand, copySSHIDGOOS = previousFind, previousRun, previousGOOS })
	copySSHIDGOOS = "linux"
	findCommand = func(name string) (string, error) { return name, nil }
	copyCalls := 0
	runCommand = func(name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
		if name == "ssh-copy-id" {
			copyCalls++
		}
		if name == "ssh" && args[len(args)-1] == "Beta" {
			fmt.Fprint(stderr, "bad config")
			return errors.New("exit status 255")
		}
		return nil
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"copy-ssh-id", "--identity=~/.ssh/id"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "validate Host Beta") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if copyCalls != 0 {
		t.Fatalf("copy calls=%d", copyCalls)
	}
}

func TestCopySSHIDRequiresToolsAndLiteralHosts(t *testing.T) {
	bypassDependencyChecks(t)
	home := t.TempDir()
	setHome(t, home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(sshDir, "config")
	writeTestFile(t, config, "Host Alpha\n")
	writeTestFile(t, filepath.Join(sshDir, "id"), "private")
	writeTestFile(t, filepath.Join(sshDir, "id.pub"), "public")

	previousFind, previousGOOS := findCommand, copySSHIDGOOS
	t.Cleanup(func() { findCommand, copySSHIDGOOS = previousFind, previousGOOS })
	copySSHIDGOOS = "linux"
	findCommand = func(name string) (string, error) {
		if name == "ssh" {
			return "", errors.New("missing")
		}
		return name, nil
	}
	if _, err := copySSHID(copySSHIDOptions{identity: filepath.Join(sshDir, "id")}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "ssh was not found") {
		t.Fatalf("tool error=%v", err)
	}

	writeTestFile(t, config, "Host *.example !blocked\n")
	if _, err := copySSHID(copySSHIDOptions{identity: filepath.Join(sshDir, "id")}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "no literal Host") {
		t.Fatalf("empty hosts error=%v", err)
	}
}

func TestDependencyPlans(t *testing.T) {
	previousFind, previousRun := findCommand, runCommand
	t.Cleanup(func() { findCommand, runCommand = previousFind, previousRun })
	runCommand = func(_ string, args []string, _ io.Reader, stdout, _ io.Writer) error {
		if len(args) == 1 && args[0] == "-V" {
			fmt.Fprint(stdout, "OpenSSH_9.0")
		}
		return nil
	}
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")

	for _, test := range []struct {
		name      string
		goos      string
		openWrt   bool
		available map[string]bool
		wayland   bool
		want      string
		wantErr   string
	}{
		{name: "OpenWrt opkg", goos: "linux", openWrt: true, available: map[string]bool{"opkg": true}, want: "opkg install openssh-client"},
		{name: "OpenWrt apk", goos: "linux", openWrt: true, available: map[string]bool{"apk": true}, want: "apk add openssh-client"},
		{name: "Linux Wayland", goos: "linux", available: map[string]bool{"ssh": true, "apt-get": true}, wayland: true, want: "apt-get install -y wl-clipboard"},
		{name: "Windows", goos: "windows", available: map[string]bool{"powershell.exe": true}, want: "Add-WindowsCapability"},
		{name: "macOS", goos: "darwin", available: map[string]bool{"ssh": true}, wantErr: "macOS system components"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.wayland {
				t.Setenv("WAYLAND_DISPLAY", "wayland-0")
			}
			findCommand = func(name string) (string, error) {
				if test.available[name] {
					return name, nil
				}
				return "", errors.New("missing")
			}
			_, commands, err := dependencyPlan(test.goos, test.openWrt, true, dependencyAll)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			display := ""
			for _, command := range commands {
				display += command.display + "\n"
			}
			if !strings.Contains(display, test.want) {
				t.Fatalf("commands=%q want %q", display, test.want)
			}
		})
	}
}

func TestLinuxDependencyPackageNames(t *testing.T) {
	for _, test := range []struct{ manager, want string }{
		{manager: "apt-get", want: "openssh-client"},
		{manager: "dnf", want: "openssh-clients"},
		{manager: "yum", want: "openssh-clients"},
		{manager: "pacman", want: "openssh"},
		{manager: "apk", want: "openssh-client-default"},
		{manager: "zypper", want: "openssh-clients"},
	} {
		packages := linuxDependencyPackages(test.manager, false, true, false, false)
		if len(packages) != 1 || packages[0] != test.want {
			t.Fatalf("manager=%s packages=%v", test.manager, packages)
		}
	}
}

func TestOptionalDependencyWarningsDoNotFail(t *testing.T) {
	previousFind, previousRun := findCommand, runCommand
	t.Cleanup(func() { findCommand, runCommand = previousFind, previousRun })
	findCommand = func(name string) (string, error) {
		if (runtime.GOOS == "linux" && name == "apt-get") || (runtime.GOOS == "windows" && name == "powershell.exe") {
			return name, nil
		}
		return "", errors.New("missing")
	}
	runCommand = func(string, []string, io.Reader, io.Writer, io.Writer) error {
		t.Fatal("warning-only check ran an install command")
		return nil
	}
	var stderr bytes.Buffer
	if err := ensureDependencies(dependencySSH, nil, false, false, io.Discard, &stderr); err != nil || !strings.Contains(stderr.String(), "copy-ssh-id") {
		t.Fatalf("error=%v stderr=%q", err, stderr.String())
	}
}

func TestFeatureDependencyInstallContinuesCommand(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux package manager behavior")
	}
	previousFind, previousRun := findCommand, runCommand
	t.Cleanup(func() { findCommand, runCommand = previousFind, previousRun })
	installed := false
	findCommand = func(name string) (string, error) {
		if name == "apt-get" || name == "sudo" || (name == "ssh" && installed) {
			return name, nil
		}
		return "", errors.New("missing")
	}
	runCommand = func(_ string, args []string, _ io.Reader, stdout, _ io.Writer) error {
		if len(args) == 1 && args[0] == "-V" {
			fmt.Fprint(stdout, "OpenSSH_9.0")
		} else if strings.Contains(strings.Join(args, " "), "openssh-client") {
			installed = true
		}
		return nil
	}
	if err := ensureDependencies(dependencySSH, strings.NewReader("yes\n"), true, true, io.Discard, io.Discard); err != nil || !installed {
		t.Fatalf("installed=%v error=%v", installed, err)
	}
}

func TestVersionCommandsAndUpdateUsage(t *testing.T) {
	previous := version
	version = "v0.1.0"
	t.Cleanup(func() { version = previous })

	for _, args := range [][]string{{"version"}, {"--version"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 || stdout.String() != "pdo v0.1.0\n" || stderr.Len() != 0 {
			t.Fatalf("args=%v code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
	for _, args := range [][]string{{"version", "extra"}, {"update", "--unknown"}, {"update", "--check", "extra"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Fatalf("args=%v code=%d", args, code)
		}
	}
	version = "devel"
	var stdout, stderr bytes.Buffer
	if code := run([]string{"update", "--check"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "development builds") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestVersionParsing(t *testing.T) {
	valid := []string{"v0.1.0", "v1.20.300"}
	for _, value := range valid {
		if _, err := parseVersion(value); err != nil {
			t.Errorf("parseVersion(%q): %v", value, err)
		}
	}
	invalid := []string{"0.1.0", "v01.0.0", "v1.0", "v1.0.0-rc.1", "v1.0.0+meta", "devel"}
	for _, value := range invalid {
		if _, err := parseVersion(value); err == nil {
			t.Errorf("parseVersion(%q) succeeded", value)
		}
	}
}

func TestUpdateCheckIsReadOnly(t *testing.T) {
	for _, test := range []struct {
		name    string
		current string
		latest  string
		want    string
	}{
		{name: "available", current: "v0.1.0", latest: "v0.1.1", want: "Update available: v0.1.0 -> v0.1.1\n"},
		{name: "current", current: "v0.1.0", latest: "v0.1.0", want: "pdo v0.1.0 is up to date\n"},
		{name: "no downgrade", current: "v0.2.0", latest: "v0.1.0", want: "pdo v0.2.0 is up to date\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(release{TagName: test.latest})
			}))
			defer server.Close()
			home := t.TempDir()
			u := updater{http: server.Client(), apiBase: server.URL, version: test.current, home: home}
			var output bytes.Buffer
			if err := u.run(true, &output); err != nil {
				t.Fatal(err)
			}
			if output.String() != test.want {
				t.Fatalf("output=%q want=%q", output.String(), test.want)
			}
			if _, err := os.Stat(filepath.Join(home, ".config")); !os.IsNotExist(err) {
				t.Fatalf("--check wrote to home: %v", err)
			}
		})
	}
}

func TestInternalMigrationIgnoresOptionalDependencies(t *testing.T) {
	home := t.TempDir()
	tx, err := startTransaction(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.cleanup() })
	previous := checkDependencies
	checkDependencies = func(dependencyFeature, io.Reader, bool, bool, io.Writer, io.Writer) error {
		t.Fatal("migration checked optional dependencies")
		return nil
	}
	t.Cleanup(func() { checkDependencies = previous })
	var stdout, stderr bytes.Buffer
	if code := run([]string{"__pdo-migrate", home, tx.dir}, &stdout, &stderr); code != 0 || stdout.String() != "0\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestAutomaticUpdate(t *testing.T) {
	previousVersion, previousAPIBase := version, releaseAPIBase
	version = "v0.1.0"
	t.Cleanup(func() { version, releaseAPIBase = previousVersion, previousAPIBase })

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		json.NewEncoder(w).Encode(release{TagName: "v0.1.1"})
	}))
	defer server.Close()
	releaseAPIBase = server.URL

	for _, answer := range []string{"Y", "y", "yes", "YES"} {
		t.Run("accept "+answer, func(t *testing.T) {
			setHome(t, t.TempDir())
			installed := false
			var stderr bytes.Buffer
			if !automaticUpdate([]string{"version"}, strings.NewReader(answer+"\n"), true, io.Discard, &stderr, func(updater, release, io.Writer) error {
				installed = true
				return nil
			}) || !installed || !strings.Contains(stderr.String(), "rerun the command") {
				t.Fatalf("answer=%q installed=%v stderr=%q", answer, installed, stderr.String())
			}
		})
	}

	t.Run("noninteractive preserves stdin and caches check", func(t *testing.T) {
		home := t.TempDir()
		setHome(t, home)
		input := strings.NewReader("piped text\n")
		var stderr bytes.Buffer
		before := requests
		if automaticUpdate([]string{"copy"}, input, false, io.Discard, &stderr, func(updater, release, io.Writer) error {
			t.Fatal("installed without confirmation")
			return nil
		}) {
			t.Fatal("noninteractive check stopped command")
		}
		remaining, _ := io.ReadAll(input)
		if string(remaining) != "piped text\n" || !strings.Contains(stderr.String(), "run 'pdo update'") {
			t.Fatalf("remaining=%q stderr=%q", remaining, stderr.String())
		}
		if automaticUpdate([]string{"copy"}, strings.NewReader("yes\n"), true, io.Discard, io.Discard, func(updater, release, io.Writer) error {
			t.Fatal("fresh cache installed update")
			return nil
		}) || requests != before+1 {
			t.Fatalf("fresh cache made another request: before=%d after=%d", before, requests)
		}
		marker := filepath.Join(home, ".config", "pdo", ".update-check")
		stale := time.Now().Add(-automaticUpdateInterval - time.Minute)
		if err := os.Chtimes(marker, stale, stale); err != nil {
			t.Fatal(err)
		}
		if automaticUpdate([]string{"copy"}, strings.NewReader("n\n"), true, io.Discard, io.Discard, func(updater, release, io.Writer) error {
			t.Fatal("installed after rejection")
			return nil
		}) || requests != before+2 {
			t.Fatalf("stale cache requests: before=%d after=%d", before, requests)
		}
	})

	t.Run("skips update internal and development commands", func(t *testing.T) {
		setHome(t, t.TempDir())
		before := requests
		for _, args := range [][]string{{"update"}, {"__pdo-migrate"}} {
			if automaticUpdate(args, strings.NewReader("yes\n"), true, io.Discard, io.Discard, func(updater, release, io.Writer) error { return nil }) {
				t.Fatalf("args=%v stopped command", args)
			}
		}
		version = "devel"
		if automaticUpdate([]string{"version"}, strings.NewReader("yes\n"), true, io.Discard, io.Discard, func(updater, release, io.Writer) error { return nil }) {
			t.Fatal("development build stopped command")
		}
		version = "v0.1.0"
		if requests != before {
			t.Fatalf("skipped commands made %d requests", requests-before)
		}
	})

	t.Run("check and install failures do not stop command", func(t *testing.T) {
		home := t.TempDir()
		setHome(t, home)
		previous := releaseAPIBase
		releaseAPIBase = "http://127.0.0.1:1"
		if automaticUpdate([]string{"version"}, strings.NewReader("yes\n"), true, io.Discard, io.Discard, func(updater, release, io.Writer) error { return nil }) {
			t.Fatal("failed check stopped command")
		}
		if _, err := os.Stat(filepath.Join(home, ".config", "pdo", ".update-check")); err != nil {
			t.Fatalf("failed check was not cached: %v", err)
		}
		releaseAPIBase = previous
		stale := time.Now().Add(-automaticUpdateInterval - time.Minute)
		marker := filepath.Join(home, ".config", "pdo", ".update-check")
		if err := os.Chtimes(marker, stale, stale); err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		if automaticUpdate([]string{"version"}, strings.NewReader("yes\n"), true, io.Discard, &stderr, func(updater, release, io.Writer) error {
			return errors.New("injected failure")
		}) || !strings.Contains(stderr.String(), "injected failure") {
			t.Fatalf("install failure stderr=%q", stderr.String())
		}
	})
}

func TestUpdateRejectsBadLatestRelease(t *testing.T) {
	for _, found := range []release{
		{TagName: "v0.1.1-rc.1"},
		{TagName: "v0.1.1", Prerelease: true},
		{TagName: "v0.1.1", Draft: true},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(found)
		}))
		u := updater{http: server.Client(), apiBase: server.URL, version: "v0.1.0"}
		if err := u.run(true, io.Discard); err == nil {
			t.Fatalf("release %+v accepted", found)
		}
		server.Close()
	}
}

func TestUpdateErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	u := updater{http: server.Client(), apiBase: server.URL, version: "v0.1.0"}
	if err := u.run(true, io.Discard); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("network error=%v", err)
	}
	if _, err := updateAssetName("freebsd", "amd64"); err == nil {
		t.Fatal("unsupported platform accepted")
	}
	u = updater{goos: runtime.GOOS, goarch: runtime.GOARCH}
	if err := u.install(release{TagName: "v0.1.1"}, io.Discard); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing asset error=%v", err)
	}
}

func TestRunAllContinuesAndSorts(t *testing.T) {
	var paths []string
	server := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/missing") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeRemoteFile(t, w, []byte("ok\n"), "sha")
	})
	setGitHubServer(t, server)
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("PDO_GITHUB_PAT", "token")
	writeConfig(t, home, map[string]dotfileConfig{
		"z-missing": {Remote: remoteURL("missing"), Local: "~/.missing"},
		"a-good":    {Remote: remoteURL("good"), Local: "~/.good"},
	})

	var stdout, stderr bytes.Buffer
	if code := run([]string{"download", "dotfiles"}, &stdout, &stderr); code != 1 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(paths) != 2 || !strings.HasSuffix(paths[0], "/good") || !strings.HasSuffix(paths[1], "/missing") {
		t.Fatalf("request paths = %v", paths)
	}
	assertFileContent(t, filepath.Join(home, ".good"), []byte("ok\n"))
	if !strings.Contains(stdout.String(), "1 succeeded, 1 failed") || !strings.Contains(stderr.String(), "z-missing") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestDownloadCreatesNoOpsAndPreservesMode(t *testing.T) {
	remote := []byte("new\n")
	server := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeRemoteFile(t, w, remote, "sha")
	})
	client := testClient(t, server)
	path := filepath.Join(t.TempDir(), "nested", "config")

	message, err := downloadDotfile(path, client)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(message, "downloaded") {
		t.Fatalf("message = %q", message)
	}
	assertFileContent(t, path, remote)
	if runtime.GOOS != "windows" {
		assertMode(t, path, 0o600)
		assertMode(t, filepath.Dir(path), 0o700)
	}

	message, err = downloadDotfile(path, client)
	if err != nil || message != "already up to date" {
		t.Fatalf("message=%q error=%v", message, err)
	}
	backups, _ := filepath.Glob(path + ".pdo-backup-*")
	if len(backups) != 0 {
		t.Fatalf("backups = %v", backups)
	}

	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := downloadDotfile(path, client); err != nil {
		t.Fatal(err)
	}
	backups, _ = filepath.Glob(path + ".pdo-backup-*")
	if len(backups) != 1 {
		t.Fatalf("backups = %v", backups)
	}
	assertFileContent(t, backups[0], []byte("old\n"))
	if runtime.GOOS != "windows" {
		assertMode(t, path, 0o640)
		assertMode(t, backups[0], 0o640)
	}
}

func TestDownloadFollowsSymlinkAndRejectsInvalidTargets(t *testing.T) {
	server := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeRemoteFile(t, w, []byte("new\n"), "sha")
	})
	client := testClient(t, server)
	home := t.TempDir()
	target := filepath.Join(home, "target")
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link1 := filepath.Join(home, "link1")
	link2 := filepath.Join(home, "link2")
	if err := os.Symlink("target", link1); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink("link1", link2); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := downloadDotfile(link2, client); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, target, []byte("new\n"))
	if info, err := os.Lstat(link2); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link changed: info=%v error=%v", info, err)
	}

	dangling := filepath.Join(home, "dangling")
	if err := os.Symlink("missing", dangling); err != nil {
		t.Fatal(err)
	}
	if _, err := downloadDotfile(dangling, client); err == nil || !strings.Contains(err.Error(), "resolve") {
		t.Fatalf("dangling error = %v", err)
	}
	directory := filepath.Join(home, "directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := downloadDotfile(directory, client); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory error = %v", err)
	}
}

func TestDownloadAPIFailureLeavesLocalUntouched(t *testing.T) {
	server := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"bad credentials"}`)
	})
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := downloadDotfile(path, testClient(t, server)); err == nil {
		t.Fatal("expected error")
	}
	assertFileContent(t, path, []byte("local\n"))
	backups, _ := filepath.Glob(path + ".pdo-backup-*")
	if len(backups) != 0 {
		t.Fatalf("backups = %v", backups)
	}
}

func TestUploadCreatesUpdatesNoOpsAndConflicts(t *testing.T) {
	tests := []struct {
		name       string
		getStatus  int
		putStatus  int
		remote     []byte
		wantPutSHA string
		wantErr    string
		wantPuts   int
	}{
		{name: "create", getStatus: http.StatusNotFound, putStatus: http.StatusCreated, wantPuts: 1},
		{name: "update", getStatus: http.StatusOK, putStatus: http.StatusOK, remote: []byte("remote\n"), wantPutSHA: "sha", wantPuts: 1},
		{name: "no-op", getStatus: http.StatusOK, remote: []byte("local\n")},
		{name: "conflict", getStatus: http.StatusOK, putStatus: http.StatusConflict, remote: []byte("remote\n"), wantPutSHA: "sha", wantErr: "changed during upload", wantPuts: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			putCalls := 0
			server := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					if test.getStatus == http.StatusNotFound {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					writeRemoteFile(t, w, test.remote, "sha")
					return
				}
				putCalls++
				var payload struct {
					Message string `json:"message"`
					Content string `json:"content"`
					SHA     string `json:"sha"`
					Branch  string `json:"branch"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				content, err := base64.StdEncoding.DecodeString(payload.Content)
				if err != nil || string(content) != "local\n" || payload.Message != "pdo: upload dotfiles --ssh-config" || payload.SHA != test.wantPutSHA || payload.Branch != "main" {
					t.Fatalf("payload=%+v content=%q error=%v", payload, content, err)
				}
				w.WriteHeader(test.putStatus)
				if test.putStatus == http.StatusConflict {
					fmt.Fprint(w, `{"message":"conflict"}`)
				}
			})
			client := testClient(t, server)
			path := filepath.Join(t.TempDir(), "config")
			if err := os.WriteFile(path, []byte("local\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			message, err := uploadDotfile(path, client)
			if test.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if test.name == "no-op" && message != "already up to date" {
					t.Fatalf("message = %q", message)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v", err)
			}
			if putCalls != test.wantPuts {
				t.Fatalf("PUT calls = %d, want %d", putCalls, test.wantPuts)
			}
		})
	}

	if _, err := uploadDotfile(filepath.Join(t.TempDir(), "missing"), testClient(t, githubServer(t, func(http.ResponseWriter, *http.Request) {}))); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing error = %v", err)
	}
}

func TestUploadFollowsSymlink(t *testing.T) {
	var uploaded []byte
	server := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var payload struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		uploaded, _ = base64.StdEncoding.DecodeString(payload.Content)
		w.WriteHeader(http.StatusCreated)
	})
	home := t.TempDir()
	target := filepath.Join(home, "target")
	link := filepath.Join(home, "link")
	if err := os.WriteFile(target, []byte("linked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := uploadDotfile(link, testClient(t, server)); err != nil {
		t.Fatal(err)
	}
	if string(uploaded) != "linked\n" {
		t.Fatalf("uploaded = %q", uploaded)
	}
}

func TestGitHubRequestAndTokenRedaction(t *testing.T) {
	server := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/contents/ssh/config" || r.URL.Query().Get("ref") != "main" {
			t.Fatalf("URL = %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer token-for-test" || r.Header.Get("User-Agent") != "pdo" {
			t.Fatalf("headers = %v", r.Header)
		}
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"token-for-test rejected"}`)
	})
	_, _, _, err := testClient(t, server).get()
	if err == nil || strings.Contains(err.Error(), "token-for-test") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error = %v", err)
	}
}

func TestMigrationPreservesUnknownFieldsSymlinkModeAndRollsBack(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, ".config", "pdo")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "actual-config.json")
	original := []byte("{\"schema_version\":1,\"future\":{\"keep\":true}}\n")
	if err := os.WriteFile(target, original, 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(configDir, "config.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	tx, err := startTransaction(home)
	if err != nil {
		t.Fatal(err)
	}
	steps := map[int]jsonMigration{
		1: func(document map[string]json.RawMessage) error {
			document["added"] = json.RawMessage(`"one"`)
			return nil
		},
		2: func(document map[string]json.RawMessage) error {
			document["added"] = json.RawMessage(`"two"`)
			return nil
		},
	}
	changed, err := migrateJSONFile(link, 3, steps, tx)
	if err != nil || !changed {
		t.Fatalf("changed=%v error=%v", changed, err)
	}
	var migrated map[string]json.RawMessage
	data, _ := os.ReadFile(target)
	if err := json.Unmarshal(data, &migrated); err != nil {
		t.Fatal(err)
	}
	var future struct {
		Keep bool `json:"keep"`
	}
	if err := json.Unmarshal(migrated["future"], &future); err != nil {
		t.Fatal(err)
	}
	if string(migrated["schema_version"]) != "3" || string(migrated["added"]) != `"two"` || !future.Keep {
		t.Fatalf("migrated=%s", data)
	}
	if runtime.GOOS != "windows" {
		assertMode(t, target, 0o640)
		assertMode(t, filepath.Join(tx.dir, "file-0"), 0o600)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link changed: info=%v error=%v", info, err)
	}
	if err := tx.rollback(); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, target, original)
	if err := tx.cleanup(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationRestoresMissingFileAndRequiresContinuousChain(t *testing.T) {
	home := t.TempDir()
	tx, err := startTransaction(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config", "pdo", "data.json")
	if err := tx.backup(path); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(path, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tx.rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("new file was not removed: %v", err)
	}
	if err := tx.cleanup(); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(home, ".config", "pdo", "config.json")
	if err := os.WriteFile(configPath, []byte(`{"schema_version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tx, err = startTransaction(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrateJSONFile(configPath, 3, map[int]jsonMigration{1: func(map[string]json.RawMessage) error { return nil }}, tx); err == nil || !strings.Contains(err.Error(), "missing migration") {
		t.Fatalf("error=%v", err)
	}
	if len(tx.manifest.Files) != 0 {
		t.Fatalf("files were backed up before chain validation: %+v", tx.manifest.Files)
	}
	tx.cleanup()
}

func TestRollbackRestoresMigrationAndExecutable(t *testing.T) {
	home := t.TempDir()
	tx, err := startTransaction(home)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, ".config", "pdo", "config.json")
	if err := os.WriteFile(configPath, []byte("old config"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tx.backup(configPath); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(configPath, []byte("new config"), 0o600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(home, "pdo")
	backup := executable + ".pdo-update-old"
	if err := os.WriteFile(executable, []byte("new binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	cause := fmt.Errorf("injected failure")
	if err := rollbackUpdate(tx, executable, backup, runtime.GOOS, cause); err == nil || err.Error() != cause.Error() {
		t.Fatalf("rollback error=%v", err)
	}
	assertFileContent(t, configPath, []byte("old config"))
	assertFileContent(t, executable, []byte("old binary"))
	if _, err := os.Stat(tx.dir); !os.IsNotExist(err) {
		t.Fatalf("transaction remains: %v", err)
	}
}

func TestSameVersionUpdateRunsNoOpMigrationWithoutPAT(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(release{TagName: "v0.1.0"})
	}))
	defer server.Close()
	home := t.TempDir()
	configDir := filepath.Join(home, ".config", "pdo")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "config.json")
	original := []byte(`{"schema_version":1,"unknown":true}`)
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	u := updater{http: server.Client(), apiBase: server.URL, version: "v0.1.0", home: home}
	var output bytes.Buffer
	if err := u.run(false, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "pdo v0.1.0 is up to date\n" {
		t.Fatalf("output=%q", output.String())
	}
	assertFileContent(t, configPath, original)
	if _, err := os.Stat(filepath.Join(configDir, ".update-transaction")); !os.IsNotExist(err) {
		t.Fatalf("transaction was not cleaned: %v", err)
	}
}

func TestUpdateInstallsVerifiedCandidate(t *testing.T) {
	candidate := buildCandidate(t, "v0.1.1")
	binary, err := os.ReadFile(candidate)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := updateAssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(binary))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			json.NewEncoder(w).Encode(release{TagName: "v0.1.1", Assets: []releaseAsset{{Name: asset}, {Name: "SHA256SUMS"}}})
		case "/v0.1.1/SHA256SUMS":
			fmt.Fprintf(w, "%s  %s\n", sum, asset)
		case "/v0.1.1/" + asset:
			w.Write(binary)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	home := t.TempDir()
	target := filepath.Join(home, "pdo")
	if runtime.GOOS == "windows" {
		target += ".exe"
	}
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	u := updater{
		http:         server.Client(),
		apiBase:      server.URL,
		downloadBase: server.URL,
		version:      "v0.1.0",
		goos:         runtime.GOOS,
		goarch:       runtime.GOARCH,
		executable:   target,
		home:         home,
	}
	var output bytes.Buffer
	if err := u.run(false, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "Updated pdo from v0.1.0 to v0.1.1\n" {
		t.Fatalf("output=%q", output.String())
	}
	if err := verifyExecutableVersion(target, "v0.1.1"); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if _, err := os.Stat(target + ".pdo-update-old"); err != nil {
			t.Fatalf("Windows old executable was not retained: %v", err)
		}
		if err := cleanupOldExecutable(target); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUpdateRejectsBadHashAndCandidateVersion(t *testing.T) {
	candidate := buildCandidate(t, "v0.1.1")
	binary, err := os.ReadFile(candidate)
	if err != nil {
		t.Fatal(err)
	}
	asset, _ := updateAssetName(runtime.GOOS, runtime.GOARCH)
	for _, test := range []struct {
		name             string
		tag              string
		sum              string
		dependenciesFail bool
		want             string
	}{
		{name: "hash", tag: "v0.1.1", sum: strings.Repeat("0", 64), want: "checksum verification failed"},
		{name: "version", tag: "v0.1.2", sum: fmt.Sprintf("%x", sha256.Sum256(binary)), want: "candidate version mismatch"},
		{name: "dependencies", tag: "v0.1.1", sum: fmt.Sprintf("%x", sha256.Sum256(binary)), dependenciesFail: true, want: "candidate dependency check failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.dependenciesFail {
				t.Setenv("PDO_TEST_DEPENDENCIES_FAIL", "1")
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/releases/latest":
					json.NewEncoder(w).Encode(release{TagName: test.tag, Assets: []releaseAsset{{Name: asset}, {Name: "SHA256SUMS"}}})
				case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
					fmt.Fprintf(w, "%s  %s\n", test.sum, asset)
				default:
					w.Write(binary)
				}
			}))
			defer server.Close()
			home := t.TempDir()
			target := filepath.Join(home, "pdo")
			if runtime.GOOS == "windows" {
				target += ".exe"
			}
			if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
				t.Fatal(err)
			}
			u := updater{http: server.Client(), apiBase: server.URL, downloadBase: server.URL, version: "v0.1.0", goos: runtime.GOOS, goarch: runtime.GOARCH, executable: target, home: home}
			if err := u.run(false, io.Discard); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v", err)
			}
			assertFileContent(t, target, []byte("old"))
		})
	}
}

func TestReplaceRunningExecutable(t *testing.T) {
	if os.Getenv("PDO_REPLACE_TEST_CHILD") == "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := replaceExecutable(executable, os.Getenv("PDO_REPLACE_TEST_STAGED"), 0o755, runtime.GOOS); err != nil {
			t.Fatal(err)
		}
		return
	}
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	running := filepath.Join(dir, "running")
	staged := filepath.Join(dir, "staged")
	if runtime.GOOS == "windows" {
		running += ".exe"
		staged += ".exe"
	}
	copyFile(t, executable, running, 0o755)
	copyFile(t, executable, staged, 0o755)
	command := exec.Command(running, "-test.run=^TestReplaceRunningExecutable$")
	command.Env = append(os.Environ(), "PDO_REPLACE_TEST_CHILD=1", "PDO_REPLACE_TEST_STAGED="+staged)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("replace child: %v: %s", err, output)
	}
	if _, err := os.Stat(running); err != nil {
		t.Fatal(err)
	}
	if err := cleanupOldExecutable(running); err != nil {
		t.Fatal(err)
	}
}

func buildCandidate(t *testing.T, candidateVersion string) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	program := `package main
import ("fmt"; "os")
func main() {
 if len(os.Args) == 2 && os.Args[1] == "version" { fmt.Println("pdo ` + candidateVersion + `"); return }
 if len(os.Args) == 2 && os.Args[1] == "__pdo-dependencies" { if os.Getenv("PDO_TEST_DEPENDENCIES_FAIL") == "1" { os.Exit(1) }; return }
 if len(os.Args) == 4 && os.Args[1] == "__pdo-migrate" { fmt.Println("0"); return }
 os.Exit(2)
}`
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "candidate")
	if runtime.GOOS == "windows" {
		output += ".exe"
	}
	command := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", output, source)
	if data, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build candidate: %v: %s", err, data)
	}
	return output
}

func copyFile(t *testing.T, source, destination string, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, data, mode); err != nil {
		t.Fatal(err)
	}
}

func githubServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func setGitHubServer(t *testing.T, server *httptest.Server) {
	t.Helper()
	previous := githubAPIBase
	githubAPIBase = server.URL
	t.Cleanup(func() { githubAPIBase = previous })
}

func testClient(t *testing.T, server *httptest.Server) *githubClient {
	t.Helper()
	setGitHubServer(t, server)
	return &githubClient{
		http:       server.Client(),
		token:      "token-for-test",
		repository: "owner/repo",
		branch:     "main",
		path:       "ssh/config",
		message:    "pdo: upload dotfiles --ssh-config",
	}
}

func remoteURL(path string) string {
	return "https://github.com/owner/repo/blob/main/" + path
}

func setHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

func runSetupForTest(t *testing.T, inputText string, secrets ...string) (*bytes.Buffer, *bytes.Buffer, int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "setup-input")
	if err := os.WriteFile(path, []byte(inputText), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	previousInput, previousTerminal, previousSecret := setupInput, isSetupTerminal, readSetupSecret
	setupInput = input
	isSetupTerminal = func(*os.File) bool { return true }
	readSetupSecret = func(*os.File) ([]byte, error) {
		if len(secrets) == 0 {
			return nil, io.EOF
		}
		secret := secrets[0]
		secrets = secrets[1:]
		return []byte(secret), nil
	}
	t.Cleanup(func() {
		setupInput, isSetupTerminal, readSetupSecret = previousInput, previousTerminal, previousSecret
		input.Close()
	})
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	return stdout, stderr, run([]string{"setup"}, stdout, stderr)
}

func bypassDependencyChecks(t *testing.T) {
	t.Helper()
	previous := checkDependencies
	checkDependencies = func(dependencyFeature, io.Reader, bool, bool, io.Writer, io.Writer) error { return nil }
	t.Cleanup(func() { checkDependencies = previous })
}

func writeConfig(t *testing.T, home string, dotfiles map[string]dotfileConfig) {
	t.Helper()
	configDir := filepath.Join(home, ".config", "pdo")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(config{
		SchemaVersion: 1,
		GitHub:        githubConfig{PATEnv: "PDO_GITHUB_PAT"},
		Dotfiles:      dotfiles,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeRemoteFile(t *testing.T, writer http.ResponseWriter, content []byte, sha string) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(githubFile{
		Content:  base64.StdEncoding.EncodeToString(content),
		Encoding: "base64",
		SHA:      sha,
		Type:     "file",
	}); err != nil {
		t.Fatal(err)
	}
}

func assertFileContent(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("%s mode = %o, want %o", path, info.Mode().Perm(), want)
	}
}

type cloudClipboardFixture struct {
	server         *httptest.Server
	mutex          sync.Mutex
	next           int
	rooms          map[string]cloudContent
	files          map[string][]byte
	uploads        map[string]*cloudUploadFixture
	chunkSizes     []int
	deleted        int
	finishRequests int
	failChunk      bool
	failFinish     bool
}

type cloudUploadFixture struct {
	room string
	name string
	data []byte
}

func newCloudClipboardFixture(t *testing.T) *cloudClipboardFixture {
	t.Helper()
	fixture := &cloudClipboardFixture{
		rooms:   make(map[string]cloudContent),
		files:   make(map[string][]byte),
		uploads: make(map[string]*cloudUploadFixture),
	}
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer secret" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		path := strings.TrimPrefix(request.URL.Path, "/api")
		switch {
		case path == "/text" && request.Method == http.MethodPost:
			if request.Header.Get("Content-Type") != "text/plain" {
				http.Error(writer, "bad content type", http.StatusBadRequest)
				return
			}
			data, err := io.ReadAll(request.Body)
			if err != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)
				return
			}
			room := request.URL.Query().Get("room")
			content := string(data)
			fixture.mutex.Lock()
			fixture.next++
			id := fmt.Sprint(fixture.next)
			fixture.rooms[room] = cloudContent{Type: "text", Content: &content}
			fixture.mutex.Unlock()
			writeJSON(writer, map[string]string{"id": id, "type": "text", "url": "https://untrusted.example/content/" + id})

		case path == "/upload/chunk" && request.Method == http.MethodPost:
			if request.Header.Get("Content-Type") != "text/plain" {
				http.Error(writer, "bad content type", http.StatusBadRequest)
				return
			}
			name, err := io.ReadAll(request.Body)
			if err != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)
				return
			}
			fixture.mutex.Lock()
			fixture.next++
			uuid := fmt.Sprintf("00000000-0000-4000-8000-%012x", fixture.next)
			fixture.uploads[uuid] = &cloudUploadFixture{room: request.URL.Query().Get("room"), name: string(name)}
			fixture.mutex.Unlock()
			writeJSON(writer, map[string]any{"result": map[string]string{"uuid": uuid}})

		case strings.HasPrefix(path, "/upload/chunk/") && request.Method == http.MethodPost:
			data, err := io.ReadAll(request.Body)
			if err != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)
				return
			}
			uuid := strings.TrimPrefix(path, "/upload/chunk/")
			fixture.mutex.Lock()
			upload := fixture.uploads[uuid]
			fail := fixture.failChunk
			if upload != nil && !fail {
				upload.data = append(upload.data, data...)
				fixture.chunkSizes = append(fixture.chunkSizes, len(data))
			}
			fixture.mutex.Unlock()
			if fail {
				http.Error(writer, "chunk failed", http.StatusInternalServerError)
				return
			}
			if upload == nil {
				http.Error(writer, "unknown upload", http.StatusBadRequest)
				return
			}
			writeJSON(writer, map[string]any{})

		case strings.HasPrefix(path, "/upload/finish/") && request.Method == http.MethodPost:
			uuid := strings.TrimPrefix(path, "/upload/finish/")
			fixture.mutex.Lock()
			fixture.finishRequests++
			upload := fixture.uploads[uuid]
			fail := fixture.failFinish
			if upload != nil && !fail {
				size := int64(len(upload.data))
				kind := "file"
				if strings.HasSuffix(strings.ToLower(upload.name), ".png") {
					kind = "image"
				} else if strings.HasSuffix(strings.ToLower(upload.name), ".txt") {
					kind = "text"
				}
				fixture.files[uuid] = append([]byte(nil), upload.data...)
				fixture.rooms[upload.room] = cloudContent{Type: kind, Name: upload.name, Size: &size, UUID: uuid}
				fixture.next++
			}
			id := fmt.Sprint(fixture.next)
			fixture.mutex.Unlock()
			if fail {
				http.Error(writer, "finish failed", http.StatusInternalServerError)
				return
			}
			if upload == nil || request.URL.Query().Get("room") != upload.room {
				http.Error(writer, "unknown upload", http.StatusBadRequest)
				return
			}
			writeJSON(writer, map[string]string{"id": id, "type": "file", "url": "https://untrusted.example/content/" + id})

		case path == "/content/latest" && request.Method == http.MethodGet:
			if request.URL.Query().Get("json") != "true" {
				http.Error(writer, "json required", http.StatusBadRequest)
				return
			}
			fixture.mutex.Lock()
			content, ok := fixture.rooms[request.URL.Query().Get("room")]
			fixture.mutex.Unlock()
			if !ok {
				http.Error(writer, "not found", http.StatusNotFound)
				return
			}
			if content.UUID != "" {
				writeJSON(writer, map[string]any{
					"type": content.Type, "name": content.Name, "size": content.Size,
					"uuid": content.UUID, "url": "https://untrusted.example/file",
				})
			} else {
				writeJSON(writer, content)
			}

		case strings.HasPrefix(path, "/file/") && request.Method == http.MethodGet:
			uuid := strings.TrimPrefix(path, "/file/")
			fixture.mutex.Lock()
			data, ok := fixture.files[uuid]
			data = append([]byte(nil), data...)
			fixture.mutex.Unlock()
			if !ok {
				http.Error(writer, "not found", http.StatusNotFound)
				return
			}
			writer.Header().Set("Content-Length", fmt.Sprint(len(data)))
			writer.Write(data)

		case strings.HasPrefix(path, "/file/") && request.Method == http.MethodDelete:
			uuid := strings.TrimPrefix(path, "/file/")
			fixture.mutex.Lock()
			delete(fixture.files, uuid)
			delete(fixture.uploads, uuid)
			fixture.deleted++
			fixture.mutex.Unlock()
			writeJSON(writer, map[string]string{"status": "deleted"})

		default:
			http.Error(writer, "unknown request", http.StatusBadRequest)
		}
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (fixture *cloudClipboardFixture) setFile(room, name string, data []byte) {
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	fixture.next++
	uuid := fmt.Sprintf("00000000-0000-4000-8000-%012x", fixture.next)
	size := int64(len(data))
	fixture.files[uuid] = append([]byte(nil), data...)
	fixture.rooms[room] = cloudContent{Type: "file", Name: name, Size: &size, UUID: uuid}
}

func (fixture *cloudClipboardFixture) uploadChunkSizes() []int {
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	return append([]int(nil), fixture.chunkSizes...)
}

func writeJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	json.NewEncoder(writer).Encode(value)
}

func setCloudServer(t *testing.T, server *httptest.Server) {
	t.Helper()
	previous := cloudHTTPClient
	cloudHTTPClient = func() *http.Client { return server.Client() }
	t.Cleanup(func() { cloudHTTPClient = previous })
}

func writeCloudConfig(t *testing.T, home, host string) {
	t.Helper()
	t.Setenv("PDO_CLOUD_CLIPBOARD_PASSWORD", "secret")
	configDirectory := filepath.Join(home, ".config", "pdo")
	if err := os.MkdirAll(configDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(config{
		SchemaVersion: 1,
		CloudClipboard: cloudClipboardConfig{
			Host:        strings.TrimRight(host, "/") + "/api/",
			Room:        "personal",
			PasswordEnv: "PDO_CLOUD_CLIPBOARD_PASSWORD",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDirectory, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
