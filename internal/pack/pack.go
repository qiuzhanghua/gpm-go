// Package pack 把一个「已经装配好的目录」打成分发包。
//
// 装配目录里该有的是：`<简称>-manifest.yaml`、`payload/`，带工具链的包
// 再加 `tools/<os>_<arch>/`。打包要做三件在跨平台上很难做对的事 —— 算
// sha256、保住可执行位、处理符号链接，所以它留在 gpm 里而不是一个 shell
// 脚本里。Windows 的 Git Bash 里连 zip 都没有；而 macOS 的 .app 内部是有
// 符号链接的，普通 zip 会把它们展开成副本（甚至坏掉）。
package pack

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/qiuzhanghua/gpm-go/internal/manifest"
	"github.com/qiuzhanghua/gpm-go/internal/stage"
)

// 包里那三条「用户双击/运行」的入口。由 pack 生成，装配方不用自己写。
//
// v3.15 起多一条 install.ps1（D44）：Windows 上给终端用户与随包的 GUI-Setup 用；
// install.cmd 仍是「双击就能跑」的那条路，也是策略禁止脚本时的退路（两者都留）。
const (
	InstallSH  = "install.sh"
	InstallCMD = "install.cmd"
	InstallPS1 = "install.ps1"
)

// shell 是「这条入口脚本由谁解释」。同一个家在三种脚本里得写成三种样子：
// $HOME / %USERPROFILE% / $env:USERPROFILE%。
type shell int

const (
	shellSH  shell = iota // install.sh：POSIX sh
	shellCMD              // install.cmd：cmd.exe
	shellPS1              // install.ps1：Windows PowerShell 5.1 及以上
)

// scriptDir 把 --default-dir 给的路径转成脚本里能直接用的写法。
//
// 要处理的只有开头的 `~`：双引号里的 `~` 不会展开，`$HOME` / `%USERPROFILE%` /
// `$env:USERPROFILE` 会。除此之外原样照抄 —— 装到哪儿是打包方的决定，这里不替它做主。
func scriptDir(defaultDir string, sh shell) string {
	if defaultDir != "~" && !strings.HasPrefix(defaultDir, "~/") && !strings.HasPrefix(defaultDir, `~\`) {
		return defaultDir
	}
	rest := defaultDir[1:]
	if sh == shellSH {
		return "$HOME" + rest
	}
	// 顺手把分隔符也换掉：`~/cot` 展开成 `%USERPROFILE%/cot` 能用，
	// 但在 Windows 上看起来就是个错的东西。
	rest = strings.ReplaceAll(rest, "/", `\`)
	if sh == shellPS1 {
		// PowerShell 不认 %USERPROFILE%：那是 cmd 的写法，在双引号里也不会展开。
		return "$env:USERPROFILE" + rest
	}
	return `%USERPROFILE%` + rest
}

// homeName 认出「某家工具链的家」这种写法：`~/cot`、`~\tdp`、`~`。
//
// 只认清单允许的那两家（manifest.KnownRequires）：把 `--default-dir ~/ad`
// 也变成 `$AD_HOME` 就等于替某个应用发明了一个环境变量。
func homeName(p string) (string, bool) {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return "", false
	}
	rest := strings.Trim(p[1:], `/:\`)
	if rest == "" || strings.ContainsAny(rest, `/:\`) {
		return "", false
	}
	if !manifest.IsKnownRequire(rest) {
		return "", false
	}
	return rest, true
}

// toolchainDir 是「某家工具链的家」在脚本里的写法：环境变量优先，缺省落到
// 用户目录下的同名目录 —— 与 gpm 内部的 home.RequireHome 同一套规矩，
// 所以用户在 shell 里设了 COT_HOME，双击安装脚本也认。
//
// 返回的 pre 是「设默认值」那一行，arg 是 --dir 的写法。install.cmd 那一支的行尾
// 必须是 CRLF（理由见 InstallBatch）；install.ps1 那一支故意用 LF，交给
// InstallPowerShell 一次性统一成 CRLF —— 免得两处都写 \r\n 时变成 \r\r\n。
func toolchainDir(req string, sh shell) (pre, arg string) {
	name := strings.ToUpper(req) + "_HOME"
	switch sh {
	case shellCMD:
		pre = "if not defined " + name + " set \"" + name + "=%USERPROFILE%\\" + req + "\"\r\n"
		return pre, ` --dir "%` + name + `%"`
	case shellPS1:
		pre = "if (-not $env:" + name + ") { $env:" + name + " = \"$env:USERPROFILE\\" + req + "\" }\n"
		// 不写引号：PowerShell 里变量展开的结果本来就是**一个**参数（带空格也一样，
		// 实测过），而 `"$env:COT_HOME"` 反而会让空值变成一个空参数。
		return pre, " --dir $env:" + name
	}
	return "", ` --dir "${` + name + `:-$HOME/` + req + `}"`
}

// dirSpec 决定脚本里的 --dir 到底怎么写。
//
//	--default-dir 是工具链的家（~/cot）  → 走环境变量，#COT_HOME 能覆盖
//	--default-dir 是别的路径             → 原样照抄（$HOME / %USERPROFILE% 展开）
//	没给 --default-dir，但清单有 requires → 用第一家工具链的家
//	都没有                                → 不传 --dir，交给 gpm 定（平台数据目录/<简称>）
func dirSpec(defaultDir string, requires []string, sh shell) (pre, arg string) {
	if defaultDir != "" {
		if name, ok := homeName(defaultDir); ok {
			return toolchainDir(name, sh)
		}
		return "", ` --dir "` + scriptDir(defaultDir, sh) + `"`
	}
	if len(requires) > 0 {
		return toolchainDir(requires[0], sh)
	}
	return "", ""
}

// InstallScript 是 install.sh 的原文。
//
// 它只做四件事：切到自己的目录、给 gpm 补可执行位、把安装交给 gpm、把用户
// 给的参数原样转交（v3.12、D42）。
// 之所以坚持 `chmod +x` 而不是指望解压工具：Windows 的资源管理器解压会丢掉
// Unix 权限位，用户也可能用别的方式解压。安装逻辑一行都不写在这里 ——
// 写在这里就等于每个平台各维护一份，而它们迟早会不一致。
//
// 转交参数的理由是一处真实的体验断裂：gpm 自己会在放弃 PATH 集成时打印一行
// 可照抄的 `./gpm install . --yes`，而用户站在解压出来的目录里更顺手的写法是
// `./install.sh --yes`。不转交的话 `--yes` 就被静默吞掉 —— 交互终端里退化成
// 一个 y/N 询问，非交互（CI、脚本）里干脆跳过 PATH 集成，而退出码仍是 0，
// 看上去像"装成功了、只是 PATH 没接上"。最后拼上 `"$@"` 就没有这回事了。
//
// 位置在 gpm 与烘进来的 `--dir` **之后**是有意的：flag 取后出现的那一个，
// 所以用户自己的 `--yes` / `--with` / `--dir` 都盖得住脚本里的默认值。
// 用带引号的 `"$@"` 而不是 `$@`，否则带空格的路径会被拆成多个参数；
// `set -u` 下 `"$@"` 也安全 —— 没有参数时它展开为空，不是未定义变量。
//
// 装到哪儿按这个次序定（与 D21 一致）：
//
//  1. --default-dir 烘进来的值（打包方明说）；写的是 `${COT_HOME:-$HOME/cot}`
//     这种带环境变量兜底的写法，所以用户在 shell 里设的家仍然算数；
//  2. 清单里 requires 的第一家工具链的家（$COT_HOME / $TDP_HOME，缺省 ~/cot、~/tdp）；
//  3. 什么都不传 —— 交给 gpm 自己定（平台数据目录/<简称>）。
func InstallScript(defaultDir string, requires []string) string {
	_, dir := dirSpec(defaultDir, requires, shellSH)
	return fmt.Sprintf(`#!/bin/sh
# 由 gpm pack 生成，请勿手工编辑。
set -eu
cd "$(dirname "$0")"
chmod +x ./gpm 2>/dev/null || true
exec ./gpm install .%s "$@"
`, dir)
}

// InstallBatch 是 install.cmd 的原文（Windows 用户双击这个）。
//
// 在 Windows 上优先用 .cmd 而不是 .ps1：PowerShell 默认 ExecutionPolicy 是
// Restricted，右键「使用 PowerShell 运行」常常直接报「在此系统上禁止运行脚本」，
// 而 .cmd 双击就能跑。
//
// 两处与 install.sh 刻意不同，都是被 cmd.exe 的解析逼出来的（都实测过）：
//
//  1. 行尾必须是 CRLF。cmd.exe 按当前代码页逐行读批处理，注释里只要有非 ASCII
//     字节，UTF-8 那些字节在别的代码页下解码时会把行尾的 LF 一起吃进去，于是
//     注释行与下一行黏连、残渣被当成命令去执行：在 OEM 936 的中文 Windows 上
//     每次运行都会先多打一行 `'…' is not recognized as an internal or external
//     command`。2×2 对照（CRLF/LF × ASCII/中文）只有 LF+非 ASCII 复现，
//     CRLF 让行尾不再落在解码歧义里。install.sh 正好相反：CRLF 会让
//     `#!/bin/sh` 失效，所以两份脚本不能共用一个模板，也各有一个测试钉着。
//  2. 注释只用 ASCII。第 1 条只保证「中文 + CRLF」不再出错，而注释是给人看的：
//     在 936 的控制台里 `type install.cmd` 出来仍是乱码。这个脚本由打包机生成、
//     要在别人的机器上读，ASCII 是唯一稳的写法。
//
// 退出码也要带出去：末行若直接是 `pause`，`cmd /c install.cmd` 拿到的是 pause 的
// 0 —— 安装失败也判成功，从脚本或 CI 里调用就会误判。所以先存 %ERRORLEVEL%
// 再 pause，最后用它退出（pause 只负责让双击的用户看清输出）。
//
// 用户给的参数也原样转交（v3.12、D42，与 install.sh 同一件事）：双击时 `%*`
// 展开为空，脚本行为与从前逐字节一致；在终端里写 `install.cmd --yes` 才真的
// 把 `--yes` 送到 gpm 手上。用 `%*` 而不是 `%1 %2 …`（后者漏掉第 10 个之后的
// 参数），也不要给它加引号 —— `"%*"` 会把所有参数粘成一个。
func InstallBatch(defaultDir string, requires []string) string {
	pre, dir := dirSpec(defaultDir, requires, shellCMD)
	return fmt.Sprintf("@echo off\r\n"+
		"rem generated by gpm pack -- do not edit.\r\n"+
		"setlocal\r\n"+
		"cd /d \"%%~dp0\"\r\n"+
		"%sgpm.exe install .%s %%*\r\n"+
		"set \"RC=%%ERRORLEVEL%%\"\r\n"+
		"pause\r\n"+
		"exit /b %%RC%%\r\n", pre, dir)
}

// InstallPowerShell 是 install.ps1 的原文（v3.15、D44）。
//
// 为什么还要第三条：Windows 上 `.cmd` 是唯一「双击就能跑」的东西（`.ps1` 双击会被
// 记事本打开），可它在终端里最难用 —— 末尾那句 `pause` 是给双击的人看的，脚本或 CI
// 调它必须拿 `< NUL` 喂一个 EOF 才不会挂住；参数靠 `%*` 转交、退出码得先存起来再
// pause。而 PowerShell 用户（以及随包的 GUI-Setup）手上有更好的选择。
//
// 四条硬规矩，每条都有实测依据（Windows 11，PowerShell 7.6 与 5.1 都试过）：
//
//  1. **UTF-8 with BOM + CRLF**。5.1 读没有 BOM 的文件时按当前 ANSI 代码页解码：
//     烘进来的家路径带中文时（`C:\用户\cot`）会**静默**变成乱码路径（7.x 正常，
//     5.1 实测出来是 `鐑樼剻鐨勮矾寰?`），装到别的目录去都不知道。BOM 是唯一让 5.1
//     也按 UTF-8 读的办法；CRLF 与 install.cmd 同理（在记事本里别是一坨）。
//  2. **不写 param() 块**。`pwsh -File install.ps1 --dir X --yes` 在 5.1 与 7 上、
//     脚本**没有** param 块时，五个参数原样进 `$args`（实测）；一旦声明了 param
//     块，`--dir` 就会被当成参数名去绑定，行为还随版本而变。
//  3. **参数用 `@args` 原样转交**，位置与另两条一样在 `gpm install .` 与烘进去的
//     `--dir` 之后（v3.12、D42）。实测带空格与中文的参数在 5.1 / 7 上都能逐元素
//     传到底：`["--dir","C:\用户\张三\a b\cot","--with","cot","--yes"]`。
//  4. **退出码要带出去，而且不能把「没跑起来」当成功**。真失败时 `$LASTEXITCODE`
//     是 3，末行 `exit $LASTEXITCODE` 外层就看到 3；但可执行文件压根找不到时
//     PowerShell 抛 CommandNotFoundException 而 `$LASTEXITCODE` **仍是空** —— 直接
//     exit 它就成了 0，安装失败判成功（与 install.cmd 那个「末行是 pause 吞掉退出码」
//     是同一类坑，只是换了个 shell）。所以先把它设成 Stop 并包在 try/catch 里。
//
// 另外两处有意为之：**切到脚本自己的目录**（`gpm install .` 里的 `.` 是当前目录，
// 不切过去就装错东西），以及**不写 pause / Read-Host** —— 要「按任意键」的是双击
// 场景，那条路仍归 install.cmd。
func InstallPowerShell(defaultDir string, requires []string) string {
	pre, dir := dirSpec(defaultDir, requires, shellPS1)
	body := pre + `$ErrorActionPreference = 'Stop'
# 7.3 起这个开关决定原生命令失败要不要抛异常；显式关掉，免得将来默认翻转改掉退出码语义。
$PSNativeCommandUseErrorActionPreference = $false
try {
  Set-Location -LiteralPath $PSScriptRoot
  & "$PSScriptRoot\gpm.exe" install .` + dir + ` @args
} catch {
  Write-Host $_.Exception.Message
  if ($LASTEXITCODE) { exit $LASTEXITCODE }
  exit 1
}
if ($null -eq $LASTEXITCODE) { exit 1 }
exit $LASTEXITCODE
`
	// BOM 与 CRLF 都在这里一次成型：模板正文用 \n 写，省得两处各写一遍 \r\n
	// 再拼成 \r\r\n。
	return "\ufeff" + strings.ReplaceAll(body, "\n", "\r\n")
}

// Options 是打一个包所需的全部输入。
type Options struct {
	Dir    string // 含 <简称>-manifest.yaml 与 payload/（必要时 tools/）的目录
	Out    string // 输出 zip 路径；留空则 dist/<id>-<version>-<goos>-<goarch>.zip
	GOOS   string // 目标平台；留空用当前平台
	GOARCH string
	Self   string // 要嵌进包里的 gpm 可执行文件；留空用当前进程
	// DefaultDir 烘进 install.sh / install.cmd / install.ps1 的默认安装根，例如 ~/cot。
	// 留空表示打包方不指定：清单里有 requires 就装进那家工具链的家，
	// 否则由 gpm 自己定（平台数据目录/<简称>）。
	DefaultDir string
	// Setup 是随包的 GUI 设置程序（GUI-Setup）的路径：清单 setup: 段里
	// 声明的那一份（macOS 是 .app 目录、Windows/Linux 是单个可执行文件）。
	// 清单声明了就必须给（v3.13、D43）。
	Setup string
	Log   io.Writer // 进度输出；留空则静默
}

// Build 打出一个可分发的 zip，返回它的绝对路径。
//
// 产物的内容与顺序都已固定：
//
//	install.sh  install.cmd  install.ps1  [GUI-Setup]  gpm  <简称>-manifest.yaml  SHA256SUMS  payload/…  tools/…
//
// GUI-Setup 是清单 setup: 段声明了才有的（v3.13、D43），位置紧挨着三条入口脚本（v3.15、D44 起 install.ps1 是第三条）
// ——解压出来第一眼看到的就是"双击这个"。
func Build(opt Options) (string, error) {
	goos, goarch := opt.GOOS, opt.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	log := opt.Log
	if log == nil {
		log = io.Discard
	}

	dir, err := filepath.Abs(opt.Dir)
	if err != nil {
		return "", err
	}
	m, err := manifest.Load(dir)
	if err != nil {
		return "", err
	}
	if err := m.Validate(goos); err != nil {
		return "", err
	}
	setup, setupEntry, err := resolveSetup(opt.Setup, m, goos, log)
	if err != nil {
		return "", err
	}

	self := opt.Self
	if self == "" {
		self, err = os.Executable()
		if err != nil {
			return "", fmt.Errorf("找不到当前可执行文件：%w", err)
		}
	}
	fi, err := os.Stat(self)
	if err != nil {
		return "", fmt.Errorf("找不到 gpm 可执行文件 %s: %w", self, err)
	}
	if fi.IsDir() {
		return "", fmt.Errorf("%s 是个目录，不是可执行文件", self)
	}

	out := opt.Out
	if out == "" {
		out = filepath.Join("dist", fmt.Sprintf("%s-%s-%s-%s.zip", m.ID, m.Version, goos, goarch))
	}
	if a, err := filepath.Abs(out); err == nil {
		out = a
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}

	// 先算 SHA256SUMS：它得在写 zip 之前就是一个确定的字节串。
	sums, n, err := sumsFor(dir)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(log, "已核算 %d 个文件\n", n)

	f, err := os.Create(out)
	if err != nil {
		return "", err
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	err = writeAll(zw, dir, self, goos, opt.DefaultDir, sums, m, setup, setupEntry)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		f.Close()
		os.Remove(out) // 半个 zip 比没有 zip 更糟：它会让人以为打包成功了
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(out)
		return "", err
	}
	return out, nil
}

func writeAll(zw *zip.Writer, dir, self, goos, defaultDir string, sums []byte, m *manifest.Manifest, setup string, setupEntry manifest.Entry) error {
	when := time.Now()

	if err := addBytes(zw, InstallSH, 0o755, []byte(InstallScript(defaultDir, m.Requires)), when); err != nil {
		return err
	}
	if err := addBytes(zw, InstallCMD, 0o644, []byte(InstallBatch(defaultDir, m.Requires)), when); err != nil {
		return err
	}
	if err := addBytes(zw, InstallPS1, 0o644, []byte(InstallPowerShell(defaultDir, m.Requires)), when); err != nil {
		return err
	}
	if setup != "" {
		if err := addSetup(zw, setupEntry.Rel(), setup, goos, when); err != nil {
			return err
		}
	}

	// 目标平台是 Windows 时，包里的二进制必须叫 gpm.exe —— install.cmd 与
	// install.ps1 调的就是它。
	selfName := "gpm"
	if goos == "windows" {
		selfName = "gpm.exe"
	}
	mode := fs.FileMode(0o755)
	if fi, err := os.Stat(self); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := addFile(zw, selfName, mode, self, when); err != nil {
		return err
	}

	// 清单名来自装配目录里那个真实文件（<简称>-manifest.yaml），不自己拼。
	name := manifest.FileNameFor(m.Short())
	if err := addFile(zw, name, 0o644, filepath.Join(dir, name), when); err != nil {
		return err
	}
	if err := addBytes(zw, stage.SumsFile, 0o644, sums, when); err != nil {
		return err
	}
	if err := addTree(zw, filepath.Join(dir, manifest.PayloadDir), manifest.PayloadDir, when); err != nil {
		return err
	}

	// 工具链是可选的：清单没写 requires 就不该有；写了就必须有（安装时会查）。
	tools := filepath.Join(dir, manifest.ToolsDir)
	if _, err := os.Stat(tools); err == nil {
		return addTree(zw, tools, manifest.ToolsDir, when)
	}
	return nil
}

// resolveSetup 把 --setup 给的路径与清单 setup: 段对一遍，返回要打进 zip 的那份。
//
// 两边都必须有，缺一个都是打包方忘了：清单说了有 GUI-Setup 而没给路径，发出去的
// 包里就只有一个 install.sh（用户双击 .app 只会看到"打不开"）；给了路径而清单没
// 声明，那个文件就白躺在 zip 里，GUI-Setup 自己按清单找根也找不到自己该是谁。
//
// 名字也要对得上：清单里写 `GUI-Setup.app`、命令行给 `/tmp/build/GUI-Setup.app`
// 是同一份东西；写成别的名字就是清单在撒谎。形态（bundle 是目录、exe 是文件）
// 一并查 —— 这条能挡住"把 .app 当成 exe 声明"这种错位。
func resolveSetup(given string, m *manifest.Manifest, goos string, log io.Writer) (string, manifest.Entry, error) {
	e, err := m.SetupFor(goos)
	if err != nil {
		return "", manifest.Entry{}, err
	}
	want := e.Rel()
	if want == "" {
		if given != "" {
			return "", e, fmt.Errorf("给了 --setup %s，但清单的 setup 里没有 %s 平台：先在清单里声明它叫什么", given, goos)
		}
		return "", e, nil
	}
	if given == "" {
		return "", e, fmt.Errorf("清单声明了 setup.%s（%s），打包时得用 --setup <路径> 指出那份文件", goos, want)
	}
	abs, err := filepath.Abs(given)
	if err != nil {
		return "", e, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", e, fmt.Errorf("找不到 --setup 给的 %s: %w", given, err)
	}
	if got := filepath.Base(abs); filepath.ToSlash(got) != filepath.ToSlash(want) {
		return "", e, fmt.Errorf("--setup 给的是 %s，而清单 setup.%s 写的是 %s：名字得一致，GUI-Setup 就是按这个名字找自己的", got, goos, want)
	}
	if e.Kind() == "bundle" && !fi.IsDir() {
		return "", e, fmt.Errorf("setup.%s 声明的是 bundle（%s），但 --setup 给的是个文件", goos, want)
	}
	if e.Kind() == "exe" && fi.IsDir() {
		return "", e, fmt.Errorf("setup.%s 声明的是 exe（%s），但 --setup 给的是个目录", goos, want)
	}
	fmt.Fprintf(log, "随包的 %s：%s\n", want, abs)
	return abs, e, nil
}

// addSetup 把 GUI-Setup 放进 zip 顶层。
//
// 目录（macOS 的 .app）整棵树搬，权限位与符号链接照旧 —— 里面的主可执行文件
// 就是靠这一位跑起来的，而 Windows 的资源管理器解压、以及某些网盘/邮件中转
// 都会把它丢掉（0644 的 .app 内层二进制是打包环节的常见事故：双击没反应，
// 报错还看不出所以然）。所以对非 Windows 目标补一次 x 位：源文件已经有 x 位
// 就原样保留，一个都没有就补成 0755。
//
// 补哪一份？只补 bundle 的主可执行文件，不是树里每个 0644 的文件 —— 把
// Info.plist、资源文件也标成可执行是另一种坏包。名字先信 Info.plist 的
// CFBundleExecutable，读不出来再退到 Contents/MacOS/<bundle 名去掉 .app>。
func addSetup(zw *zip.Writer, name, src, goos string, when time.Time) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		if goos == "windows" {
			return addTree(zw, src, name, when)
		}
		main := bundleExecutable(src)
		return addTreeWith(zw, src, name, when, func(rel string, mode fs.FileMode) fs.FileMode {
			if main != "" && filepath.ToSlash(rel) == main && mode&0o111 == 0 {
				return mode | 0o755
			}
			return mode
		})
	}
	mode := fi.Mode().Perm()
	if goos != "windows" && mode&0o111 == 0 {
		mode |= 0o755
	}
	return addFile(zw, name, mode, src, when)
}

// bundleExecutable 猜出 .app 里的主可执行文件，返回相对 bundle 根的斜杠路径。
func bundleExecutable(root string) string {
	if b, err := os.ReadFile(filepath.Join(root, "Contents", "Info.plist")); err == nil {
		if n := plistString(string(b), "CFBundleExecutable"); n != "" {
			return "Contents/MacOS/" + n
		}
	}
	base := filepath.Base(filepath.Clean(root))
	stem := strings.TrimSuffix(base, ".app")
	if stem == "" || stem == base {
		return "" // 目录不叫 .app，猜不出主程序叫什么，那就不动权限位。
	}
	return "Contents/MacOS/" + stem
}

// plistString 从 plist 文本里抠出 <key>key</key> 后面那个 <string> 值。
//
// 只认这一种写法，够用：CFBundleExecutable 是可执行文件的名字，Xcode、Wails、
// Tauri 生成的都是这个形状（中间没有嵌套的 <dict>）。抠不出来就返回空串，
// 让调用方退到按名字猜。
func plistString(s, key string) string {
	marker := "<key>" + key + "</key>"
	i := strings.Index(s, marker)
	if i < 0 {
		return ""
	}
	rest := s[i+len(marker):]
	open := strings.Index(rest, "<string>")
	if open < 0 {
		return ""
	}
	rest = rest[open+len("<string>"):]
	end := strings.Index(rest, "</string>")
	if end < 0 {
		return ""
	}
	name := strings.TrimSpace(rest[:end])
	if name == "" || strings.ContainsAny(name, `/\`) {
		return "" // 名字里带路径分隔符的一律不认。
	}
	return name
}

// sumsFor 生成 SHA256SUMS 的内容，路径相对包根（形如 `payload/AI Desk.app/…`
// 或 `tools/darwin_arm64/cot`），只覆盖常规文件 —— 符号链接不算，这一点要和
// stage.VerifySums 保持一致。工具链二进制也在覆盖范围内（D32）。
//
// 随包的 GUI-Setup（v3.13、D43）**不**在覆盖范围内：安装流程一个字节都不读它
// （它跑在安装之前，是它自己叫 gpm 干活的），把它算进去只会让每个包多校验一份
// 几十 MB 的东西。它和 install.sh / gpm / 清单是同一类：包根上的"信任起点"。
func sumsFor(dir string) ([]byte, int, error) {
	type entry struct{ path, sum string }
	var entries []entry

	for _, sub := range []string{manifest.PayloadDir, manifest.ToolsDir} {
		root := filepath.Join(dir, sub)
		if _, err := os.Stat(root); err != nil {
			// 没有 tools/ 是正常的：不带工具链的包。
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, 0, err
		}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			sum, err := stage.HashFile(p)
			if err != nil {
				return err
			}
			entries = append(entries, entry{filepath.ToSlash(rel), sum})
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
	}
	if len(entries) == 0 {
		return nil, 0, fmt.Errorf("%s/ 里一个常规文件都没有", manifest.PayloadDir)
	}

	// 按路径排序，而不是按 hash：出问题时人眼要能一行行对着文件看。
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		// 两个空格是 sha256sum 的格式，读回来的解析器只按第一段空白切分，
		// 所以路径里的空格（macOS 的 "AI Desk.app"）不会出问题。
		lines = append(lines, e.sum+"  "+e.path)
	}
	return []byte(strings.Join(lines, "\n") + "\n"), len(entries), nil
}

func addBytes(zw *zip.Writer, name string, mode fs.FileMode, data []byte, when time.Time) error {
	method := zip.Deflate
	if mode&fs.ModeSymlink != 0 {
		// 符号链接的内容是链接目标，几字节而已，压缩没有意义。
		method = zip.Store
	}
	h := &zip.FileHeader{Name: filepath.ToSlash(name), Method: method, Modified: when}
	h.SetMode(mode) // 只有调这个才会把 Unix 权限位写进 ExternalAttrs
	w, err := zw.CreateHeader(h)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func addFile(zw *zip.Writer, name string, mode fs.FileMode, src string, when time.Time) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return addBytes(zw, name, mode, b, when)
}

// addTree 把一棵目录树整个搬进 zip，保留权限位与符号链接。
//
// filepath.WalkDir 不会跟着符号链接走，正合需要：.app 里的链接该原样复制，
// 不能展开成它指向的那份副本。
func addTree(zw *zip.Writer, root, prefix string, when time.Time) error {
	return addTreeWith(zw, root, prefix, when, nil)
}

// addTreeWith 是 addTree 的带修正版：fix 不为 nil 时，每个常规文件的权限位
// 先过一遍它。搬 .app 时用它把主可执行文件的 x 位补回来，别的文件原样。
func addTreeWith(zw *zip.Writer, root, prefix string, when time.Time, fix func(rel string, mode fs.FileMode) fs.FileMode) error {
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("找不到 %s/", prefix)
		}
		return err
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := prefix
		if rel != "." {
			name = prefix + "/" + filepath.ToSlash(rel)
		}

		switch {
		case d.IsDir():
			if rel == "." {
				return nil
			}
			h := &zip.FileHeader{Name: name + "/", Method: zip.Store, Modified: when}
			h.SetMode(fs.ModeDir | 0o755)
			_, err := zw.CreateHeader(h)
			return err

		case d.Type()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return addBytes(zw, name, fs.ModeSymlink|0o777, []byte(target), when)

		case !d.Type().IsRegular():
			// 设备文件、命名管道之类既不常见也无法在 zip 里表达，跳过。
			return nil

		default:
			fi, err := d.Info()
			if err != nil {
				return err
			}
			mode := fi.Mode().Perm()
			if fix != nil {
				mode = fix(rel, mode)
			}
			return addFile(zw, name, mode, p, when)
		}
	})
}
