# gpm 包格式与安装布局（契约 · 已冻结 · v3.15）

本文只记录**已经拍板**的接口。它是 `AI Desk` 的 CI 与 `gpm` 之间唯一的契约，
两个仓库各自独立演进时以本文为准。

## 1. 三项裁决

| # | 问题 | 裁决 |
|---|---|---|
| 1 | gpm 的家目录（安装根）怎么定 | `--dir` > **按清单里 `requires` 取的那个家**（`${COT_HOME:-$HOME/cot}` / `${TDP_HOME:-$HOME/tdp}`）> **从 gpm 自己的位置推断**（`<家目录>/bin/gpm` 自证家目录，见第 5 节）> **平台数据目录 + 简称**（`requires` 为空、又没有别的东西可依附时的落脚点：macOS `~/Library/Application Support/<简称>`、Windows `%LOCALAPPDATA%\<简称>`、Linux `${XDG_DATA_HOME:-~/.local/share}/<简称>`）> 当前目录。打包方用 **`gpm pack --default-dir`** 把默认值烘进 `install.sh` / `install.cmd`（AI Desk 烘的是 `~/cot`）。**装完之后**（`list` / `where` / `uninstall` / `env`）走的是**另一条链**（`home.Resolve`）：`--dir` > **从 gpm 自己的位置推断** > `$COT_HOME` > `$TDP_HOME` > 当前目录——「自己住哪个家」比环境变量更具体（用户敲的是 `<某个家>/bin/gpm`），而且这一档**不看平台数据目录**：那是安装那一次"没有工具链可依附"时的落脚点（[`DESIGN.md`](DESIGN.md) D21、§2.3）。**v3.5 起没有 `GPM_HOME`**：这个家同时是 cot / tdp 自己的家，gpm 的内部结构（`bin/`、`lib/`、账本、`staging/`）与工具链的东西住在同一个目录里。**v3.7 起账本按家命名**：`<家>/<家目录名>-state.json`（`~/cot/cot-state.json`），v3.6 及以前的 `state.json` 仍可读、写回时迁移（[`DESIGN.md`](DESIGN.md) D36）。**v3.8 起工具链已经装好就跳过**：`<家>/bin/cot`（`tdp` 同理）已经是文件时不再跑包里自带的那一份，`--force` 才重铺；跳过自举不影响 PATH 集成（[`DESIGN.md`](DESIGN.md) D37）。**v3.6 起 GUI 应用的入口也直接住在这个家的顶层**——`<家>/AI Desk.app`、`<家>/ai-desk.exe`——`lib/<id>_<版本>_<平台>/` 那套命名留给命令行插件（见第 5 节）。 |
| 2 | gpm 怎么到用户手上 | **分发包自带 gpm**：zip 里同时放 gpm 二进制与几行 bootstrap 脚本，用户解压后跑脚本即可，不需要预装任何东西。清单声明 `requires` 时还要带上 `tools/<os>_<arch>/` 里那家工具链（第 2、7 节）。 |
| 3 | gpm 的范围 | **GUI 应用的安装器 + 离线搬运 zip 自带的工具链**：只收录带图形界面的程序；一个 zip 装一个应用，**账本本身支持多包**。清单声明 `requires: [cot]` 时，install.sh 先跑 zip 里的 cot（第 7 节）；工具链的资产**不进账本、卸载不碰**（第 6 节）；**已经装好就不重铺**——`<家>/bin/cot` 在就跳过自举、`--force` 才照包里那份重铺（第 7 节、[`DESIGN.md`](DESIGN.md) D37）。不做纯 CLI 工具的抓取与安装、不做镜像表/版本源、**没有一行网络代码**——不抓取、不升级，对标 `dpkg` 而不是 `apt`（见 [`DESIGN.md`](DESIGN.md) §0.5、D32/D34）。 |

裁决 1 的直接好处：gpm 写下的每一处都记在账本里（第 6 节），**卸载就是照账逐条回放**。
**v3.5 之后"卸载 = 删掉一个目录"不再成立**——那个家里还住着 cot / tdp 自己的东西，
所以只能按账本删、不能整目录删（第 5 节末、[`DESIGN.md`](DESIGN.md) D34）。

裁决 1 的代价（明确接受）：**gpm 只有一个"自己挑"的落脚点**，而且只在没有工具链的家可依附时
才用它。用户在终端里直接敲 `gpm install .`、没有任何 `--dir` / `$COT_HOME` / `$TDP_HOME`、
gpm 二进制也不在某个 `<家目录>/bin/gpm` 的位置上时：清单声明了 `requires` 就装进那家的家，
没声明就装到**平台数据目录 + 简称**（上面第 4 档）——不会掉进当前目录。
想让位子更明确，还是由打包方用 `--default-dir` 说清楚：这正是
`install.sh` / `install.cmd` 存在的主要理由。

## 2. 分发包

一个平台一个归档，命名 `<id>-<version>-<os>-<arch>.zip`，例如
`ai-desk-1.0.0-darwin-arm64.zip`。

```
ai-desk-1.0.0-darwin-arm64.zip
├── install.sh          # macOS / Linux 的 bootstrap
├── install.cmd         # Windows 的 bootstrap（双击这条；策略禁止脚本时的退路）
├── install.ps1         # Windows 的 bootstrap（v3.15：PowerShell 5.1 / 7，终端与 GUI-Setup 用）
├── GUI-Setup.app       # 选填（v3.13）：随包的图形安装器，清单 setup: 段声明（第 3 节）
│                       # Windows / Linux 上是单个可执行文件 GUI-Setup.exe
├── gpm                 # 本平台的 gpm 二进制（Windows 为 gpm.exe）
├── ad-manifest.yaml    # 清单，文件名 = <简称>-manifest.yaml
├── tools/              # 选填：requires 非空时才有（第 3、7 节）
│   └── darwin_arm64/cot
├── payload/            # 载荷：根下只能有【一个】条目（v3.6）
│   └── AI Desk.app     # 它整份落到 <家>/AI Desk.app
└── SHA256SUMS          # 校验和
```

* 归档里只放本平台需要的入口形态，清单里仍可写全三个平台（见下）。
* 平台不匹配时 gpm 直接拒绝安装，不会装出一个跑不起来的目录。
* 这个 zip 由 **`gpm pack`** 产出
  （`gpm pack <装配目录> [--os OS] [--arch ARCH] [--gpm 可执行文件] [--default-dir PATH]`）。
  装配目录里只需要 `<简称>-manifest.yaml` + `payload/`（要自举工具链再加 `tools/`；**`payload/` 根下只能有入口那一个条目**，v3.6），其余几样是 `gpm pack` 现场生成的——
  所以**打出来的 `SHA256SUMS`、`install.sh`、`install.cmd`、`install.ps1` 在三个平台上是同一份实现**，
  不靠每个仓库各写一遍打包脚本（理由见 [`DESIGN.md`](DESIGN.md) §2.15）。`tools/` 也一并打进 zip 并纳入 `SHA256SUMS`——坏掉的 cot 必须在动手之前就被发现。
* `--default-dir` 只影响生成脚本里的那一个默认值（第 7 节）；留空时脚本**干脆不传 `--dir`**：
  清单有 `requires` 就交给 gpm 去解析那家的家，没有就落到平台数据目录 + 简称（第 1 节）。
* **`setup:` / `--setup`（v3.13）**：清单声明了随包的图形安装器就必须给 `--setup <路径>`，
  那份文件被放到 zip **顶层**（与 `install.sh` / `gpm` 并排），**不进 `payload/`、不进 `SHA256SUMS`**；
  它相对**包根**（只有 `entry:` 相对 `payload/`）。非 Windows 的 bundle 里，
  `Contents/Info.plist` 的 `CFBundleExecutable` 点名的那个文件缺 x 位就补 `0755`——**只补它一个**。
  七种错法（声明了没给 / 给了没声明 / 名字对不上 / bundle 给了文件 / exe 给了目录 /
  setup 缺当前平台 / 路径越出包根）都在打包时报错，且不留半包（[`DESIGN.md`](DESIGN.md) D43、FR-32）。

## 3. 清单：<简称>-manifest.yaml

```yaml
id: ai-desk            # 小写字母/数字/._-，目录名与账本主键
name: AI Desk          # 显示名，用来建 ~/Applications/AI Desk.app
version: 1.0.0
requires: [cot]        # 选填：要哪家工具链（cot / tdp），决定安装根与是否自举；缺省 []
entry:
  darwin:  { bundle: AI Desk.app }   # 相对于 payload/，macOS 用 .app
  linux:   { exe: ad }               # 相对于 payload/，裸可执行文件
  windows: { exe: ad.exe }
setup:                               # 选填（v3.13）：随包的图形安装器，相对于【包根】
  darwin:  { bundle: GUI-Setup.app }
  windows: { exe: GUI-Setup.exe }
launch:
  cmd: ad                          # 终端里敲的命令名
  mode: activate                   # 仅 macOS 的 bundle 有意义：activate | direct
```

规则：

* `entry.<goos>` **必须恰好**给 `bundle` 或 `exe` 之一。同时给或都不给都报错。
* `entry` 里的路径**相对于 `payload/`**；安装后它就在**家目录顶层**（v3.6），
  也就是说 `bundle: AI Desk.app` 会落到 `<家>/AI Desk.app`，
  账本里的 `dir` 就是 `<家>/<入口顶层名>`。
* `payload/` 根下**只能有入口那一个条目**（v3.6）：多一个就报错，一个字节都不落。
  入口名撞家骨架（`bin` / `lib` / `staging` / 账本（`<家目录名>-state.json`，旧名 `state.json` 也算）/ `log`）时即使 `--force` 也拒；
  家里已有别人的同名东西时报错、`--force` 才放行（[`DESIGN.md`](DESIGN.md) D35、FR-23）。
* 路径必须是相对路径，禁止绝对路径、盘符、`..`。
* `launch.cmd` 必须是合法的可执行文件名（不含 `/`、`\`、`..`），也**不能是 `gpm`**（那是 gpm 自己用的）。
* `launch.cmd` 在同一个家里撞名就**拒绝安装**（[`DESIGN.md`](DESIGN.md) FR-21）：
  账本里这个命令名已经属于别的包，或者 `<家>/bin/<cmd>` 已经存在、且不是 gpm 写的（没有
  `由 gpm 生成，请勿手工编辑。` 这一行）。只有 `--force` 才覆盖。
* `launch.mode`：
  * `activate`（默认）：Linux / Windows 上生成常规启动器（Windows 是 `<cmd>.cmd`）。
  * `direct`：同上。
  * **macOS：`requires` 非空时两种模式生成的东西一样**——先注入工具链环境（`COT_HOME` / `TDP_HOME`
    与前置的 `<家>/bin`，`<家>/bin/env-cot.vars` 存在时 source 它），再直接
    `exec "<app>/Contents/MacOS/<CFBundleExecutable>" "$@"`。原因：`open` 走 LaunchServices、
    **不给环境**（实测只有 15 项 launchd 基线，`open --env` 不生效），而"终端启动的应用
    看得见工具链"是 v3.5 的硬需求。代价是丢掉双击等价、单实例激活与 Dock 行为；
    Finder / 启动台双击这条路径拿不到注入（见 [`DESIGN.md`](DESIGN.md) D33、A7、R13）。
  * **`requires` 为空时不改道**：没有环境要注入，`activate` 照旧生成
    `exec open "<app>" --args "$@"`（保住双击语义），只有 `direct` 直接 exec 内层可执行文件。
* 上游直接发 `.app` 就用 `bundle`。**本版没有 `bin`**：上游只发裸可执行文件的
  macOS 应用，现在装不了（合成 `.app` 外壳的设计已定、代码未实现，见
  [`DESIGN.md`](DESIGN.md) §2.7 与 R8）。硬把裸 Mach-O 当 `exe` 收进来，
  用户拿到的是一个没有应用身份的东西——双击会被交给终端应用执行。
* **`setup`（v3.13，选填）**：随包的图形安装器。`setup.<goos>` 与 `entry.<goos>` 同形
  （`bundle` / `exe` 二选一），但**相对于包根**；写了就必须覆盖当前平台，否则这个包不适用于本机。
  打包时清单声明了就得用 `gpm pack --setup` 指出那份文件，且文件名与清单里写的逐字相同
  （第 2 节、[`DESIGN.md`](DESIGN.md) D43）。

## 4. SHA256SUMS

标准格式，一行一条：

```
<64 位小写 hex>  <相对路径>
```

* `payload/` 下的**每一个常规文件都必须出现在清单里**，gpm 逐条校验，
  有一条对不上就中止安装。符号链接不参与校验。`tools/` 里的文件同样要出现在清单里。
* 缺失 `SHA256SUMS` 时 gpm 打印警告并继续，账本把这次安装标成 `unverified`。
* `gpm install --skip-verify` 可以显式跳过校验（只用于调试）。
* 这份文件由 `gpm pack` 生成（按路径排序、每行 `<hash>` + 两个空格 + 路径），
  发布者不需要自己算——包括 `AI Desk` 的 CI。

## 5. 安装后的布局

```
<家>/                             # = $COT_HOME（默认 ~/cot）或 $TDP_HOME（默认 ~/tdp）；
                                  #   requires 为空时 = 平台数据目录/<简称>
├── bin/
│   ├── gpm                       # 自动拷进来，保证之后 list/uninstall 可用
│                                 #   家里已有一份时比版本（v3.11 起，D41）：这份更新才替换
│   ├── ad                        # 终端启动器（Windows 上是 ad.cmd）
│   ├── cot / tdp                 # 工具链自举的产物（gpm 不记账、卸载不碰）
│   └── activate* / env-cot*      # cot 的 activate 框架，同上
├── AI Desk.app                   # 入口本体：payload/ 根下那一个条目原样落在这里（v3.6、账本 dir）
│                                 #   Windows / Linux 上就是 ai-desk.exe / ai-desk，与 bin/ 平级
├── lib/                          # 命令行插件的命名空间（v3.6 起 gpm 不再往里写）
│   └── go_1.27.1_darwin_arm64/ … # cot 装的插件（gpm 不记账、卸载不碰）
├── staging/                      # 解包中转：每次安装一个 unpack-<纳秒>，装完连这一层一起收掉
│                                 #   （里面还有东西就留着：另一个 gpm 可能正在解包）
└── <家目录名>-state.json         # 账本：所有外部副作用的唯一真相（v3.7；v3.6 是 state.json）
```

**这个家不是 gpm 独占的**（v3.5）：`bin/` 与 `lib/` 里还有 cot / tdp 自己放的东西——上面带"不记账"注的都是。`gpm uninstall` 只按账本回放、**不扫目录**，否则"卸载一个 GUI 应用"会把用户的 go / java 一起删掉（[`DESIGN.md`](DESIGN.md) D34）。`<家>` 本身是什么路径由调用方决定
（第 1 节：`--dir` > 按 `requires` 取的家 > 从自己的位置推断 > 平台数据目录/<简称> > 当前目录），
安装布局与它无关。

`bin/`、家目录顶层与 `lib/` 的分界是**有没有图形界面**，不是"是不是可执行文件"（v3.6）：
带 GUI 的应用本体住在家目录**顶层**（`<家>/AI Desk.app`），没有图形界面的小东西——
终端启动器与 gpm 自己——住在 `bin/`，而 `lib/<id>_<version>_<os>_<arch>/` 只留给
**命令行插件**（cot 正在用）。这条分界也是 gpm 能"从自己在哪儿反推家目录"的依据（第 1 节）：
用户在新终端里敲 `gpm list` 时环境里并没有 `COT_HOME`（工具链没被激活过），`<家目录>/bin/gpm` 这个位置
把它找回来——判据是所在目录正好叫 `bin`、文件名正好是 `gpm`、且上一级有账本（`<家目录名>-state.json`；v3.6 及以前的 `state.json` 也认），
所以 `/usr/local/bin/gpm` 这种地方不会被误认。

**入口落点 = `payload/` 根下那个名字**（v3.6）：gpm 不拼接目录名，`payload/AI Desk.app`
就叫 `<家>/AI Desk.app`。v3.5 及其以前是 `<家>/lib/<id>_<version>_<os>_<arch>/`，
那个形状现在只属于命令行插件（cot 的 `lib/go_1.27.1_darwin_arm64/` 正是它）。
**重装是覆盖式的、不是并存的**：同一个 `id` 再装一次，gpm 先按卸载流程删掉**账本里记的那个
`dir`** 与启动器，再落新的——所以 v3.5 装在 `lib/…` 里的老包会被干净换掉，不会留下孤儿
（"升级"中间有一小段窗口期旧版本已经不在了——本版接受这个代价，换来的是"一个 id 只有一处落点"的简单状态）。

**但动手之前要先查那个应用在不在跑**：入口底下还有活着的进程时，覆盖安装与卸载
都**拒绝执行**（一个字节都不动），并说明"它不会自己退出、之后读到的东西可能是另一份"；
显式加 `--force` 才继续。理由与实现见 `DESIGN.md` §2.9.1。查不出来（平台不支持 / 没权限）时
只提示、不拦。

## 6. 外部副作用（全部登记在账本 `<家目录名>-state.json` 里）

| 平台 | 副作用 | 卸载时 |
|---|---|---|
| 全平台 | `<家>/bin/<cmd>` 启动器（Windows 上是 `<cmd>.cmd`） | 删除 |
| 全平台 | `<家>/bin/gpm`（Windows 上是 `gpm.exe`）自拷贝 | 家里已有一份时比一次版本（v3.11、D41）：这份严格更新才替换并记账，否则留着且不记账。账本为空时删除；但环境里有 `COT_HOME` / `TDP_HOME`（激活过的 shell）时留着 |
| 全平台 | 入口：`<家>/<payload 顶层名>`（v3.6；v3.5 是 `<家>/lib/<id>_<ver>_<os>_<arch>/`） | 整份删除（账本 `dir`） |
| macOS | `~/Applications/<name>.app` 软链（进启动台 / Spotlight） | 删除 |
| Linux | `~/.local/share/applications/<id>.desktop`（遵守 `$XDG_DATA_HOME`） | 删除 |
| POSIX | shell 配置里的 `# >>> gpm >>>` … `# <<< gpm <<<` 标记块 | 摘除标记块 |
| Windows | `HKCU\Environment` 的 `Path` 最前面那一条 | 摘除那一条 |
| Windows | `%APPDATA%\Microsoft\Windows\Start Menu\Programs\gpm\<name>.lnk` | 删除 |

工具链自己的资产（`bin/cot`、`bin/tdp`、`env-cot*`、`activate*`、`lib/` 下的插件）**不在这张表里**：它们不是 gpm 写的，卸载时一律不碰（第 5 节、[`DESIGN.md`](DESIGN.md) D34）。家里已经有这家工具链的命令时（`<家>/bin/cot` 等），gpm 连"跑一遍包里那份"都不做，只打印一行"已经装好 …，跳过"（第 7 节、[`DESIGN.md`](DESIGN.md) D37）。

**PATH 集成是唯一需要用户点头的副作用**：写入 shell 配置前必须用人话问一次，
用户拒绝时软件照装、图标照建，只是终端里敲不出来（功能降级，不是失败）。

macOS 上 zsh 不读 `~/.profile`，所以落点按 `$SHELL` 计算：zsh 写
`~/.zprofile` + `~/.zshrc`，bash 写 `~/.bash_profile`，fish 写
`~/.config/fish/config.fish`；不论 `$SHELL` 都再写一份 `~/.profile` 兜底。
标记块本身是幂等守卫，重复写入不会把 PATH 撑大。

Windows 上的两条硬规定：

* **读出什么类型就写回什么类型**（`REG_SZ` / `REG_EXPAND_SZ`）。用
  `[Environment]::SetEnvironmentVariable(..., "User")` 会把 `REG_EXPAND_SZ`
  压成 `REG_SZ`，用户原有的 `%USERPROFILE%` 之类从此不再展开——所以必须直接操作
  注册表原值。写完广播 `WM_SETTINGCHANGE`，否则已经开着的进程不会重读环境。
* **往返必须逐字节保真**：插入时原值一个字节不动（只在最前面加一条），摘除时按
  `;` 切分、只丢掉命中的那一条、**空条目原样保留**（Windows 上空条目表示"当前
  目录"，用户 PATH 的默认值结尾本来就带一个分号）。判重要归一化大小写、正反斜杠
  与未展开的 `%VAR%`。

## 7. bootstrap 脚本

`install.sh`：

```sh
#!/bin/sh
set -eu
cd "$(dirname "$0")"
chmod +x ./gpm 2>/dev/null || true
exec ./gpm install . --dir "${COT_HOME:-$HOME/cot}" "$@"
```

`install.cmd`：

```bat
@echo off
setlocal
cd /d "%~dp0"
if not defined COT_HOME set "COT_HOME=%USERPROFILE%\cot"
gpm.exe install . --dir "%COT_HOME%" %*
pause
```

`install.ps1`（v3.15、D44；给终端用户与随包的 GUI-Setup 用）：

```powershell
if (-not $env:COT_HOME) { $env:COT_HOME = "$env:USERPROFILE\cot" }
$ErrorActionPreference = 'Stop'
# 7.3 起这个开关决定原生命令失败要不要抛异常；显式关掉，免得将来默认翻转改掉退出码语义。
$PSNativeCommandUseErrorActionPreference = $false
try {
  Set-Location -LiteralPath $PSScriptRoot
  & "$PSScriptRoot\gpm.exe" install . --dir $env:COT_HOME @args
} catch {
  Write-Host $_.Exception.Message
  if ($LASTEXITCODE) { exit $LASTEXITCODE }
  exit 1
}
if ($null -eq $LASTEXITCODE) { exit 1 }
exit $LASTEXITCODE
```

三条脚本只做四件事：切到自己的目录、把家解析成一个具体路径、把 gpm 叫起来、**把用户给的参数原样转交**——`install.sh` 结尾是 `exec ./gpm install . --dir "…" "$@"`、`install.cmd` 是 `gpm.exe install . --dir "…" %*`、`install.ps1` 是 `& "$PSScriptRoot\gpm.exe" install . --dir … @args`（v3.12、D42；ps1 见 v3.15、D44）。拼接位置固定在烘进去的 `--dir` **之后**，靠 Go flag「后出现者覆盖先出现者」让调用方显式给的家赢过打包默认值；`"$@"` 必须带引号（不带引号的 `$@` 会把带空格的路径再拆一次词），`%*` 不带引号（cmd 里 `"%*"` 会把整串并成一个参数），`@args` 直接 splat（实测带空格与中文都能逐元素传到底）。
**安装逻辑一行都不在脚本里**，所以三个平台不会各自跑偏。
工具链自举同样不在脚本里——它由 gpm 按清单的 `requires` 编排（第 3 节、[`DESIGN.md`](DESIGN.md) §2.5.1）。

上面那几行里的 `$HOME/cot`、`%USERPROFILE%\cot`、`$env:USERPROFILE\cot` 是 `gpm pack` 按清单的 `requires`
（或打包方给的 `--default-dir "~/cot"`）烘进来的默认值（`~` 展开：POSIX 用 `$HOME`，
cmd 用 `%USERPROFILE%`，PowerShell 用 `$env:USERPROFILE` —— 它不认 `%…%`）。没给 `--default-dir`、清单也没有 `requires` 时，脚本里
**没有 `--dir` 这一项**——让 gpm 自己按第 1 节的链去定。**烘进去的只是默认值**：用户仍可用 `COT_HOME` / `TDP_HOME`
环境变量或 `--dir` 覆盖。v3.12 起，脚本收到的参数也会原样交给 gpm，所以 `./install.sh --dir ~/tdp` 里那个 `--dir` 排在烘进去的之后、直接盖掉它。

**`.ps1` 那四条规矩**（都是实测逼出来的）：① **UTF-8 with BOM + CRLF**——Windows PowerShell 5.1
读没有 BOM 的文件时按当前 ANSI 代码页解码，烘进来的中文家路径会**静默**乱码、装到别的目录去；
② **不写 `param()` 块**——`-File` 下 `--dir` / `--yes` 才会原样进 `$args`（5.1 与 7 都如此），
声明了 param 块就会被当成参数名去绑定；③ 参数用 `@args` 转交、位置同上；④ 退出码要带出去，
而且**不能把「没跑起来」当成功**（可执行文件找不到时 `$LASTEXITCODE` 是空的，直接 `exit` 它
就成了 0）。另外它**不写 `pause` / `Read-Host`**——要「按任意键」的是双击场景，那条路仍归
`install.cmd`。它自己**必须由调用方带 `-ExecutionPolicy Bypass` 调起**：从下载的 zip 里解出来的
`.ps1` 带着下载标记，`RemoteSigned`（Windows 客户端的常见策略）会直接拒绝执行——见
gsetup-go 的 `REQUIREMENTS.md`。

`<bin>/gpm` 已经存在时**比一次版本**（v3.11、D41）：先跑它的 `--version` 把版本问出来，
包里这份**严格更新**才替换掉它（并计入账本 `self`），同版本 / 更旧 / 问不出来（不是可
执行文件、跑不起来、超时、输出认不出）一律原样留着、只提示一句，也不计入账本；
`--force` 无视版本直接覆盖。替换**不看那个文件是谁放的**（[`DESIGN.md`](DESIGN.md) D41、
R19）。反过来，**gpm 自己拷进去的那一份**（账本 `self`）在卸载到最后一个包时删除——
除非环境里有 `COT_HOME` / `TDP_HOME`：那说明用户正在一个激活过的 shell 里，这个家归
工具链管，那份 gpm 留着（想删就先 `unset` 再卸）。

## 8. CLI（本版）

```
gpm install <目录或 .zip> [--dir PATH] [--with cot[,tdp]] [--yes] [--no-path] [--skip-verify] [--force]
gpm list
gpm where <id>
gpm uninstall <id> [--dir PATH] [--yes] [--force]
gpm env [--dir PATH]          # 打印 export PATH=... （PATH 集成被拒时用）
gpm pack <装配目录> [--out PATH] [--os OS] [--arch ARCH] [--gpm 可执行文件] [--default-dir PATH] [--setup PATH]
```

家的解析优先级有**两条链**（第 1 节）：

* **安装那一次**（`gpm install`）：`--dir` > **按 `requires` 取的家**（`$COT_HOME` / `$TDP_HOME`）
  > 从 gpm 自己的位置推断（二进制正好在 `<家目录>/bin/gpm`、且上一级有账本
  （`<家目录名>-state.json`，旧名 `state.json` 也认）时，家目录就是上一级）>
  **平台数据目录/<简称>**（`requires` 为空时：macOS `~/Library/Application Support`、
  Windows `%LOCALAPPDATA%`、Linux `${XDG_DATA_HOME:-~/.local/share}`）> 当前目录。
* **装完之后**（`list` / `where` / `uninstall` / `env`）：`--dir` > 从 gpm 自己的位置推断
  > `$COT_HOME` / `$TDP_HOME` > 当前目录（不看平台数据目录）。

`--force` 绕过的只有三道闸：**"那个应用正在运行"**（第 5 节末）、**"这个命令名已经被别人占着"**
（第 3 节）与**"家里已经有别人的同名入口"**（第 3 节）。它不是忽略一切错误的万能开关——
`payload/` 根下多一个条目、入口名撞家骨架这两条，连 `--force` 也拒。
另外，**工具链要重铺也得 `--force`**：家里已经有 `<家>/bin/cot`（或 `tdp`）时，不带 `--force` 就跳过自举（第 7 节、[`DESIGN.md`](DESIGN.md) D37）。

`--yes` 与 `--force` 是两件事：`--yes` 回答"不用问我了"（install 的 PATH 集成询问、**uninstall 的"确定要删吗"**），`--force` 回答"我知道有风险，照做"（应用正在运行、命令名被别人占着、家里有别人的同名入口、重铺工具链）。**脚本里调 `uninstall` 必须显式给 `--yes`**：不是交互终端又没有 `--yes` 时它什么都不删，退出码仍是 `0`，输出里带一行可照抄的命令——忘给会在第一次运行时就看得见地停下，而不是安静地删掉东西（[`DESIGN.md`](DESIGN.md) D39、§2.9.2、R17）。

`gpm pack` 是**发布者**侧的（给自己 CI 用），不是终端用户用的；它的输出就是本文第 2 节那个 zip。
`--default-dir` 同样只在打包时用，它写进生成脚本、运行时不参与解析。`--with` 覆盖清单里的 `requires`，只用于调试（第 3 节）。
`--setup` 指出清单 `setup:` 段声明的那份图形安装器（第 2、3 节）：它落在 zip 顶层、不进 `SHA256SUMS`（v3.13）。

## 9. 与设计文档的关系

本文是**契约**：字段、路径、脚本、CLI 以本文为准。
"为什么这么设计"、三平台现状、CI、风险与开放问题、以及被废弃的 v1/v2 范围，
都在 [`DESIGN.md`](DESIGN.md)（现为 v3.15：Windows 上多一条 `install.ps1`、三条入口并存（D44、FR-33）+ 随包的图形安装器走清单 `setup:` 段、由 `gpm pack --setup` 放 zip 顶层（D43、FR-32）+ 入口脚本把用户的参数原样转交（D42、FR-31）+ 安装器自己擦掉下载标记（D40）+ 收掉空的 `staging/`（D40）+ gpm 自身也能升级（D41）+ 卸载之前先问一句、`--yes` 跳过（D39、§2.9.2）+ 工具链已经装好就跳过（D37）+ 失败回滚只回滚 GUI（D34、FR-25）+ 账本写回之前比对原文、不加锁（D38）+ 账本按家命名 `<家目录名>-state.json`（D36）+ gpm 之名 + 家 = 工具链自己的家 + **入口落在家目录顶层**（D35）+ 清单声明 `requires` 并离线自举 zip 自带的 cot / tdp + 启动器注入环境 + 装完之后从自己的位置把根找回来 + 三平台已落地 + CI 跑绿；**v3.14 补三处"契约说了、代码没做/没写"**：fish 也写 `~/.profile`、`$SHELL` 没设时按平台默认、以及把家目录的两条链（安装 `ResolveInstall` / 装完之后 `Resolve`）写进第 1、8 节）。
