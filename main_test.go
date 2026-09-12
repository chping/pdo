package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestConfigValidation(t *testing.T) {
	home := t.TempDir()
	valid := config{
		SchemaVersion: 1,
		GitHub:        githubConfig{PATEnv: "PDO_GITHUB_PAT"},
		Dotfiles: map[string]dotfileConfig{
			"ssh-config": {
				Remote: "https://api.github.com/repos/owner/repo/contents/ssh/config?ref=feature%2Fone",
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
			item.Remote = "https://github.com/owner/repo/blob/main/config"
			cfg.Dotfiles["ssh-config"] = item
		}, wantErr: "Contents API URL"},
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
				if prepared["ssh-config"].local != filepath.Join(home, ".ssh", "config") || prepared["ssh-config"].branch != "feature/one" {
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

func TestParseRemoteRejectsInvalidURLs(t *testing.T) {
	invalid := []string{
		"http://api.github.com/repos/o/r/contents/a?ref=main",
		"https://token@api.github.com/repos/o/r/contents/a?ref=main",
		"https://api.github.com/repos/o/r/contents/a",
		"https://api.github.com/repos/o/r/contents/a?ref=main&download=1",
		"https://api.github.com/repos/o/r/contents/../a?ref=main",
		"https://api.github.com/repos/o/r/contents/?ref=main",
		"https://api.github.com/repos/o/r/contents/a?ref=main#fragment",
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
	return "https://api.github.com/repos/owner/repo/contents/" + path + "?ref=main"
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
