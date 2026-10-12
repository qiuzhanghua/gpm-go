# gpm — GUI 应用安装器

把「我的应用怎么装到别人的机器上」压缩成 **下载一个 zip → 解压 → 跑一个脚本**，
装完之后**图标能点、终端能敲、还能干净卸掉**。

**gpm = GUI Package Manager**，但它是 `dpkg`，不是 `apt`：会装、会卸、会列、会查，
**没有 `upgrade` 子命令，也没有一行网络代码**（`grep -rn 'net/http\|http.Get\|net/url' --include='*.go' .` 无命中）。
分发者自己决定什么时候出新版本、用户自己把 zip 拿过来 —— 这是整套设计的前提，不是「以后补上」。

- 当前软件版本 **v0.6.6**，设计契约 **v3.15**（[两套版本号](#两套版本号)）。
- macOS / Linux / Windows 一份实现：安装逻辑全在 Go 里，`install.sh` / `install.cmd` / `install.ps1` 里只有几行胶水
  （切到自己的目录、把家解析成一个具体路径、把安装交给 gpm、把用户给的参数原样转交）。
- **主要用户**是拿到分发包、想双击装上的非开发人员；**次要用户**是要 `--yes` 无人值守的 CI 与内网运维。

## 这是什么

一个 zip 装**一个带图形界面的应用**，zip 里同时带着 gpm 自己、清单、载荷，
以及（清单声明了 `requires` 时）要一起铺到本机的 cot / tdp 工具链：

```
ai-desk-1.0.0-darwin-arm64.zip
├── install.sh          # macOS / Linux 的 bootstrap
├── install.cmd         # Windows 的 bootstrap（双击这条；策略禁脚本时的退路）
├── install.ps1         # Windows 的 bootstrap（PowerShell 5.1 / 7，终端与 GUI-Setup 用）
├── GUI-Setup.app       # 选填：随包的图形安装器（清单 setup: 段声明）
├── gpm                 # 本平台的 gpm 二进制（Windows 为 gpm.exe）
├── ad-manifest.yaml    # 清单，文件名 = <简称>-manifest.yaml
├── tools/              # 选填：requires 非空时才有，装本机时先铺工具链
│   └── darwin_arm64/cot
├── payload/            # 载荷：根下只能有【一个】条目
│   └── AI Desk.app     # 它整份落到 <家>/AI Desk.app
└── SHA256SUMS          # 校验和，gpm pack 生成
```

用户不需要预装任何东西：zip 自带 gpm，整个安装过程**不联网**。

目前真实在用的例子是 [AI Desk](https://github.com/qiuzhanghua/ai-desk)：
它的 CI 用 `gpm pack` 把 Tauri 产物 + 随包的 cot / tdp 打成一个 zip，
用户解压跑一遍脚本，就得到 `~/cot/AI Desk.app`、`~/Applications` 里的图标、
以及一个 `ad` 启动器。gpm 不为 Tauri 做特殊处理 —— 它只是「一个 zip，一个 GUI 应用」。

要单独拿一份 gpm（比如给自己的打包流程用），去
[Releases](https://github.com/qiuzhanghua/gpm-go/releases) 下 `gpm-<版本>-<os>-<arch>[.exe]` 就行 ——
它是 `CGO_ENABLED=0` 编出来的静态二进制，拷到哪儿都能跑。

## 装：用户视角

```sh
unzip ai-desk-1.0.0-darwin-arm64.zip
cd ai-desk-1.0.0-darwin-arm64
./install.sh                      # macOS / Linux
install.cmd                       # Windows：双击，或在 cmd 里跑
install.ps1                       # Windows：PowerShell 里跑（5.1 / 7）
```

脚本收到的参数会**原样转交**给 gpm（v3.12 起），所以下面这些都成立：

```sh
./install.sh --yes                # 无人值守：连 PATH 那次询问也跳过
./install.sh --dir ~/tdp          # 装到别处（脚本里烘的 --dir 排在前面，被这个盖掉）
./install.sh --force              # 应用正开着 / 命令名撞了，也要照做
```

包里带图形安装器时（清单有 `setup:` 段），也可以双击：macOS 是 `GUI-Setup.app`，
Windows 是 `GUI-Setup.exe`，Linux 是顶层那个裸可执行文件。GUI-Setup 负责选目录、
装完把界面跟着变，最后调的还是同一个 gpm —— 见 [gsetup-go](https://github.com/qiuzhanghua/gsetup-go)。

安装期间**只有一次询问**：要不要把 `<家>/bin` 写进 PATH。拒绝也照样装完，
只是终端里敲不出来（属于功能降级，不是失败）；之后想要那行环境变量，`gpm env` 会打印给你。

### macOS：被 Gatekeeper 拦住怎么办

带「下载」标记的 `.app` 双击时，macOS 可能把它挪到一个**只读临时目录**里运行
（App Translocation），在那儿看不见包里的 `install.sh` 与清单。所以：

- 命令行这条路不受影响：`./install.sh` 照跑，gpm 装完还会递归清掉入口上的 `com.apple.quarantine`（v3.10 起）。
- 一定要双击的话，解压后先 `xattr -dr com.apple.quarantine <解压出来的目录>` 再双击。

根治办法是签名 + 公证 —— 那是发布阻塞项：quarantine 那一半已经由 v3.10 解决，
签名/公证这一半还没做（`docs/DESIGN.md` 的 R1，状态 half done）。

## 装完的样子

```text
<家>/                             # = $COT_HOME（缺省 ~/cot）或 $TDP_HOME（缺省 ~/tdp）
├── bin/
│   ├── gpm                       # 自动拷进来，保证之后 list / uninstall 可用
│   ├── ad                        # 终端启动器（Windows 上是 ad.cmd）
│   ├── cot / tdp                 # 工具链自举的产物：gpm 不记账、卸载不碰
│   └── activate* / env-cot*      # cot 的 activate 框架，同上
├── AI Desk.app                   # 入口本体：payload/ 根下那一个条目原样落在这里
├── lib/                          # 命令行插件的命名空间（gpm 不往里写；cot 在用）
├── staging/                      # 解包中转，每次安装一个 unpack-<纳秒>，装完收掉
└── cot-state.json                # 账本：所有外部副作用的唯一真相
```

三件容易误解的事：

* **这个家不是 gpm 独占的。** 一个家里同时住着 cot / tdp 自己的东西（`bin/cot`、
  `lib/go_1.27.1_darwin_arm64/` …）。所以 `gpm uninstall` **只按账本回放，不扫目录** ——
  否则「卸载一个 GUI 应用」会把用户的 go / java 一起删掉。
* **重装（也就是升级）是覆盖式的，不是并存的。** 同一个 `id` 再装一次，gpm 先按卸载流程
  删掉账本里记的那个落点与启动器，再落新的；一个 id 只有一处落点。
* **动手之前先看那个应用在不在跑。** 入口底下还有活着的进程时，覆盖安装与卸载都拒绝执行
  （一个字节都不动），说明它不会自己退出；显式 `--force` 才继续。查不出来时只提示、不拦。

## 用（CLI）

```text
gpm install <目录或 .zip> [--dir PATH] [--with cot,tdp] [--yes] [--no-path] [--skip-verify] [--force]
gpm list [--dir PATH]
gpm where <id> [--dir PATH]
gpm uninstall <id> [--dir PATH] [--yes] [--force]
gpm pack <装配目录> [--out PATH] [--os OS] [--arch ARCH] [--gpm 可执行文件] [--default-dir PATH] [--setup PATH]
gpm env [--dir PATH]
gpm version
```

| 命令 | 干什么 |
|---|---|
| `gpm install <目录或 .zip>` | 解包 → 校验和 → 工具链自举 → 落入口 → 建启动器与图标 → 把副作用记进账本 |
| `gpm list`（`ls`） | 列出这个家里账本记着的包 |
| `gpm where <id>` | 打印这个 id 的落点（应用在哪、启动器在哪） |
| `gpm uninstall <id>`（`remove` / `rm`） | 照账本回放删除 |
| `gpm env` | 打印那行 `export PATH=…`（PATH 集成被拒、或要自己动手时用） |
| `gpm pack <装配目录>` | **发布者**侧：装配目录 → 分发包 zip |
| `gpm version`（`--version` / `-v`） | 版本号 |
| `gpm help`（`-h` / `--help`） | 上面这段用法 |

`list` / `where` / `env` 都认 `--dir`，只是 `gpm help` 印出来的用法里没写全。

日常最常用的几条：

```sh
gpm list                                  # 我在这个家里装了啥
gpm where ai-desk                         # AI Desk 装到哪去了
gpm install ./ai-desk-0.3.4-darwin-arm64.zip --yes
gpm uninstall ai-desk                     # 交互终端上摊开要删的东西，问一句
```

### `--yes` 与 `--force` 是两件事

* `--yes` 回答「**不用问我了**」：install 时跳过 PATH 集成的询问，uninstall 时跳过「确定要删吗」。
* `--force` 回答「**我知道有风险，照做**」：应用正在运行、命令名已经被别人占着、
  家里已有别人的同名入口、工具链要重铺。

**脚本里调 `uninstall` 必须显式给 `--yes`。** 不是交互终端又没有 `--yes` 时它什么都不删、
退出码仍是 `0`，输出里给一行可照抄的命令 —— 忘给会在第一次运行时就看得见地停下，
而不是安静地删掉东西。

`--force` 也不是忽略一切错误的万能开关：`payload/` 根下多了一个条目、入口名撞家骨架
（`bin` / `lib` / `staging` / 账本 / `log`）这两条，谁来都拒。

### 家目录按这个顺序确定

**安装时**（`gpm install`）：

1. `--dir`
2. 清单里 `requires` 第一家的家（`$COT_HOME` / `$TDP_HOME`，缺省 `~/cot` / `~/tdp`）
3. **从 gpm 自己的位置推断**
4. 平台数据目录 + 简称（`requires` 为空时：macOS `~/Library/Application Support/<简称>`、
   Windows `%LOCALAPPDATA%\<简称>`、Linux `${XDG_DATA_HOME:-~/.local/share}/<简称>`）
5. 当前目录

**装完之后**（`list` / `where` / `uninstall` / `env`）走的是另一条链：

1. `--dir`
2. **从 gpm 自己的位置推断**
3. `$COT_HOME` / `$TDP_HOME`
4. 当前目录

差别是「自己住哪个家」与「环境变量」谁优先：装完之后你敲的是 `<某个家>/bin/gpm list`，
那个位置是**更具体的证据**（shell 里可能激活着另一个家，人却在看这一个），所以它排在环境变量前面；
这条链也不看平台数据目录——那是安装那一次「没有工具链可依附」时的落脚点。
两条链都写在契约里（[`docs/PACKAGE-FORMAT.md`](docs/PACKAGE-FORMAT.md) 第 1 节、
[`docs/DESIGN.md`](docs/DESIGN.md) D21 与 §2.3）。

「从 gpm 自己的位置推断」的判据：所在目录正好叫 `bin`、文件名正好是 `gpm`、
且上一级有账本（`<家目录名>-state.json`，旧名 `state.json` 也认）——
所以 `<家>/bin/gpm` 这个位置本身就把家说出来了，`/usr/local/bin/gpm` 这种地方不会被误认。

装到哪儿通常由 `install.sh` / `install.cmd` 里烘着的 `--dir` 传进来（来自 `gpm pack --default-dir`
或清单的 `requires`）；命令行上直接调 gpm 时才走上面这两条链。

## 三平台的副作用（全部登记在账本里）

| 平台 | 副作用 | 卸载时 |
|---|---|---|
| 全平台 | `<家>/bin/<cmd>` 启动器（Windows 上是 `<cmd>.cmd`） | 删除 |
| 全平台 | `<家>/bin/gpm` 自拷贝：家里那份**严格更新**才替换并记账（v3.11 起） | 账本为空时删除；但环境里有 `COT_HOME` / `TDP_HOME` 时留着 |
| 全平台 | 入口 `<家>/<payload 顶层名>` | 整份删除 |
| macOS | `~/Applications/<name>.app` 软链（进启动台 / Spotlight） | 删除 |
| Linux | `~/.local/share/applications/<id>.desktop`（遵守 `$XDG_DATA_HOME`） | 删除 |
| POSIX | shell 配置里的 `# >>> gpm >>>` … `# <<< gpm <<<` 标记块 | 摘除标记块 |
| Windows | `HKCU\Environment` 的 `Path` 最前面那一条 | 摘除那一条 |
| Windows | 开始菜单 `…\Programs\gpm\<name>.lnk` | 删除 |

几个细节是踩出来的：

* **macOS 上 zsh 不读 `~/.profile`**，所以落点按 `$SHELL` 算：zsh 写 `~/.zprofile` + `~/.zshrc`，
  bash 在 macOS 上写 `~/.bash_profile`（Linux 上是 `~/.bashrc`），fish 写 `~/.config/fish/config.fish`；
  **不论 `$SHELL` 都再写一份 `~/.profile` 兜底**（fish 也一样——它自己那份是给 fish 用的，
  将来换成 bash/sh、或者跑 `sh -l` 时，`~/.profile` 才是 PATH 的来源）。`$SHELL` 没设时
  按平台默认（macOS 是 zsh，其余是 bash）。
  标记块本身是幂等守卫，重复写不会把 PATH 撑大。
* **Windows 上读出什么类型就写回什么类型**（`REG_SZ` / `REG_EXPAND_SZ`）：用
  `[Environment]::SetEnvironmentVariable(..., "User")` 会把 `REG_EXPAND_SZ` 压成 `REG_SZ`，
  用户原有的 `%USERPROFILE%` 从此不再展开 —— 所以直接操作注册表原值，写完广播 `WM_SETTINGCHANGE`。
* **往返必须逐字节保真**：摘除时按 `;` 切分、只丢掉命中的那一条、空条目原样保留。
* **启动器会注入工具链环境**：先注入 `COT_HOME` / `TDP_HOME` 与前置的 `<家>/bin`
  （`<家>/bin/env-cot.vars` 存在时 source 它），再 exec 应用。macOS 上因为 `open` 不给环境
  （实测只有 15 项 launchd 基线），**要注入环境（`requires` 非空）或清单写了 `mode: direct`** 的 bundle
  都直接 exec `.app` 内层可执行文件 —— 代价是丢掉双击等价与单实例激活；只有
  「bundle + `requires` 为空 + `mode: activate`」这一种组合才照旧走 `open`，保住双击语义。

## 发布者视角：造一个包

装配目录里只需要两样（要自举工具链再加 `tools/`）：

```text
装配目录/
├── ad-manifest.yaml
├── payload/
│   └── AI Desk.app          # 根下只能有这一个条目
└── tools/
    └── darwin_arm64/cot     # requires 非空时才要
```

清单（文件名 = `<简称>-manifest.yaml`）：

```yaml
id: ai-desk            # 小写字母/数字/._-，目录名与账本主键
name: AI Desk          # 显示名，用来建 ~/Applications/AI Desk.app
version: 1.0.0
requires: [cot]        # 选填：要哪家工具链，决定安装根与是否自举；缺省 []
entry:
  darwin:  { bundle: AI Desk.app }   # 相对于 payload/，macOS 用 .app
  linux:   { exe: ad }               # 相对于 payload/，裸可执行文件
  windows: { exe: ad.exe }
setup:                               # 选填：随包的图形安装器，相对于【包根】
  darwin:  { bundle: GUI-Setup.app }
  windows: { exe: GUI-Setup.exe }
launch:
  cmd: ad                            # 终端里敲的命令名
  mode: activate                     # 仅 macOS 的 bundle 有意义：activate | direct
```

然后：

```sh
gpm pack 装配目录 \
  --gpm /path/to/gpm \
  --default-dir '~/cot' \
  --setup 'GUI-Setup.app' \
  --os darwin --arch arm64 \
  --out dist/ai-desk-1.0.0-darwin-arm64.zip
```

* `install.sh` / `install.cmd` / `install.ps1` / `SHA256SUMS` 都是 `gpm pack` 现场生成的**三平台同一份实现** ——
  不需要每个应用仓库各写一遍打包脚本，也就不会各自跑偏。
* `--default-dir` 只影响生成脚本里那一个默认值，用户仍可用 `COT_HOME` / `TDP_HOME` 或 `--dir` 覆盖。
* `--setup` 把图形安装器放在 zip **顶层**（与 `install.sh` 并排），**不进 `payload/`、不进 `SHA256SUMS`**：
  与 `install.sh` / `gpm` / 清单同类，属于包根上的「信任起点」。清单声明了 `setup:` 就必须给 `--setup`，
  七种错法（声明了没给 / 给了没声明 / 名字对不上 / bundle 给了文件 / exe 给了目录 / setup 缺当前平台 /
  路径越出包根）都在打包时报错，且不留半包。
* `SHA256SUMS` 覆盖 `payload/` 与 `tools/` 下的每一个常规文件，装的时候逐条校验，有一条对不上就中止。
  缺失这份文件时打印警告并继续，账本把这次安装标成 `unverified`；`--skip-verify` 只用于调试。
* 平台不匹配的包 gpm 直接拒绝，不会装出一个跑不起来的目录。

AI Desk 的 `tools/package.sh` 就是这套东西的一层壳，可当完整例子看：
[ai-desk/README.md § 打包成 gpm 分发包](https://github.com/qiuzhanghua/ai-desk#打包成-gpm-分发包)。

### 与 AI Desk 的构建契约

gpm 的 release 产物命名是**别的仓库的构建契约**：

```text
gpm-<版本>-<os>-<arch>[.exe]        例如 gpm-0.6.5-darwin-arm64
```

ai-desk 的 CI 就按这个名字 `gh release download` 拿二进制。**改了这里必须同步改 ai-desk 的
`.github/workflows/release.yml`**（这一条同时写在 `.github/workflows/release.yml` 的注释里）。

## 故意不做的事

| 不做 | 为什么 |
|---|---|
| 抓取 / 升级 / 版本源 / 镜像表 | 那是 `apt` 的活。gpm 是 `dpkg`：装了 zip 就完事，没有一行网络代码 |
| 装纯命令行工具与语言运行时的「下载」 | 入口是**图形界面**。`requires` 只搬 zip 里自带的 cot / tdp 那张壳，插件仍归工具链管 |
| 合成 `.app` 外壳 | 上游只发裸可执行文件的 macOS 应用现在装不了：硬塞会得到一个没有应用身份、双击被交给终端的东西（设计已定、未实现，见 R8） |
| 「卸载 = 删掉一个目录」 | 那个家里还住着 cot / tdp 的东西，只能按账本删 |
| 账本加锁 | 改的是「写回之前比对原文」：被别人动过就让这次操作失败、提示重跑（不加锁文件） |
| 回滚一切 | 失败时只回滚 GUI 那部分，命令行部分（cot 等）不回滚 —— 说不定以前就装好了 |

## 两套版本号

这个仓库里同时有两个「版本」，在 commit message 和别人的 README 里都会出现：

* **软件版本 `v0.6.5`** —— git tag、release 资产名、`gpm version` 打印的那个。
* **设计契约 `v3.15`** —— `docs/DESIGN.md` 的版本，配套的决策编号（`D41`、`FR-32`、`R1` …；
  `FR-*` / `A*` 的完整清单在 `docs/REQUIREMENTS.md`）与 `docs/PACKAGE-FORMAT.md` 顶部那句
  「契约 · 已冻结 · v3.14」。契约号变大表示**接口/行为**改了，软件版本按需发布。

看到「gpm v3.11 起」说的是行为契约，看到「gpm 0.6.5」说的是那个二进制。

## 文档

| 文件 | 是什么 |
|---|---|
| [`docs/PACKAGE-FORMAT.md`](docs/PACKAGE-FORMAT.md) | **契约（已冻结）**：分发包结构、清单字段、`SHA256SUMS`、装完的布局、副作用表、bootstrap 脚本、CLI。AI Desk 的 CI 与 gpm 之间唯一以本文为准 |
| [`docs/DESIGN.md`](docs/DESIGN.md) | 设计与决策：为什么是 gpm、家的选择、安装流水线、三平台现状、风险与开放问题（`D*` 决策、`FR-*` 需求、`A*` 验收、`R*` 风险） |
| [`docs/REQUIREMENTS.md`](docs/REQUIREMENTS.md) | 需求原文与逐版裁决记录 |

## 开发与测试

```sh
go build ./cmd/gpm                 # 出来就是 ./gpm
go test ./...                      # 本机单测
go vet ./...

# 给二进制注入版本号（发布就是这么干的）
go build -trimpath -ldflags "-s -w -X main.version=0.6.5" -o gpm ./cmd/gpm
```

代码结构：

| 目录 | 干什么 |
|---|---|
| `cmd/gpm/` | CLI 分派与 `usage`（`install` / `uninstall` / `list` / `where` / `pack` / `env` / `version` / `help`） |
| `internal/manifest/` | 清单解析与校验（`<简称>-manifest.yaml`） |
| `internal/pack/` | `gpm pack`：装配目录 → 分发包（含生成 bootstrap 脚本与 `SHA256SUMS`） |
| `internal/install/` | 安装流水线：解包 → 校验和 → 工具链自举（先装工具链：它装不上时应用这边一个字节都还没动）→ 落入口 → 启动器与图标 → 记账 |
| `internal/integrate/` | 平台副作用：PATH 标记块、`~/Applications` 软链、`.desktop`、开始菜单与注册表 |
| `internal/ledger/` | 账本 `<家>/<家目录名>-state.json`：所有外部副作用的唯一真相 |
| `internal/stage/` | `staging/` 解包中转 |
| `internal/home/` | 家目录的解析与推断 |
| `internal/proc/` | 「那个应用正在不在跑」 |

CI（[`.github/workflows/ci.yml`](.github/workflows/ci.yml)）：

* **三平台**（ubuntu / macos / windows）跑 `go vet ./...` + `go test ./...`；Linux 上先装
  `desktop-file-utils`（`entry_linux_test.go` 会跑一次 `desktop-file-validate`）。
* Windows 上额外用 `GPM_TEST_REGISTRY=1` 跑一次 `TestUserPathRoundTrip` —— 那条会**真的改一次
  `HKCU\Environment` 的 `Path` 再放回**，所以只在一次性 runner 上开；开发机上别随手加这个环境变量。
* 另有一条 `crosscompile`：darwin / linux / windows × amd64 / arm64 **六个平台**都要真的编得出来，
  否则别人（比如 ai-desk）下载不到东西。

## 发版

```sh
git tag -a v0.6.5 -m "gpm v0.6.5：……"
git push gh main v0.6.5        # GitHub：CI 与 Release 都在这儿
git push gitee main v0.6.5     # Gitee：镜像，同样推 main 与 tag
```

（这个仓库有两个远端：`gh` = GitHub，`gitee` = Gitee 镜像。CI 与发 Release 都在 GitHub，
Gitee 只做镜像 —— 但**两边都推 main 与 tag**，别只推一边。）

[`.github/workflows/release.yml`](.github/workflows/release.yml) 收到 `v*` tag 后在 ubuntu 上交叉编出
六份产物，跑 `gh release create` 建 Release 并附上（也可以在 Actions 页面手动指定 tag 触发）。
产物名与 AI Desk 的契约见上文。

## 许可证

MIT，© 太极计算机股份有限公司创新研究院。见 [`LICENSE`](LICENSE)。
