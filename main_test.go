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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
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

func TestCloudClipboardConfigValidation(t *testing.T) {
	t.Setenv("PDO_CLOUD_CLIPBOARD_PASSWORD", "secret")
	valid := config{CloudClipboard: cloudClipboardConfig{
		Host:        "https://clipboard.example.com/",
		Prefix:      "personal",
		Username:    "pdo",
		PasswordEnv: "PDO_CLOUD_CLIPBOARD_PASSWORD",
	}}
	if client, err := valid.prepareCloudClipboard(); err != nil || client.password != "secret" {
		t.Fatalf("client=%+v error=%v", client, err)
	}
	invalidHosts := []string{
		"http://clipboard.example.com/",
		"https://user@clipboard.example.com/",
		"https://clipboard.example.com/webdis",
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
	for _, field := range []string{"host", "prefix", "username", "password_env"} {
		cfg := valid
		switch field {
		case "host":
			cfg.CloudClipboard.Host = ""
		case "prefix":
			cfg.CloudClipboard.Prefix = ""
		case "username":
			cfg.CloudClipboard.Username = ""
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
	fixture := newWebdisFixture(t)
	home := t.TempDir()
	setHome(t, home)
	writeCloudConfig(t, home, fixture.server.URL)
	setCloudServer(t, fixture.server)

	previousRead, previousWrite := readClipboard, writeClipboard
	t.Cleanup(func() { readClipboard, writeClipboard = previousRead, previousWrite })
	var writtenKind string
	var writtenData []byte
	writeClipboard = func(kind string, data []byte) error {
		writtenKind = kind
		writtenData = append([]byte(nil), data...)
		return nil
	}

	text := "中文\n\"quoted\""
	var stdout, stderr bytes.Buffer
	if code := run([]string{"copy", text}, &stdout, &stderr); code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("copy code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if code := run([]string{"paste"}, &stdout, &stderr); code != 0 || stdout.String() != text || writtenKind != "text" || string(writtenData) != text {
		t.Fatalf("paste code=%d stdout=%q stderr=%q kind=%q data=%q", code, stdout.String(), stderr.String(), writtenKind, writtenData)
	}

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
	if code := run([]string{"paste"}, &stdout, &stderr); code != 0 || stdout.String() != fmt.Sprintf("image/png 2x3 %d bytes\n", imageData.Len()) || writtenKind != "image-png" || !bytes.Equal(writtenData, imageData.Bytes()) {
		t.Fatalf("image paste code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	fixture.set("personal:pdo:clipboard:type", []byte("image-png"))
	fixture.set("personal:pdo:clipboard:data", []byte("not a png"))
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
	fixture := newWebdisFixture(t)
	home := t.TempDir()
	setHome(t, home)
	writeCloudConfig(t, home, fixture.server.URL)
	setCloudServer(t, fixture.server)

	sourceDirectory := t.TempDir()
	target := filepath.Join(sourceDirectory, "target.bin")
	content := []byte{0, 1, 2, 3, 255}
	if err := os.WriteFile(target, content, 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(sourceDirectory, "資料 file.bin")
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
	destination := t.TempDir()
	existing := filepath.Join(destination, filepath.Base(source))
	if err := os.WriteFile(existing, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := run([]string{"paste-file", destination}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("paste-file code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	first := filepath.Join(destination, "資料 file (1).bin")
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
	second := filepath.Join(destination, "資料 file (2).bin")
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

	fixture.set("personal:pdo:file:name", []byte("../escape"))
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"paste-file", destination}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "invalid remote file name") {
		t.Fatalf("invalid name code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
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

func TestWebdisRESPValidationAndProgress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, "*2\r\n$4\r\ntext\r\n$67108865\r\n")
	}))
	defer server.Close()
	client := webdisClient{http: server.Client(), host: server.URL, username: "pdo", password: "secret"}
	called := false
	err := client.getPair("type", "data", func(string, int64, io.Reader) error {
		called = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "64 MiB") || called {
		t.Fatalf("oversized RESP error=%v called=%v", err, called)
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
		args     []string
		config   string
		identity string
		wantErr  bool
	}{
		{args: []string{"--identity=key"}, identity: "key"},
		{args: []string{"--config", "config", "--identity", "key"}, config: "config", identity: "key"},
		{args: []string{"--identity", "key", "--config=config"}, config: "config", identity: "key"},
		{args: nil, wantErr: true},
		{args: []string{"--identity"}, wantErr: true},
		{args: []string{"--identity="}, wantErr: true},
		{args: []string{"--identity", "--config=config"}, wantErr: true},
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
		if err != nil || options.config != test.config || options.identity != test.identity {
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

	previousFind, previousRun := findCommand, runCommand
	t.Cleanup(func() { findCommand, runCommand = previousFind, previousRun })
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
	identity := filepath.Join(sshDir, "id_test")
	config := filepath.Join(sshDir, "config")
	if strings.Join(calls[2].args, "|") != strings.Join([]string{"-i", identity, "-F", config, "Alpha"}, "|") {
		t.Fatalf("copy args=%v", calls[2].args)
	}
}

func TestRunCopySSHIDPreflightFailurePreventsCopies(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(sshDir, "config"), "Host Alpha Beta\n")
	writeTestFile(t, filepath.Join(sshDir, "id"), "private")
	writeTestFile(t, filepath.Join(sshDir, "id.pub"), "public")

	previousFind, previousRun := findCommand, runCommand
	t.Cleanup(func() { findCommand, runCommand = previousFind, previousRun })
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

	previousFind := findCommand
	t.Cleanup(func() { findCommand = previousFind })
	findCommand = func(name string) (string, error) {
		if name == "ssh-copy-id" {
			return "", errors.New("missing")
		}
		return name, nil
	}
	if _, err := copySSHID(copySSHIDOptions{identity: filepath.Join(sshDir, "id")}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "ssh-copy-id was not found") {
		t.Fatalf("tool error=%v", err)
	}

	writeTestFile(t, config, "Host *.example !blocked\n")
	if _, err := copySSHID(copySSHIDOptions{identity: filepath.Join(sshDir, "id")}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "no literal Host") {
		t.Fatalf("empty hosts error=%v", err)
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
		name string
		tag  string
		sum  string
		want string
	}{
		{name: "hash", tag: "v0.1.1", sum: strings.Repeat("0", 64), want: "checksum verification failed"},
		{name: "version", tag: "v0.1.2", sum: fmt.Sprintf("%x", sha256.Sum256(binary)), want: "candidate version mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
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

type webdisFixture struct {
	server *httptest.Server
	mutex  sync.Mutex
	values map[string][]byte
}

func newWebdisFixture(t *testing.T) *webdisFixture {
	t.Helper()
	fixture := &webdisFixture{values: make(map[string][]byte)}
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		if !ok || username != "pdo" || password != "secret" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		escapedPath := strings.TrimPrefix(request.URL.EscapedPath(), "/")
		parts := strings.Split(escapedPath, "/")
		if len(parts) == 0 {
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}
		parts[len(parts)-1] = strings.TrimSuffix(parts[len(parts)-1], ".raw")
		for index := range parts {
			decoded, err := url.PathUnescape(parts[index])
			if err != nil {
				http.Error(writer, "bad escape", http.StatusBadRequest)
				return
			}
			parts[index] = decoded
		}
		switch parts[0] {
		case "MSET":
			if request.Method != http.MethodPut || len(parts) != 4 {
				http.Error(writer, "bad MSET", http.StatusBadRequest)
				return
			}
			data, err := io.ReadAll(request.Body)
			if err != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)
				return
			}
			fixture.mutex.Lock()
			fixture.values[parts[1]] = []byte(parts[2])
			fixture.values[parts[3]] = data
			fixture.mutex.Unlock()
			io.WriteString(writer, "+OK\r\n")
		case "MGET":
			if request.Method != http.MethodGet || len(parts) != 3 {
				http.Error(writer, "bad MGET", http.StatusBadRequest)
				return
			}
			fixture.mutex.Lock()
			values := [][]byte{
				append([]byte(nil), fixture.values[parts[1]]...),
				append([]byte(nil), fixture.values[parts[2]]...),
			}
			_, firstPresent := fixture.values[parts[1]]
			_, secondPresent := fixture.values[parts[2]]
			fixture.mutex.Unlock()
			io.WriteString(writer, "*2\r\n")
			for index, value := range values {
				present := firstPresent
				if index == 1 {
					present = secondPresent
				}
				if !present {
					io.WriteString(writer, "$-1\r\n")
					continue
				}
				fmt.Fprintf(writer, "$%d\r\n", len(value))
				writer.Write(value)
				io.WriteString(writer, "\r\n")
			}
		default:
			http.Error(writer, "unknown command", http.StatusBadRequest)
		}
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (fixture *webdisFixture) set(key string, value []byte) {
	fixture.mutex.Lock()
	fixture.values[key] = append([]byte(nil), value...)
	fixture.mutex.Unlock()
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
			Host:        host,
			Prefix:      "personal",
			Username:    "pdo",
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
