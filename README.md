# cpdo

个人命令行工具集。v1 支持在本机 `~/.ssh/config` 与 GitHub 私有仓库之间同步原始文件内容。

## 安装

macOS / Linux：

```sh
curl -fsSL https://github.com/chping/cpdo/releases/latest/download/install.sh | sh
```

Windows PowerShell：

```powershell
irm https://github.com/chping/cpdo/releases/latest/download/install.ps1 | iex
```

安装器会识别 `amd64` / `arm64`、验证 SHA-256，并在配置不存在时创建模板。它不会修改 PATH 或覆盖已有配置。

- macOS / Linux：`~/.local/bin/cpdo`
- Windows：`%LOCALAPPDATA%\Programs\cpdo\cpdo.exe`
- 所有平台配置：`~/.config/cpdo/config.json`

## 配置

```json
{
  "schema_version": 1,
  "github": {
    "pat_env": "CPDO_GITHUB_PAT"
  },
  "ssh_config": {
    "repository": "OWNER/REPOSITORY",
    "branch": "main",
    "path": "ssh_config"
  }
}
```

创建一个仅能访问目标私有仓库的 GitHub fine-grained personal access token，并赋予 Contents 读写权限。token 只从配置指定的环境变量读取，不写入配置文件：

```sh
export CPDO_GITHUB_PAT="your-token"
```

```powershell
$env:CPDO_GITHUB_PAT = "your-token"
```

配置按功能分区。未来新增功能时添加新的顶层区块；未识别区块会被旧版本忽略。只有破坏性配置变更才增加 `schema_version`。

## 使用

```sh
cpdo download ssh-config
cpdo upload ssh-config
cpdo --help
```

`download` 在内容变化时先于实际文件旁创建 `config.cpdo-backup-<UTC时间戳>`，再替换文件。若 `~/.ssh/config` 是符号链接，会保留链接并更新最终目标；悬空链接、循环链接及非普通文件会被拒绝。

`upload` 以本地内容为准，通过 GitHub blob SHA 防止请求期间覆盖并发修改。内容相同时两个命令都不会写文件或提交。

## 卸载

macOS / Linux：

```sh
curl -fsSL https://github.com/chping/cpdo/releases/latest/download/uninstall.sh | sh
```

Windows PowerShell：

```powershell
irm https://github.com/chping/cpdo/releases/latest/download/uninstall.ps1 | iex
```

卸载器只删除 `cpdo` 可执行文件，保留 `~/.config/cpdo/config.json`。

## 开发

需要 Go 1.27：

```sh
go test ./...
go build .
```

推送 `v*` tag 会在测试通过后发布 macOS、Linux、Windows 的 `amd64` / `arm64` 二进制、安装与卸载脚本及 `SHA256SUMS`。
