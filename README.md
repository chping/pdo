# pdo

跨平台个人命令行工具集，支持同步 dotfiles、跨设备剪贴板和文件中转，以及向 SSH config 中的主机批量部署公钥。

## 安装

macOS / Linux：

```sh
curl -fsSL https://github.com/chping/pdo/releases/latest/download/install.sh | sh
```

OpenWrt（BusyBox `wget`）：

```sh
wget -qO- https://github.com/chping/pdo/releases/latest/download/install.sh | sh
```

Windows PowerShell：

```powershell
irm https://github.com/chping/pdo/releases/latest/download/install.ps1 | iex
```

安装器会识别 `amd64` / `arm64` 并验证 SHA-256。下载器、SHA-256 工具和 `awk` 是安装器硬依赖；Linux 缺少时会显示安装命令并在交互终端询问是否安装，拒绝、失败或非交互执行会停止安装。Linux 支持 `apt-get`、`dnf`、`yum`、`pacman`、`apk`、`zypper`，OpenWrt 支持 `opkg` 和新版 `apk`，非 root 用户使用 `sudo`。

OpenSSH 和系统剪贴板组件是功能级可选依赖。缺失时安装器会说明受影响功能和安装命令，但仍完成 pdo 安装；OpenWrt 默认 Dropbear 可以保留，只有使用 `copy-ssh-id` 时才需要 OpenSSH Client。macOS 或 Windows 缺少系统剪贴板组件时应修复系统组件，安装器不会安装第三方替代品。

安装器硬依赖满足后，安装器会询问是否立即运行 `pdo setup`。选择否（或没有交互终端）时，仅在配置不存在时创建模板。Windows 安装器会将安装目录加入用户 PATH（当前 PowerShell 立即可用）；macOS / Linux / OpenWrt 不修改 PATH，也不会覆盖已有配置。

- macOS / Linux / OpenWrt：`~/.local/bin/pdo`
- Windows：`%LOCALAPPDATA%\Programs\pdo\pdo.exe`
- 所有平台配置：`~/.config/pdo/config.json`
- 私有凭据：`~/.config/pdo/.env`（仅 `pdo` 自动加载）

## 配置

推荐在终端运行 `pdo setup`。它会隐藏输入 GitHub PAT 和剪贴板密码，引导填写 SSH config 的 GitHub 文件链接及可选剪贴板服务，并将凭据写入 `~/.config/pdo/.env`（Unix 上权限为 `0600`）。该文件使用普通 `KEY=value` 格式，`pdo` 会自动加载；不会修改启动它的 shell 配置，且当前 shell 中已设置的同名环境变量优先。

也可以手工编辑配置：

```json
{
  "schema_version": 1,
  "cloud_clipboard": {
    "host": "https://clipboard.example.com/",
    "room": "personal",
    "password_env": "PDO_CLOUD_CLIPBOARD_PASSWORD"
  },
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

### 配置跨设备 Copy & Paste

该功能使用 [cloud-clipboard-go](https://github.com/Jonnyan404/cloud-clipboard-go) v5.0.6 或更高版本提供的 HTTP API。pdo 只把服务作为中转接口，不依赖网页客户端，也不直接访问其存储。

1. 按 [cloud-clipboard-go 配置文档](https://github.com/Jonnyan404/cloud-clipboard-go/blob/main/cloud-clip/config.md) 部署服务，并至少设置：

   ```env
   AUTH_PASSWORD=your-password
   TEXT_LIMIT=67108864
   FILE_LIMIT=67108864
   ```

   `MESSAGE_NUM` 控制历史记录条数，`FILE_EXPIRE` 控制文件保留时间，由服务部署方自行设置。pdo 始终只读取对应房间的最新一条记录。

2. 通过 HTTPS 暴露 cloud-clipboard-go，不要直接公开未加密的 HTTP 端口。Nginx 可在现有 TLS `server` 中使用：

   ```nginx
   location / {
       proxy_pass http://127.0.0.1:9501;
       client_max_body_size 128m;
       proxy_read_timeout 600s;
       proxy_send_timeout 600s;
   }
   ```

3. 在每台设备的 `~/.config/pdo/config.json` 中填写相同的 `cloud_clipboard` 段：

   - `host`：cloud-clipboard-go 的 HTTPS 服务基址，例如 `https://clipboard.example.com/`。如果服务设置了 `PREFIX=/clipboard`，则填写 `https://clipboard.example.com/clipboard/`。不能包含凭据、query 或 fragment。
   - `room`：设备间共享的房间前缀。pdo 自动使用 `<room>-pdo-clipboard` 和 `<room>-pdo-file` 两个独立房间。
   - `password_env`：保存密码的环境变量名，不是密码本身。

4. 在每台设备上设置与服务端 `AUTH_PASSWORD` 相同的密码。`pdo setup` 会将它保存到私有 env 文件；也可以手工设置环境变量（环境变量优先）：

   ```sh
   export PDO_CLOUD_CLIPBOARD_PASSWORD="your-password"
   ```

   ```powershell
   $env:PDO_CLOUD_CLIPBOARD_PASSWORD = "your-password"
   ```

`cloud_clipboard` 段是可选的，只在调用四个跨设备命令时校验。反向代理必须允许至少 64 MiB 请求体，读写超时应不少于 600 秒。

连接、TLS 握手或等待服务响应头超过 10 秒时，命令会返回错误；文件传输仍允许最长 10 分钟。缺少密码时会直接提示运行 `pdo setup`。

内容由自管的 cloud-clipboard-go 保存，不提供端到端加密。

dotfile 名称使用 kebab-case，并对应命令行的 `--<名称>`。`remote` 使用 GitHub 文件页面的标准链接 `https://github.com/OWNER/REPOSITORY/blob/BRANCH/PATH`；`local` 必须以 `~/` 开头或使用当前平台的绝对路径。pdo 会在内部将文件链接转换为 GitHub Contents API 请求。

创建一个仅能访问目标私有仓库的 GitHub fine-grained personal access token，并赋予 Contents 读写权限。`pdo setup` 会保存它到私有 env 文件；也可以手工设置配置指定的环境变量（环境变量优先）：

```sh
export PDO_GITHUB_PAT="your-token"
```

```powershell
$env:PDO_GITHUB_PAT = "your-token"
```

配置的本地路径会被视为明确的上传授权。不要配置 SSH 私钥、token 或其他秘密文件。

## 使用

```sh
# 交互式写入 GitHub、SSH config 和可选剪贴板配置
pdo setup

# 同步配置中的全部 dotfiles
pdo download dotfiles
pdo upload dotfiles

# 同步指定的一项或多项
pdo download dotfiles --ssh-config
pdo upload dotfiles --ssh-config --git-config

# 设备 A：上传参数文本、管道文本，或当前系统剪贴板中的图片/文本
pdo copy "text"
printf 'text from pipe\n' | pdo copy
pdo copy

# 设备 B：下载最近的文本/图片；系统剪贴板不可用时仍在终端输出
pdo paste

# 设备 A 上传文件；设备 B 下载到当前目录或指定的已存在目录
pdo copy-file ./archive.zip
pdo paste-file
pdo paste-file ./downloads

# 使用默认 ~/.ssh/config 部署指定公钥
pdo copy-ssh-id --identity ~/.ssh/id_ed25519

# 使用指定 SSH config；也支持 --identity=<path> 和 --config=<path>
pdo copy-ssh-id --identity ~/.ssh/id_ed25519.pub --config ./ssh_config

# 仅部署到 SSH config 中的指定 Host
pdo copy-ssh-id --identity ~/.ssh/id_ed25519 --target-host=openwrt

pdo version
pdo --version
pdo update --check
pdo update

pdo --help
```

文本/图片与文件分别使用 `<room>-pdo-clipboard` 和 `<room>-pdo-file`，单项最大 64 MiB。历史条数和文件过期时间遵循 cloud-clipboard-go 的 `MESSAGE_NUM`、`FILE_EXPIRE` 配置；pdo 不扫描历史。`copy-file` 和 `paste-file` 仅在 stderr 连接终端时显示传输进度；下载遇到同名文件会使用 `name (1).ext` 等新名称，不覆盖已有文件。

macOS 使用系统 AppKit 剪贴板，Windows 使用 PowerShell/.NET。Linux Wayland 需要 `wl-clipboard`（`sudo apt install wl-clipboard`、`sudo dnf install wl-clipboard` 或 `sudo pacman -S wl-clipboard`），X11 需要 `xclip`（`sudo apt install xclip`、`sudo dnf install xclip` 或 `sudo pacman -S xclip`）。没有 Wayland/X11 图形会话或剪贴板写入失败时，`pdo paste` 会提示原因并仍将文本或图片摘要输出到终端；剪贴板同时含图片和文本时优先使用图片。

只有无参数且没有管道输入的 `pdo copy` 会检查剪贴板读取依赖，并在交互终端询问是否安装；带参数或管道的 `copy` 不检查本地剪贴板。`paste` 不弹出安装确认，剪贴板不可用时保持成功并降级输出终端。

`pdo copy` 带文本参数时优先使用参数；无参数且标准输入来自管道或文件重定向时，将输入原样作为 UTF-8 文本上传；交互终端中无参数时读取系统剪贴板。

批量同步按配置名称排序执行；指定多个选择器时按参数顺序执行。某项失败不会阻止后续项目，最终只要有一项失败，命令就返回退出码 `1`。

`download` 在内容变化时先在实际目标旁创建 `<文件名>.pdo-backup-<UTC时间戳>`，再替换文件。Unix 上保留已有文件权限，新文件为 `0600`。符号链接会被保留并更新最终目标；悬空、循环链接及非普通文件会被拒绝。

`upload` 以本地内容为准，每个文件产生一个 GitHub commit，并通过当前 blob SHA 防止覆盖并发修改。内容相同时两个方向均不会写文件或提交。

仅同步普通文件的原始字节；不支持目录、glob、删除、合并或跨文件事务。

### 部署 SSH 公钥

`copy-ssh-id` 要求 `--identity`，`--config` 默认使用 `~/.ssh/config`。路径可以是绝对路径、`~/...` 或相对当前目录；identity 可以指向私钥基路径或 `.pub` 文件。可选的 `--target-host` 不区分大小写匹配一个字面 `Host` 别名；不指定时处理全部 Host，目标不存在时不会建立连接。pdo 会在开始连接前确认 config、私钥及对应公钥均为可读普通文件。

pdo 按配置顺序处理字面 `Host` 别名，忽略通配符和否定模式并去重。根配置以及每个 Include 文件全局区域中的 `Include` 会递归展开；支持 `~/`、相对 `~/.ssh` 的路径和 glob，但不支持 Include 中的环境变量或 OpenSSH token。条件块中的 Include 不用于枚举 Host。

开始复制前，pdo 会通过 `ssh -G` 验证所有 Host 的 OpenSSH 配置；验证全部通过后，Unix 在 `ssh-copy-id` 可用时优先调用它，否则和 Windows 一样通过 `ssh` 将公钥写入远端 `~/.ssh/authorized_keys`。内置写入会按公钥主体查重，即使注释不同也不会重复写入。密码、私钥口令和 host-key 确认直接使用当前终端。某个 Host 失败不会阻止后续 Host，最终有任一失败时退出 `1`。

OpenSSH Client 只在执行 `copy-ssh-id` 时需要。缺失时，交互终端会询问是否安装并在成功后继续原命令；无法安装或提权只会让本次 `copy-ssh-id` 失败。`ssh-copy-id` 是可选项。

## 更新

pdo 每 24 小时在命令启动时检查一次最新稳定 Release，并使用 `~/.config/pdo/.update-check` 记录检查时间。发现新版本时，交互终端可以输入 `y` 或 `yes` 立即升级，也可以直接回车忽略并继续执行当前命令；管道或重定向场景只提示运行 `pdo update`，不会读取标准输入。升级完成后请重新执行原命令。

`pdo update --check` 只检查 GitHub 上的最新稳定 Release，不写入本地文件。`pdo update` 会校验同一 Release 中的 `SHA256SUMS`、验证候选二进制版本，然后更新二进制并迁移配置和必要数据。缺少功能级可选依赖时只显示警告，不阻止更新；校验、写入或迁移失败仍会保留旧版本。

普通 `go build` 生成的开发版本显示为 `pdo devel`，并拒绝执行更新。

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

CI 在 macOS、Linux、Windows 上运行测试，并使用官方 OpenWrt stable rootfs 验证 BusyBox `wget` 安装及启动流程。推送 `v*` tag 会注入 tag 作为版本号，并在完整兼容性测试通过后发布 macOS、Linux、Windows 的 `amd64` / `arm64` 二进制、安装与卸载脚本及 `SHA256SUMS`。
