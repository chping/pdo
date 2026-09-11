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

func TestLoadConfig(t *testing.T) {
	valid := `{
  "schema_version": 1,
  "github": {"pat_env": "PDO_GITHUB_PAT"},
  "ssh_config": {"repository": "owner/repo", "branch": "main", "path": "ssh/config"},
  "future_command": {"enabled": true}
}`
	tests := []struct {
		name        string
		content     string
		validateSSH bool
		wantErr     string
	}{
		{name: "valid with unknown feature", content: valid, validateSSH: true},
		{name: "unrelated feature does not need ssh config", content: `{"schema_version":1,"future_command":{"enabled":true}}`},
		{name: "newer schema", content: strings.Replace(valid, `"schema_version": 1`, `"schema_version": 2`, 1), wantErr: "unsupported schema_version"},
		{name: "missing PAT env", content: strings.Replace(valid, `"PDO_GITHUB_PAT"`, `""`, 1), validateSSH: true, wantErr: "github.pat_env is required"},
		{name: "invalid repository", content: strings.Replace(valid, `"owner/repo"`, `"owner/repo/extra"`, 1), validateSSH: true, wantErr: "OWNER/REPOSITORY"},
		{name: "missing branch", content: strings.Replace(valid, `"main"`, `""`, 1), validateSSH: true, wantErr: "ssh_config.branch is required"},
		{name: "absolute path", content: strings.Replace(valid, `"ssh/config"`, `"/ssh/config"`, 1), validateSSH: true, wantErr: "relative repository path"},
		{name: "traversal path", content: strings.Replace(valid, `"ssh/config"`, `"ssh/../config"`, 1), validateSSH: true, wantErr: "must not contain"},
		{name: "invalid json", content: `{`, wantErr: "parse config"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfig(path)
			if err == nil && test.validateSSH {
				err = cfg.validateSSHConfig()
			}
			if test.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if test.validateSSH && cfg.SSHConfig.Repository != "owner/repo" {
					t.Fatalf("repository = %q", cfg.SSHConfig.Repository)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestRunUsageExitCodes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "pdo download") {
		t.Fatalf("help: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"download", "unknown"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "Usage:") {
		t.Fatalf("invalid command: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if code := run([]string{"download", "ssh-config"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "read config") {
		t.Fatalf("runtime error: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunDownload(t *testing.T) {
	remote := []byte("Host integrated\n")
	server := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer integration-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		writeRemoteFile(t, w, remote, "sha")
	})
	previous := githubAPIBase
	githubAPIBase = server.URL
	t.Cleanup(func() { githubAPIBase = previous })

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PDO_GITHUB_PAT", "integration-token")
	configDir := filepath.Join(home, ".config", "pdo")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configJSON := `{
  "schema_version": 1,
  "github": {"pat_env": "PDO_GITHUB_PAT"},
  "ssh_config": {"repository": "owner/repo", "branch": "main", "path": "ssh/config"}
}`
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"download", "ssh-config"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	assertFileContent(t, filepath.Join(home, ".ssh", "config"), remote)
}

func TestDownloadCreatesAndNoOps(t *testing.T) {
	remote := []byte("Host example\n  HostName example.com\n")
	server := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		assertGitHubGet(t, r)
		writeRemoteFile(t, w, remote, "remote-sha")
	})
	client := testClient(t, server)
	home := t.TempDir()

	message, err := downloadSSHConfig(home, client)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".ssh", "config")
	assertFileContent(t, path, remote)
	if !strings.Contains(message, path) {
		t.Fatalf("message = %q", message)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %o", info.Mode().Perm())
		}
	}

	message, err = downloadSSHConfig(home, client)
	if err != nil {
		t.Fatal(err)
	}
	if message != "ssh_config is already up to date" {
		t.Fatalf("message = %q", message)
	}
	backups, err := filepath.Glob(path + ".pdo-backup-*")
	if err != nil || len(backups) != 0 {
		t.Fatalf("backups = %v, error = %v", backups, err)
	}
}

func TestDownloadBacksUpAndReplaces(t *testing.T) {
	server := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeRemoteFile(t, w, []byte("new\n"), "remote-sha")
	})
	client := testClient(t, server)
	home := t.TempDir()
	path := filepath.Join(home, ".ssh", "config")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	message, err := downloadSSHConfig(home, client)
	if err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, path, []byte("new\n"))
	backups, err := filepath.Glob(path + ".pdo-backup-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups = %v, error = %v", backups, err)
	}
	assertFileContent(t, backups[0], []byte("old\n"))
	if !strings.Contains(message, filepath.Base(backups[0])) {
		t.Fatalf("message = %q", message)
	}
}

func TestDownloadFollowsSymlink(t *testing.T) {
	server := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeRemoteFile(t, w, []byte("new\n"), "remote-sha")
	})
	client := testClient(t, server)
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	dotfilesDir := filepath.Join(home, "dotfiles")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dotfilesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dotfilesDir, "config")
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	firstLink := filepath.Join(sshDir, "config-link")
	if err := os.Symlink(filepath.Join("..", "dotfiles", "config"), firstLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	configLink := filepath.Join(sshDir, "config")
	if err := os.Symlink("config-link", configLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := downloadSSHConfig(home, client); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, target, []byte("new\n"))
	if info, err := os.Lstat(configLink); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config symlink was replaced: info=%v error=%v", info, err)
	}
	backups, err := filepath.Glob(target + ".pdo-backup-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("target backups = %v, error = %v", backups, err)
	}
	assertFileContent(t, backups[0], []byte("old\n"))
}

func TestDownloadRejectsDanglingSymlinkAndNonRegularFile(t *testing.T) {
	server := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeRemoteFile(t, w, []byte("remote\n"), "remote-sha")
	})
	client := testClient(t, server)

	t.Run("dangling symlink", func(t *testing.T) {
		home := t.TempDir()
		sshDir := filepath.Join(home, ".ssh")
		if err := os.MkdirAll(sshDir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(sshDir, "config")
		if err := os.Symlink("missing", path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := downloadSSHConfig(home, client); err == nil || !strings.Contains(err.Error(), "resolve") {
			t.Fatalf("error = %v", err)
		}
		if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("dangling symlink changed: info=%v error=%v", info, err)
		}
	})

	t.Run("directory", func(t *testing.T) {
		home := t.TempDir()
		path := filepath.Join(home, ".ssh", "config")
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := downloadSSHConfig(home, client); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestDownloadAPIFailureLeavesLocalUntouched(t *testing.T) {
	server := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"bad credentials"}`)
	})
	client := testClient(t, server)
	home := t.TempDir()
	path := filepath.Join(home, ".ssh", "config")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := downloadSSHConfig(home, client); err == nil {
		t.Fatal("expected download error")
	}
	assertFileContent(t, path, []byte("local\n"))
	backups, _ := filepath.Glob(path + ".pdo-backup-*")
	if len(backups) != 0 {
		t.Fatalf("unexpected backups: %v", backups)
	}
}

func TestUploadCreatesAndUpdates(t *testing.T) {
	tests := []struct {
		name       string
		remote     []byte
		remoteSHA  string
		getStatus  int
		wantPutSHA string
	}{
		{name: "create", getStatus: http.StatusNotFound},
		{name: "update", getStatus: http.StatusOK, remote: []byte("remote\n"), remoteSHA: "old-sha", wantPutSHA: "old-sha"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			putCalls := 0
			server := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					if test.getStatus == http.StatusNotFound {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					writeRemoteFile(t, w, test.remote, test.remoteSHA)
				case http.MethodPut:
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
					if payload.Message != "pdo: upload ssh_config" || payload.SHA != test.wantPutSHA || payload.Branch != "main" {
						t.Fatalf("payload = %+v", payload)
					}
					decoded, err := base64.StdEncoding.DecodeString(payload.Content)
					if err != nil || string(decoded) != "local\n" {
						t.Fatalf("content = %q, error = %v", decoded, err)
					}
					w.WriteHeader(http.StatusCreated)
				default:
					t.Fatalf("unexpected method %s", r.Method)
				}
			})
			client := testClient(t, server)
			home := t.TempDir()
			path := filepath.Join(home, ".ssh", "config")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("local\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := uploadSSHConfig(home, client); err != nil {
				t.Fatal(err)
			}
			if putCalls != 1 {
				t.Fatalf("PUT calls = %d", putCalls)
			}
		})
	}
}

func TestUploadNoOpAndConflict(t *testing.T) {
	t.Run("no-op", func(t *testing.T) {
		putCalls := 0
		server := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut {
				putCalls++
			}
			writeRemoteFile(t, w, []byte("same\n"), "sha")
		})
		client := testClient(t, server)
		home := writeLocalSSHConfig(t, []byte("same\n"))
		message, err := uploadSSHConfig(home, client)
		if err != nil || message != "ssh_config is already up to date" || putCalls != 0 {
			t.Fatalf("message=%q error=%v PUT calls=%d", message, err, putCalls)
		}
	})

	t.Run("conflict", func(t *testing.T) {
		putCalls := 0
		server := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				writeRemoteFile(t, w, []byte("remote\n"), "sha")
				return
			}
			putCalls++
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"message":"conflict"}`)
		})
		client := testClient(t, server)
		home := writeLocalSSHConfig(t, []byte("local\n"))
		if _, err := uploadSSHConfig(home, client); err == nil || !strings.Contains(err.Error(), "changed during upload") {
			t.Fatalf("error = %v", err)
		}
		if putCalls != 1 {
			t.Fatalf("PUT calls = %d", putCalls)
		}
	})
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
	client := testClient(t, server)
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "actual-config")
	if err := os.WriteFile(target, []byte("linked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(sshDir, "config")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := uploadSSHConfig(home, client); err != nil {
		t.Fatal(err)
	}
	if string(uploaded) != "linked\n" {
		t.Fatalf("uploaded = %q", uploaded)
	}
}

func TestGitHubErrorsRedactToken(t *testing.T) {
	server := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"token-for-test rejected"}`)
	})
	client := testClient(t, server)
	_, _, _, err := client.get()
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

func testClient(t *testing.T, server *httptest.Server) *githubClient {
	t.Helper()
	previous := githubAPIBase
	githubAPIBase = server.URL
	t.Cleanup(func() { githubAPIBase = previous })
	return &githubClient{
		http:       server.Client(),
		token:      "token-for-test",
		repository: "owner/repo",
		branch:     "main",
		path:       "ssh/config",
	}
}

func assertGitHubGet(t *testing.T, request *http.Request) {
	t.Helper()
	if request.Method != http.MethodGet || request.URL.Path != "/repos/owner/repo/contents/ssh/config" || request.URL.Query().Get("ref") != "main" {
		t.Fatalf("request = %s %s", request.Method, request.URL.String())
	}
	if request.Header.Get("Authorization") != "Bearer token-for-test" {
		t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
	}
	if request.Header.Get("X-GitHub-Api-Version") != "2026-03-10" {
		t.Fatalf("API version = %q", request.Header.Get("X-GitHub-Api-Version"))
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

func writeLocalSSHConfig(t *testing.T, content []byte) string {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, ".ssh", "config")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}
