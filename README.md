# pdo

跨平台个人命令行工具集。v0.1.0 支持在本地与 GitHub 私有仓库之间同步 dotfiles。

## 安装

macOS / Linux：

```sh
curl -fsSL https://github.com/chping/pdo/releases/latest/download/install.sh | sh
```

Windows PowerShell：

```powershell
irm https://github.com/chping/pdo/releases/latest/download/install.ps1 | iex
```

安装器会识别 `amd64` / `arm64`、验证 SHA-256，并仅在配置不存在时创建模板。它不会修改 PATH 或覆盖已有配置。

- macOS / Linux：`~/.local/bin/pdo`
- Windows：`%LOCALAPPDATA%\Programs\pdo\pdo.exe`
- 所有平台配置：`~/.config/pdo/config.json`

## 配置

```json
{
  "schema_version": 1,
  "github": {
    "pat_env": "PDO_GITHUB_PAT"
  },
  "dotfiles": {
    "ssh-config": {
      "remote": "https://github.com/OWNER/REPOSITORY/blob/main/ssh_config",
      "local": "~/.ssh/config"
    },
    "git-config": {
      "remote": "https://github.com/OWNER/REPOSITORY/blob/main/git_config",
      "local": "~/.gitconfig"
    }
  }
}
```

dotfile 名称使用 kebab-case，并对应命令行的 `--<名称>`。`remote` 使用 GitHub 文件页面的标准链接 `https://github.com/OWNER/REPOSITORY/blob/BRANCH/PATH`；`local` 必须以 `~/` 开头或使用当前平台的绝对路径。pdo 会在内部将文件链接转换为 GitHub Contents API 请求。

创建一个仅能访问目标私有仓库的 GitHub fine-grained personal access token，并赋予 Contents 读写权限。token 只从配置指定的环境变量读取：

```sh
export PDO_GITHUB_PAT="your-token"
```

```powershell
$env:PDO_GITHUB_PAT = "your-token"
```

配置的本地路径会被视为明确的上传授权。不要配置 SSH 私钥、token 或其他秘密文件。

## 使用

```sh
# 同步配置中的全部 dotfiles
pdo download dotfiles
pdo upload dotfiles

# 同步指定的一项或多项
pdo download dotfiles --ssh-config
pdo upload dotfiles --ssh-config --git-config

pdo --help
```

批量同步按配置名称排序执行；指定多个选择器时按参数顺序执行。某项失败不会阻止后续项目，最终只要有一项失败，命令就返回退出码 `1`。

`download` 在内容变化时先在实际目标旁创建 `<文件名>.pdo-backup-<UTC时间戳>`，再替换文件。Unix 上保留已有文件权限，新文件为 `0600`。符号链接会被保留并更新最终目标；悬空、循环链接及非普通文件会被拒绝。

`upload` 以本地内容为准，每个文件产生一个 GitHub commit，并通过当前 blob SHA 防止覆盖并发修改。内容相同时两个方向均不会写文件或提交。

仅同步普通文件的原始字节；不支持目录、glob、删除、合并或跨文件事务。

## 卸载

macOS / Linux：

```sh
curl -fsSL https://github.com/chping/pdo/releases/latest/download/uninstall.sh | sh
```

Windows PowerShell：

```powershell
irm https://github.com/chping/pdo/releases/latest/download/uninstall.ps1 | iex
```

卸载器只删除 `pdo` 可执行文件，保留 `~/.config/pdo/config.json`。

## 开发

需要 Go 1.27：

```sh
go test ./...
go build .
```

推送 `v*` tag 会在测试通过后发布 macOS、Linux、Windows 的 `amd64` / `arm64` 二进制、安装与卸载脚本及 `SHA256SUMS`。
