# Graph Report - pdo  (2026-09-12)

## Corpus Check
- Corpus is ~11,471 words - fits in a single context window. You may not need a graph.

## Summary
- 156 nodes · 396 edges · 18 communities (15 shown, 3 thin omitted)
- Extraction: 90% EXTRACTED · 10% INFERRED · 0% AMBIGUOUS · INFERRED: 38 edges (avg confidence: 0.83)
- Token cost: 0 input · 0 output

## Community Hubs (Navigation)
- CI and Releases
- Test Helpers
- HTTP Test Fixtures
- Parsing and Migration
- Migration Rollback
- GitHub File Transfer
- Release Verification
- Updater Network Flow
- SSH Config Tests
- Atomic File Writes
- Configuration Loading
- Command Dispatch
- Update Tests
- Unix Installer
- Unix Uninstaller
- Repository Root

## God Nodes (most connected - your core abstractions)
1. `migrationTransaction` - 12 edges
2. `githubServer()` - 12 edges
3. `testClient()` - 11 edges
4. `run()` - 10 edges
5. `assertFileContent()` - 10 edges
6. `githubClient` - 9 edges
7. `downloadDotfile()` - 9 edges
8. `migrateJSONFile()` - 9 edges
9. `TestRunAllContinuesAndSorts()` - 9 edges
10. `copySSHID()` - 8 edges

## Surprising Connections (you probably didn't know these)
- `Development Test and Build Verification` --semantically_similar_to--> `Go Test and Build Verification`  [INFERRED] [semantically similar]
  README.md → .github/workflows/ci.yml
- `TestCopySSHIDArgumentParsing()` --calls--> `parseCopySSHIDArgs()`  [INFERRED]
  main_test.go → main.go
- `TestCopySSHIDRequiresToolsAndLiteralHosts()` --calls--> `copySSHID()`  [INFERRED]
  main_test.go → main.go
- `TestResolveCommandPath()` --calls--> `resolveCommandPath()`  [INFERRED]
  main_test.go → main.go
- `TestLoadConfigSchemaAndUnknownSection()` --calls--> `loadConfig()`  [INFERRED]
  main_test.go → main.go

## Import Cycles
- None detected.

## Hyperedges (group relationships)
- **Versioned Release Delivery** — github_workflows_ci_version_tag_trigger, github_workflows_ci_release_job, github_workflows_ci_release_asset_build, github_workflows_ci_sha256sums, github_workflows_ci_release_publication, readme_release_pipeline, readme_installation, readme_uninstallation [INFERRED 0.95]
- **Safe File Mutation Mechanisms** — readme_backup_before_replace, readme_blob_sha_concurrency_guard, readme_update_transaction [INFERRED 0.85]

## Communities (18 total, 3 thin omitted)

### Community 0 - "CI and Releases"
Cohesion: 0.11
Nodes (29): CI Workflow, Cross-Platform Test Matrix, Go Test and Build Verification, Cross-Platform Release Asset Build, Release Job, GitHub Release Publication, Installer Script Syntax Validation, SHA256SUMS Release Artifact (+21 more)

### Community 1 - "Test Helpers"
Cohesion: 0.25
Nodes (17): testing.T, assertFileContent(), assertMode(), TestConfigValidation(), TestCopySSHIDArgumentParsing(), TestDownloadAPIFailureLeavesLocalUntouched(), TestDownloadCreatesNoOpsAndPreservesMode(), TestLoadConfigSchemaAndUnknownSection() (+9 more)

### Community 2 - "HTTP Test Fixtures"
Cohesion: 0.25
Nodes (15): net/http.HandlerFunc, net/http/httptest.Server, net/http.ResponseWriter, githubServer(), remoteURL(), setGitHubServer(), testClient(), TestDownloadFollowsSymlinkAndRejectsInvalidTargets() (+7 more)

### Community 3 - "Parsing and Migration"
Cohesion: 0.23
Nodes (12): invalidPathPart(), openTransaction(), parseRemote(), parseSSHDirective(), resolveCommandPath(), resolveInclude(), runInternalMigrations(), runOwnedMigrations() (+4 more)

### Community 4 - "Migration Rollback"
Cohesion: 0.29
Nodes (9): atomicWriteFile(), finishFailedTransaction(), migrateJSONFile(), restoreExecutable(), rollbackUpdate(), startTransaction(), TestMigrationRestoresMissingFileAndRequiresContinuousChain(), jsonMigration (+1 more)

### Community 5 - "GitHub File Transfer"
Cohesion: 0.35
Nodes (7): io.Reader, net/http.Request, downloadDotfile(), inspectLocalFile(), readAPIResponse(), uploadDotfile(), githubClient

### Community 6 - "Release Verification"
Cohesion: 0.25
Nodes (8): os.FileInfo, checksumFor(), inspectExecutable(), main(), releaseHasAsset(), verifyExecutableVersion(), release, releaseAsset

### Community 7 - "Updater Network Flow"
Cohesion: 0.43
Nodes (5): net/http.Client, compareVersions(), parseVersion(), semanticVersion, updater

### Community 8 - "SSH Config Tests"
Cohesion: 0.36
Nodes (8): parseSSHHosts(), setHome(), TestCopySSHIDRequiresToolsAndLiteralHosts(), TestParseSSHHostsIncludesAndPatterns(), TestParseSSHHostsRejectsInvalidConfig(), TestRunCopySSHIDPreflightFailurePreventsCopies(), TestRunCopySSHIDPreflightsThenContinues(), writeTestFile()

### Community 9 - "Atomic File Writes"
Cohesion: 0.48
Nodes (7): os.FileMode, cleanupOldExecutable(), replaceExecutable(), replaceLocalFile(), copyFile(), TestReplaceRunningExecutable(), writeExclusive()

### Community 10 - "Configuration Loading"
Cohesion: 0.29
Nodes (6): expandLocalPath(), loadConfig(), config, dotfileConfig, githubConfig, preparedDotfile

### Community 11 - "Command Dispatch"
Cohesion: 0.47
Nodes (6): io.Writer, copySSHID(), parseCopySSHIDArgs(), run(), validateReadableFile(), copySSHIDOptions

### Community 12 - "Update Tests"
Cohesion: 0.50
Nodes (5): buildCandidate(), TestUpdateErrors(), TestUpdateInstallsVerifiedCandidate(), TestUpdateRejectsBadHashAndCandidateVersion(), updateAssetName()

## Knowledge Gaps
- **6 isolated node(s):** `github.com/chping/pdo`, `install.sh script`, `githubFile`, `uninstall.sh script`, `Cross-Platform Test Matrix` (+1 more)
  These have ≤1 connection - possible missing edges or undocumented components.
- **3 thin communities (<3 nodes) omitted from report** — run `graphify query` to explore isolated nodes.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `githubClient` connect `GitHub File Transfer` to `HTTP Test Fixtures`, `Parsing and Migration`, `Updater Network Flow`?**
  _High betweenness centrality (0.049) - this node is a cross-community bridge._
- **Why does `downloadDotfile()` connect `GitHub File Transfer` to `Test Helpers`, `HTTP Test Fixtures`, `Parsing and Migration`, `Atomic File Writes`, `Command Dispatch`?**
  _High betweenness centrality (0.032) - this node is a cross-community bridge._
- **Why does `testClient()` connect `HTTP Test Fixtures` to `Test Helpers`, `GitHub File Transfer`?**
  _High betweenness centrality (0.026) - this node is a cross-community bridge._
- **What connects `github.com/chping/pdo`, `install.sh script`, `githubFile` to the rest of the system?**
  _6 weakly-connected nodes found - possible documentation gaps or missing edges._
- **Should `CI and Releases` be split into smaller, more focused modules?**
  _Cohesion score 0.11083743842364532 - nodes in this community are weakly interconnected._