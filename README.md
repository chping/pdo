# pdo

跨平台个人命令行工具集。v0.1.1 支持同步 dotfiles，以及向 SSH config 中的主机批量部署公钥。

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

# 使用默认 ~/.ssh/config 部署指定公钥
pdo copy-ssh-id --identity ~/.ssh/id_ed25519

# 使用指定 SSH config；也支持 --identity=<path> 和 --config=<path>
pdo copy-ssh-id --identity ~/.ssh/id_ed25519.pub --config ./ssh_config

pdo version
pdo --version
pdo update --check
pdo update

pdo --help
```

批量同步按配置名称排序执行；指定多个选择器时按参数顺序执行。某项失败不会阻止后续项目，最终只要有一项失败，命令就返回退出码 `1`。

`download` 在内容变化时先在实际目标旁创建 `<文件名>.pdo-backup-<UTC时间戳>`，再替换文件。Unix 上保留已有文件权限，新文件为 `0600`。符号链接会被保留并更新最终目标；悬空、循环链接及非普通文件会被拒绝。

`upload` 以本地内容为准，每个文件产生一个 GitHub commit，并通过当前 blob SHA 防止覆盖并发修改。内容相同时两个方向均不会写文件或提交。

仅同步普通文件的原始字节；不支持目录、glob、删除、合并或跨文件事务。

### 部署 SSH 公钥

`copy-ssh-id` 要求 `--identity`，`--config` 默认使用 `~/.ssh/config`。路径可以是绝对路径、`~/...` 或相对当前目录；identity 可以指向私钥基路径或 `.pub` 文件。pdo 会在开始连接前确认 config、私钥及对应公钥均为可读普通文件。

pdo 按配置顺序处理字面 `Host` 别名，忽略通配符和否定模式并去重。根配置以及每个 Include 文件全局区域中的 `Include` 会递归展开；支持 `~/`、相对 `~/.ssh` 的路径和 glob，但不支持 Include 中的环境变量或 OpenSSH token。条件块中的 Include 不用于枚举 Host。

开始复制前，pdo 会通过 `ssh -G` 验证所有 Host 的 OpenSSH 配置；验证全部通过后依次执行 `ssh-copy-id -i <identity> -F <config> <host>`。`ssh-copy-id` 自行判断公钥是否已经安装，其密码、私钥口令和 host-key 确认直接使用当前终端。某个 Host 失败不会阻止后续 Host，最终有任一失败时退出 `1`。

所有平台都要求 `ssh` 和 `ssh-copy-id` 可从 `PATH` 找到。Windows OpenSSH 通常不附带 `ssh-copy-id`，需要用户自行安装兼容实现；缺少工具时该命令会在连接前失败，其他 pdo 命令不受影响。

## 更新

`pdo update --check` 只检查 GitHub 上的最新稳定 Release，不写入本地文件。`pdo update` 会校验同一 Release 中的 `SHA256SUMS`、验证候选二进制版本，然后替换当前实际执行文件。

更新时会先锁定 `~/.config/pdo/.update-transaction`，备份需要迁移的 pdo 配置或数据以及旧二进制。迁移或切换失败会恢复已修改内容；恢复失败时会保留事务目录并输出路径。成功后会删除配置和数据备份。pdo 不扫描目录、不执行远程迁移脚本，也不会修改配置中列出的 dotfiles。

普通 `go build` 生成的开发版本显示为 `pdo devel`，并拒绝执行更新。v0.1.0 是首个自更新基线，可使用 `pdo update` 升级到后续稳定版本。

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

推送 `v*` tag 会注入 tag 作为版本号，并在测试通过后发布 macOS、Linux、Windows 的 `amd64` / `arm64` 二进制、安装与卸载脚本及 `SHA256SUMS`。v0.1.0 及后续 Release tag 均不可移动或替换资产。
