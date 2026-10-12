package pack

import (
	"archive/zip"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/qiuzhanghua/gpm-go/internal/stage"
)

// writeFile 写一个文件并显式设定权限位 —— 权限位正是这里要验的东西之一。
func writeFile(t *testing.T, path string, mode os.FileMode, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// assembly 造一个最小的「已装配目录」：一个带空格的 .app（macOS 上很常见），
// 里面既有可执行文件，也有符号链接。
func assembly(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "ad-manifest.yaml"), 0o644, `id: ai-desk
name: AI Desk
version: 1.2.3
entry:
  darwin:
    bundle: "AI Desk.app"
  windows:
    exe: AI Desk.exe
  linux:
    exe: ai-desk
launch:
  cmd: ad
  mode: activate
`)

	app := filepath.Join(dir, "payload", "AI Desk.app")
	writeFile(t, filepath.Join(app, "Contents", "Info.plist"), 0o644, "<plist/>\n")
	writeFile(t, filepath.Join(app, "Contents", "MacOS", "ai-desk"), 0o755, "#!/bin/sh\necho hi\n")
	writeFile(t, filepath.Join(app, "Contents", "Frameworks", "V1", "libx.dylib"), 0o644, "BINARY")

	// 另外两个平台各自的产物。同一个装配目录里放三份，正是这套设计的意思：
	// 清单描述形态，打包时按目标平台挑一份。
	writeFile(t, filepath.Join(dir, "payload", "AI Desk.exe"), 0o755, "MZ")
	writeFile(t, filepath.Join(dir, "payload", "ai-desk"), 0o755, "ELF")

	// .app 里出现符号链接是常态，而且它们必须原样保留：
	// 展开成副本会让包变大，跟着链接走还可能直接报错。
	link := filepath.Join(app, "Contents", "Frameworks", "Current")
	if err := os.Symlink("V1", link); err != nil {
		t.Fatal(err)
	}
	return dir
}

func names(t *testing.T, zipPath string) []string {
	t.Helper()
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var out []string
	for _, f := range r.File {
		out = append(out, f.Name)
	}
	return out
}

func has(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestBuildRoundTrip 是这里最重要的一个测试：打出来的包必须能被
// stage.Materialize 原样解回来，权限位、符号链接、SHA256SUMS 一样不差。
// 「打得开」和「解回来还是对的」是两回事。
func TestBuildRoundTrip(t *testing.T) {
	dir := assembly(t)
	out := filepath.Join(t.TempDir(), "out.zip")

	self := filepath.Join(t.TempDir(), "gpm")
	writeFile(t, self, 0o755, "FAKE-GPM")

	got, err := Build(Options{Dir: dir, Out: out, GOOS: "darwin", GOARCH: "arm64", Self: self})
	if err != nil {
		t.Fatal(err)
	}
	if got != out {
		t.Errorf("返回的路径 = %q，想要 %q", got, out)
	}

	ns := names(t, got)
	for _, want := range []string{
		InstallSH, InstallCMD, InstallPS1, "gpm", "ad-manifest.yaml", stage.SumsFile,
		"payload/AI Desk.app/Contents/Info.plist",
		"payload/AI Desk.app/Contents/MacOS/ai-desk",
	} {
		if !has(ns, want) {
			t.Errorf("包里少了 %q（实际有 %v）", want, ns)
		}
	}

	dst := filepath.Join(t.TempDir(), "unpacked")
	if err := stage.Materialize(got, dst); err != nil {
		t.Fatal(err)
	}

	// 校验值必须是「照着这个包算出来的」，所以解回来之后一定能对上。
	ok, err := stage.VerifySums(dst, "payload")
	if err != nil {
		t.Fatalf("SumsVerify 失败: %v", err)
	}
	if !ok {
		t.Fatal("VerifySums 说文件被改过，但它刚从 zip 里解出来")
	}

	exe := filepath.Join(dst, "payload", "AI Desk.app", "Contents", "MacOS", "ai-desk")
	fi, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0 {
		// Windows 没有可执行位这回事，os.Stat 一律报 0666。
		t.Errorf("可执行位丢了：%v", fi.Mode())
	}

	link := filepath.Join(dst, "payload", "AI Desk.app", "Contents", "Frameworks", "Current")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("符号链接没保留：%v", err)
	}
	if target != "V1" {
		t.Errorf("链接目标 = %q，想要 %q", target, "V1")
	}
}

// TestBuildEmbedsGpmExeOnWindows：Windows 上 install.cmd 调的是 gpm.exe，
// 包里就必须真的叫这个名字。这条在 macOS 上跑也能验证，不必等 CI。
func TestBuildEmbedsGpmExeOnWindows(t *testing.T) {
	dir := assembly(t)
	out := filepath.Join(t.TempDir(), "out.zip")
	self := filepath.Join(t.TempDir(), "gpm")
	writeFile(t, self, 0o755, "FAKE-GPM")

	if _, err := Build(Options{Dir: dir, Out: out, GOOS: "windows", GOARCH: "amd64", Self: self}); err != nil {
		t.Fatal(err)
	}
	ns := names(t, out)
	if !has(ns, "gpm.exe") {
		t.Errorf("Windows 包里没有 gpm.exe：%v", ns)
	}
	if has(ns, "gpm") {
		t.Error("Windows 包里不该有叫 gpm 的文件")
	}
}

// TestBuildSumsCoverEveryFile：SHA256SUMS 的作用是「包被人动过就要发现」，
// 所以它必须覆盖 payload/ 下的每一个常规文件。
func TestBuildSumsCoverEveryFile(t *testing.T) {
	dir := assembly(t)
	out := filepath.Join(t.TempDir(), "out.zip")
	self := filepath.Join(t.TempDir(), "gpm")
	writeFile(t, self, 0o755, "FAKE-GPM")

	if _, err := Build(Options{Dir: dir, Out: out, GOOS: "darwin", Self: self}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "unpacked")
	if err := stage.Materialize(out, dst); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(dst, stage.SumsFile))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, want := range []string{
		"payload/AI Desk.app/Contents/Info.plist",
		"payload/AI Desk.app/Contents/MacOS/ai-desk",
		"payload/AI Desk.app/Contents/Frameworks/V1/libx.dylib",
	} {
		if !strings.Contains(text, "  "+want+"\n") {
			t.Errorf("SHA256SUMS 里没有 %q：\n%s", want, text)
		}
	}
	// 符号链接不参与校验（和 stage.VerifySums 的约定一致）。
	if strings.Contains(text, "Frameworks/Current") {
		t.Errorf("符号链接不该出现在 SHA256SUMS 里：\n%s", text)
	}
}

func TestBuildRejectsMissingEntryForTargetOS(t *testing.T) {
	dir := assembly(t)
	// 只有 darwin 的清单：拿到 windows 上打包就该当场报错，
	// 而不是打出一个装上去跑不起来的包。
	writeFile(t, filepath.Join(dir, "ad-manifest.yaml"), 0o644, `id: ai-desk
name: AI Desk
version: 1.2.3
entry:
  darwin:
    bundle: "AI Desk.app"
launch:
  cmd: ad
  mode: activate
`)
	_, err := Build(Options{
		Dir:  dir,
		Out:  filepath.Join(t.TempDir(), "out.zip"),
		GOOS: "windows",
		Self: os.Args[0],
	})
	if err == nil {
		t.Fatal("windows 没有 entry，却打包成功了")
	}
}

// TestBuildLeavesNoHalfZip：打包中途失败时不能在磁盘上留一个半个 zip。
// 一个看起来打成功了的坏包，比一次明确的失败危险得多。
func TestBuildLeavesNoHalfZip(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "x-manifest.yaml"), 0o644, `id: x
name: X
version: 1
entry:
  darwin:
    bundle: X.app
`)
	// 有 manifest，没有 payload/ —— addTree 会失败。
	out := filepath.Join(t.TempDir(), "out.zip")
	if _, err := Build(Options{Dir: dir, Out: out, GOOS: "darwin", Self: os.Args[0]}); err == nil {
		t.Fatal("没有 payload/ 却打包成功了")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("失败后留下了 %s", out)
	}
}

// TestInstallScriptsAreSelfContained 守住两个入口脚本的关键性质：
// 它们不能把安装逻辑写在脚本里（那就等于每平台各维护一份）。
func TestInstallScriptsAreSelfContained(t *testing.T) {
	sh := InstallScript("", nil)
	if !strings.Contains(sh, `cd "$(dirname "$0")"`) {
		t.Error("install.sh 必须以自己的位置为工作目录，否则用户在任何目录下运行都会找错文件")
	}
	if !strings.Contains(sh, "gpm install .") {
		t.Error("install.sh 必须把安装交给 gpm")
	}
	for _, forbidden := range []string{"mkdir", "cp ", "sha256"} {
		if strings.Contains(sh, forbidden) {
			t.Errorf("install.sh 里不该出现 %q —— 安装逻辑属于 gpm", forbidden)
		}
	}

	cmd := InstallBatch("", nil)
	if !strings.Contains(cmd, "gpm.exe install .") {
		t.Error("install.cmd 必须调用 gpm.exe")
	}
	if strings.Contains(cmd, "powershell") || strings.Contains(cmd, "PowerShell") {
		t.Error("install.cmd 不该依赖 PowerShell：默认 ExecutionPolicy 会拦住它")
	}

	// 没给 --default-dir、清单也没有 requires 时，不能凭空编出一个安装根来 ——
	// 装到哪儿是打包方的决定，gpm 会落到平台数据目录/<简称>。
	for _, one := range []string{sh, cmd} {
		if strings.Contains(one, "--dir") {
			t.Errorf("既没给 --default-dir 也没有 requires，却出现了 --dir：\n%s", one)
		}
	}
}

// TestInstallBatchLineEndingsAndEncoding 钉住 install.cmd 的两个「必须」：
// 行尾只能是 CRLF，内容只能是 ASCII。
//
// 这不是洁癖。cmd.exe 按当前代码页逐行读批处理，而 UTF-8 的非 ASCII 字节在
// 别的代码页下解码时会把行尾的 LF 一起吞掉，于是注释行与下一行黏连、解码残渣
// 被当成命令去执行 —— 实测（OEM 936 的中文 Windows）用户每次双击 install.cmd
// 都会先看到一行 `'…' is not recognized as an internal or external command`。
// 2×2 对照：CRLF+中文干净、LF+ASCII 干净，只有 LF+非 ASCII 复现。install.sh
// 那边要求正好相反（CRLF 会让 `#!/bin/sh` 失效），所以两份脚本各钉一个测试，
// 免得日后有人「顺手统一」了行尾。
func TestInstallBatchLineEndingsAndEncoding(t *testing.T) {
	cases := []struct {
		name string
		got  string
	}{
		{"没给 --default-dir", InstallBatch("", nil)},
		{"--default-dir 是工具链的家", InstallBatch("~/cot", nil)},
		{"按 requires 定家", InstallBatch("", []string{"cot"})},
		{"--default-dir 是绝对路径", InstallBatch(`D:\apps\ad`, nil)},
	}
	for _, tc := range cases {
		if lf, crlf := strings.Count(tc.got, "\n"), strings.Count(tc.got, "\r\n"); lf != crlf {
			t.Errorf("%s：有 %d 个换行不是 CRLF —— cmd 会把注释行和下一行黏连", tc.name, lf-crlf)
		}
		if i := firstNonASCII(tc.got); i >= 0 {
			t.Errorf("%s：第 %d 个字节是 0x%02X —— 非 ASCII 在别的代码页下会变乱码，叠加 LF 还会被当成命令执行",
				tc.name, i, tc.got[i])
		}
		if !strings.Contains(tc.got, `set "RC=%ERRORLEVEL%"`) || !strings.Contains(tc.got, "exit /b %RC%") {
			t.Errorf("%s：install.cmd 没把 gpm 的退出码带出去 —— 末行若是 pause，装失败也会 exit 0", tc.name)
		}
	}

	// 反方向：install.sh 必须是 LF。
	if sh := InstallScript("~/cot", []string{"cot"}); strings.Contains(sh, "\r\n") {
		t.Error("install.sh 里出现了 CRLF：`#!/bin/sh` 会失效，unix 上直接跑不起来")
	}
}

// firstNonASCII 返回第一个非 ASCII 字节的下标，全 ASCII 时返回 -1。
// install.ps1（v3.15、D44）是第三条入口。下面几条钉住它必须长成的样子 ——
// 与 install.cmd 那两条「必须」正好互补：那边要 CRLF + 纯 ASCII（cmd.exe 的解析
// 怪癖），这边要 **UTF-8 with BOM**（PowerShell 5.1 读没有 BOM 的文件时按当前 ANSI
// 代码页解码，烘进来的中文家路径会**静默**变成乱码路径）+ CRLF；两边都不依赖对方。
func TestInstallPowerShellFacts(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"没给 --default-dir", InstallPowerShell("", nil)},
		{"--default-dir 是工具链的家", InstallPowerShell("~/cot", nil)},
		{"按 requires 定家", InstallPowerShell("", []string{"cot"})},
		{"--default-dir 是绝对路径", InstallPowerShell(`D:\apps\ad`, nil)},
		{"烘了中文路径", InstallPowerShell(`C:\用户\cot`, nil)},
	}
	for _, tc := range cases {
		// 1) BOM 必须在最前面
		if !bytes.HasPrefix([]byte(tc.body), []byte("\xef\xbb\xbf")) {
			t.Errorf("%s：install.ps1 开头没有 UTF-8 BOM —— 5.1 会把中文家路径读成乱码", tc.name)
		}
		// 2) 行尾只能是 CRLF
		if nl, crlf := strings.Count(tc.body, "\n"), strings.Count(tc.body, "\r\n"); nl != crlf {
			t.Errorf("%s：有 %d 个 LF 但不是 CRLF（CRLF=%d）", tc.name, nl, crlf)
		}
		// 3) 不许有 param 块：有它，--dir 就会被当成参数名去绑定
		if strings.Contains(tc.body, "param(") {
			t.Errorf("%s：install.ps1 里出现了 param( —— --dir / --yes 就不再原样进 $args", tc.name)
		}
		// 4) 参数原样转交，位置在 gpm install . 与烘进去的 --dir 之后
		if n := strings.Count(tc.body, "@args"); n != 1 {
			t.Errorf("%s：应当恰有一处 @args，实际 %d 处", tc.name, n)
		}
		at := strings.Index(tc.body, "@args")
		if gi := strings.Index(tc.body, `gpm.exe" install .`); gi < 0 || gi > at {
			t.Errorf("%s：@args 必须跟在 gpm.exe install . 之后", tc.name)
		}
		if d := strings.Index(tc.body, "--dir "); d >= 0 && d > at {
			t.Errorf("%s：--dir 必须出现在 @args 之前，否则用户传的家盖不住默认值", tc.name)
		}
		// 5) 退出码：末行就是它；「跑不起来」那条路也要有守卫，不能静默 exit 0
		if !strings.HasSuffix(strings.TrimRight(tc.body, "\r\n"), "exit $LASTEXITCODE") {
			t.Errorf("%s：install.ps1 的末行应当是 exit $LASTEXITCODE：\n%s", tc.name, tc.body)
		}
		if strings.Count(tc.body, "exit $LASTEXITCODE") < 2 || !strings.Contains(tc.body, "if ($LASTEXITCODE)") {
			t.Errorf("%s：catch 里缺「拿不到退出码就 exit 1」的守卫（可执行文件找不到时 $LASTEXITCODE 是空的）：\n%s", tc.name, tc.body)
		}
		// 6) 切到自己的目录（gpm install . 的 . 是当前目录）+ 不暂停
		if !strings.Contains(tc.body, "Set-Location -LiteralPath $PSScriptRoot") {
			t.Errorf("%s：没有切到脚本自己的目录，gpm install . 会装错东西", tc.name)
		}
		if strings.Contains(tc.body, "Read-Host") || strings.Contains(tc.body, "pause") {
			t.Errorf("%s：install.ps1 不该有 pause / Read-Host —— 要按任意键的是 install.cmd", tc.name)
		}
	}
}

// 目录烘法：与 install.cmd 同一套规矩，只是写法换成 PowerShell 的。
func TestInstallPowerShellBakesDefaultDir(t *testing.T) {
	ps1 := InstallPowerShell("~/cot", nil)
	for _, want := range []string{`$env:USERPROFILE\cot`, `if (-not $env:COT_HOME)`, `--dir $env:COT_HOME`} {
		if !strings.Contains(ps1, want) {
			t.Errorf("install.ps1 里没找到 %q：\n%s", want, ps1)
		}
	}
	if strings.Contains(ps1, "%USERPROFILE%") {
		t.Errorf("install.ps1 里不该出现 cmd 的 %%USERPROFILE%%（PowerShell 不展开它）：\n%s", ps1)
	}
	if got := InstallPowerShell("", []string{"cot"}); !strings.Contains(got, `--dir $env:COT_HOME`) {
		t.Errorf("install.ps1 没按 requires 定家：\n%s", got)
	}
	if got := InstallPowerShell(`D:\apps\ad`, nil); !strings.Contains(got, `--dir "D:\apps\ad"`) {
		t.Errorf("install.ps1 没把绝对路径原样烘进去：\n%s", got)
	}
	if got := InstallPowerShell("", nil); strings.Contains(got, "--dir") {
		t.Errorf("清单没有 requires、打包方也没给 --default-dir 时不该传 --dir：\n%s", got)
	}
}

// 光看字符串不够：真拿 Windows PowerShell 5.1 读一遍、解析一遍。
//
// 一次验两件事：① 语法能被 5.1 的解析器收下（模板少括号、少引号都会在这里现形）；
// ② BOM 真的让 5.1 按 UTF-8 读 —— 烘一个中文路径进去，让 5.1 读原文找那个哨兵，
// 没有 BOM 时它会读成乱码（同一台机器上 2×2 实测过）。
func TestInstallPowerShellReadsInRealWindowsPowerShell(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("这条要真的 Windows PowerShell 5.1")
	}
	sh, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("没装 Windows PowerShell")
	}
	const cjk = `C:\用户\张三\cot`
	body := InstallPowerShell(cjk, nil)
	p := filepath.Join(t.TempDir(), InstallPS1)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `$errs = $null
[void][System.Management.Automation.Language.Parser]::ParseFile($env:GPM_PS1, [ref]$null, [ref]$errs)
if ($errs.Count) { $errs | ForEach-Object { Write-Host $_.Message }; exit 1 }
$t = Get-Content -LiteralPath $env:GPM_PS1 -Raw
if (-not $t.Contains($env:GPM_SENTINEL)) { Write-Host "读不到哨兵：BOM 没起作用"; exit 2 }
exit 0`
	cmd := exec.Command(sh, "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(), "GPM_PS1="+p, "GPM_SENTINEL="+cjk)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Windows PowerShell 5.1 说这个脚本有问题：%v\n%s\n脚本原文：\n%s", err, out, body)
	}
}

func firstNonASCII(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7f {
			return i
		}
	}
	return -1
}

// TestDefaultDirIsBakedIntoInstallers 守住 --default-dir 的职责：把「装到
// 哪儿」写进脚本。工具链的家（~/cot）写成 `${COT_HOME:-$HOME/cot}` ——
// 脚本里给的是默认值，用户在 shell 里设的家仍然算数。
func TestDefaultDirIsBakedIntoInstallers(t *testing.T) {
	sh := InstallScript("~/cot", nil)
	if !strings.Contains(sh, `--dir "${COT_HOME:-$HOME/cot}"`) {
		t.Errorf("install.sh 没把 ~/cot 写成 ${COT_HOME:-$HOME/cot}：\n%s", sh)
	}

	cmd := InstallBatch("~/cot", nil)
	if !strings.Contains(cmd, `set "COT_HOME=%USERPROFILE%\cot"`) {
		t.Errorf("install.cmd 没把 ~ 展开成 %%USERPROFILE%%：\n%s", cmd)
	}
	if !strings.Contains(cmd, `if not defined COT_HOME`) {
		t.Errorf("install.cmd 必须在 COT_HOME 未定义时才设默认值：\n%s", cmd)
	}
	if !strings.Contains(cmd, `--dir "%COT_HOME%"`) {
		t.Errorf("install.cmd 没把 --dir 指到 COT_HOME：\n%s", cmd)
	}

	// 不是工具链的家（那两家之外的路径）原样照抄，也不替它发明环境变量。
	if got := InstallScript("/opt/apps", nil); !strings.Contains(got, `--dir "/opt/apps"`) {
		t.Errorf("绝对路径 --default-dir 被改动了：\n%s", got)
	}
	if got := InstallScript("~/ad", nil); !strings.Contains(got, `--dir "$HOME/ad"`) {
		t.Errorf("非工具链的家不该被写成环境变量：\n%s", got)
	}
}

// 没给 --default-dir 时，装到哪儿由清单的 requires 决定。
func TestRequiresDrivesInstallerDir(t *testing.T) {
	sh := InstallScript("", []string{"cot"})
	if !strings.Contains(sh, `--dir "${COT_HOME:-$HOME/cot}"`) {
		t.Errorf("install.sh 没按 requires 定家：\n%s", sh)
	}
	if !strings.Contains(InstallScript("", []string{"tdp"}), `--dir "${TDP_HOME:-$HOME/tdp}"`) {
		t.Errorf("requires: [tdp] 的家不对：\n%s", sh)
	}

	cmd := InstallBatch("", []string{"cot"})
	if !strings.Contains(cmd, `--dir "%COT_HOME%"`) {
		t.Errorf("install.cmd 没按 requires 定家：\n%s", cmd)
	}
	if !strings.Contains(cmd, `set "COT_HOME=%USERPROFILE%\cot"`) {
		t.Errorf("install.cmd 的兜底值应当是 %%USERPROFILE%%\\cot，而不是 %%COT_HOME%%：\n%s", cmd)
	}
}

// TestInstallersForwardArgs 钉住 v3.12（D42）的那两行：入口脚本必须把用户给
// 的参数原样转交给 gpm。
//
// 起因是一处真实的体验断裂：gpm 自己会在放弃 PATH 集成时打印一行可照抄的
// `./gpm install . --yes`，而用户站在解压出来的目录里更顺手的写法是
// `./install.sh --yes`。不转交的话这个参数被静默吞掉 —— 交互终端里退化成
// 一个 y/N 询问，非交互（CI）里干脆跳过 PATH 集成，而退出码仍是 0。
func TestInstallersForwardArgs(t *testing.T) {
	for _, sh := range []string{
		InstallScript("", nil),
		InstallScript("~/cot", []string{"cot"}),
		InstallScript("/opt/apps", []string{"cot"}),
	} {
		if n := strings.Count(sh, `"$@"`); n != 1 {
			t.Errorf(`install.sh 里应当恰有一处 "$@"，实际 %d 处：\n%s`, n, sh)
		}
		// 不带引号的 `$@` 会把带空格的参数拆成多个；`"$@"` 才是逐个原样转交。
		if strings.Contains(strings.ReplaceAll(sh, `"$@"`, ""), "$@") {
			t.Errorf("install.sh 里有不带引号的 $@：带空格的路径会被拆开：\n%s", sh)
		}
		// 必须排在 gpm 与烘进去的 --dir 之后：flag 取后出现的那一个，所以用户
		// 自己的 --dir / --yes / --with 才盖得住脚本里的默认值。
		at := strings.Index(sh, `"$@"`)
		if gpmAt := strings.Index(sh, "gpm install ."); gpmAt < 0 || at < gpmAt {
			t.Errorf(`install.sh 里 "$@" 必须出现在 gpm install . 之后：\n%s`, sh)
		}
		if dirAt := strings.Index(sh, "--dir"); dirAt >= 0 && at < dirAt {
			t.Errorf(`install.sh 里 "$@" 必须出现在 --dir 之后：\n%s`, sh)
		}
		if !strings.HasSuffix(strings.TrimRight(sh, "\n"), `"$@"`) {
			t.Errorf(`install.sh 的末行应当是 exec ./gpm install .… "$@"：\n%s`, sh)
		}
	}

	for _, cmd := range []string{
		InstallBatch("", nil),
		InstallBatch("~/cot", nil),
		InstallBatch("", []string{"tdp"}),
	} {
		if n := strings.Count(cmd, "%*"); n != 1 {
			t.Errorf("install.cmd 里应当恰有一处 %%*，实际 %d 处：\n%s", n, cmd)
		}
		if strings.Contains(cmd, `"%*"`) {
			t.Errorf(`install.cmd 里的 %%* 不该带引号 —— 那会把所有参数粘成一个：\n%s`, cmd)
		}
		if gpmAt, at := strings.Index(cmd, "gpm.exe install ."), strings.Index(cmd, "%*"); gpmAt < 0 || at < gpmAt {
			t.Errorf("install.cmd 里的 %%* 必须出现在 gpm.exe install . 之后：\n%s", cmd)
		}
	}
}

// 带工具链的包：tools/ 也要进 zip、也要进 SHA256SUMS（D32）。
func TestBuildPacksToolsAndCoversThem(t *testing.T) {
	dir := assembly(t)
	writeFile(t, filepath.Join(dir, "ad-manifest.yaml"), 0o644, `id: ai-desk
name: AI Desk
version: 1.2.3
requires: [cot]
entry:
  darwin:
    bundle: "AI Desk.app"
launch:
  cmd: ad
`)
	tool := filepath.Join(dir, "tools", "darwin_arm64", "cot")
	writeFile(t, tool, 0o755, "#!/bin/sh\necho cot\n")

	out := filepath.Join(t.TempDir(), "out.zip")
	self := filepath.Join(t.TempDir(), "gpm")
	writeFile(t, self, 0o755, "FAKE-GPM")

	if _, err := Build(Options{Dir: dir, Out: out, GOOS: "darwin", GOARCH: "arm64", Self: self}); err != nil {
		t.Fatal(err)
	}
	if ns := names(t, out); !has(ns, "tools/darwin_arm64/cot") {
		t.Errorf("包里没有工具链：%v", ns)
	}

	dst := filepath.Join(t.TempDir(), "unpacked")
	if err := stage.Materialize(out, dst); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst, stage.SumsFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "  tools/darwin_arm64/cot\n") {
		t.Errorf("工具链没进 SHA256SUMS：\n%s", b)
	}
	// 安装时会照 payload/ 与 tools/ 两个目录查覆盖。
	if ok, err := stage.VerifySums(dst, "payload", "tools"); err != nil || !ok {
		t.Fatalf("VerifySums(payload, tools) = %v/%v", ok, err)
	}
	// 安装脚本要把 requires 的家烘出来。
	sh, err := os.ReadFile(filepath.Join(dst, InstallSH))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sh), `--dir "${COT_HOME:-$HOME/cot}"`) {
		t.Errorf("install.sh 没按 requires 定家：\n%s", sh)
	}
}

// setupAssembly 造一个声明了 setup: 的装配目录：GUI-Setup 躺在**包根**
// （和 install.sh 同级），而清单里 entry 走的那份还在 payload/ 下。
//
// 内层的可执行文件故意写成 0644：打包时不该原样照抄这个权限位。
func setupAssembly(t *testing.T) string {
	t.Helper()
	dir := assembly(t)

	writeFile(t, filepath.Join(dir, "ad-manifest.yaml"), 0o644, `id: ai-desk
name: AI Desk
version: 1.2.3
entry:
  darwin:
    bundle: "AI Desk.app"
  windows:
    exe: AI Desk.exe
setup:
  darwin:
    bundle: GUI-Setup.app
  windows:
    exe: GUI-Setup.exe
launch:
  cmd: ad
  mode: activate
`)

	writeFile(t, filepath.Join(dir, "GUI-Setup.app", "Contents", "Info.plist"), 0o644, `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>CFBundleExecutable</key>
	<string>GUI-Setup</string>
</dict>
</plist>
`)
	writeFile(t, filepath.Join(dir, "GUI-Setup.app", "Contents", "MacOS", "GUI-Setup"), 0o644, "#!/bin/sh\necho setup\n")
	writeFile(t, filepath.Join(dir, "GUI-Setup.exe"), 0o644, "MZ-setup")
	return dir
}

// index 返回名字在 zip 里的次序，找不到返回 -1。
func index(names []string, want string) int {
	for i, n := range names {
		if n == want {
			return i
		}
	}
	return -1
}

// TestBuildPacksSetupAtTopLevel 钉住 v3.13（D43）的交付形态：GUI-Setup 进
// zip **顶层**（不经 payload/），x 位补齐，且不进 SHA256SUMS。
//
// 三条都不是小事：进 payload/ 会被当成「应用的内容」装进家里、还会跟着账本走；
// x 位丢了用户双击没反应（Windows 的资源管理器解压、网盘中转都会丢这一位）；
// 进 SHA256SUMS 就要每个包多校验几十 MB，而安装流程一个字节都不读它。
func TestBuildPacksSetupAtTopLevel(t *testing.T) {
	dir := setupAssembly(t)
	out := filepath.Join(t.TempDir(), "out.zip")
	self := filepath.Join(t.TempDir(), "gpm")
	writeFile(t, self, 0o755, "FAKE-GPM")

	if _, err := Build(Options{
		Dir: dir, Out: out, GOOS: "darwin", GOARCH: "arm64", Self: self,
		Setup: filepath.Join(dir, "GUI-Setup.app"),
	}); err != nil {
		t.Fatal(err)
	}

	ns := names(t, out)
	const want = "GUI-Setup.app/Contents/MacOS/GUI-Setup"
	if !has(ns, want) {
		t.Errorf("包里没有 %q：%v", want, ns)
	}
	if has(ns, "payload/GUI-Setup.app/Contents/MacOS/GUI-Setup") {
		t.Error("GUI-Setup 被塞进了 payload/：那是「要装进家里的东西」，而 GUI-Setup 只在解压目录里跑一次")
	}

	// 位置：紧挨着两个安装脚本，排在 gpm 之前 —— 解压出来第一眼看见的就是它。
	at, gpmAt, cmdAt := index(ns, "GUI-Setup.app/Contents/Info.plist"), index(ns, "gpm"), index(ns, InstallCMD)
	if at < 0 || cmdAt < 0 || gpmAt < 0 || at < cmdAt || at > gpmAt {
		t.Errorf("GUI-Setup 的位置不对（install.cmd=%d, setup=%d, gpm=%d）：%v", cmdAt, at, gpmAt, ns)
	}

	dst := filepath.Join(t.TempDir(), "unpacked")
	if err := stage.Materialize(out, dst); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dst, want))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("GUI-Setup 的可执行位没补上：%v —— 双击会没反应", fi.Mode())
	}
	// 补 x 位是"只补主程序"，不是给整棵树加可执行 —— plist 不该跟着变。
	pl, err := os.Stat(filepath.Join(dst, "GUI-Setup.app", "Contents", "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if pl.Mode().Perm()&0o111 != 0 {
		t.Errorf("Info.plist 被标成了可执行：%v", pl.Mode())
	}

	b, err := os.ReadFile(filepath.Join(dst, stage.SumsFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "GUI-Setup") {
		t.Errorf("GUI-Setup 不该进 SHA256SUMS（安装流程不读它）：\n%s", b)
	}
	// 它不走 payload/，所以核对安装内容时依然对得上。
	if ok, err := stage.VerifySums(dst, "payload"); err != nil || !ok {
		t.Fatalf("VerifySums(payload) = %v/%v", ok, err)
	}
}

// TestBuildPacksSetupExeOnWindows：Windows 那边 setup 是单个 .exe，也要落在顶层。
func TestBuildPacksSetupExeOnWindows(t *testing.T) {
	dir := setupAssembly(t)
	out := filepath.Join(t.TempDir(), "out.zip")
	self := filepath.Join(t.TempDir(), "gpm")
	writeFile(t, self, 0o755, "FAKE-GPM")

	if _, err := Build(Options{
		Dir: dir, Out: out, GOOS: "windows", GOARCH: "amd64", Self: self,
		Setup: filepath.Join(dir, "GUI-Setup.exe"),
	}); err != nil {
		t.Fatal(err)
	}
	if ns := names(t, out); !has(ns, "GUI-Setup.exe") {
		t.Errorf("Windows 包里没有 GUI-Setup.exe：%v", ns)
	}
}

// TestBuildSetupMustAgreeWithManifest 守住清单与命令行的那组一致性检查。
//
// 每一条都对应一种真实的坏包：清单说有、包里没有（用户双击 .app 打不开）；
// 命令行给了、清单没说（GUI-Setup 自己按清单找不到自己是谁）；名字对不上
// （清单在撒谎）；bundle/exe 形态反了（把 .app 声明成 exe）。
func TestBuildSetupMustAgreeWithManifest(t *testing.T) {
	fakeSelf := func(t *testing.T) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "gpm")
		writeFile(t, p, 0o755, "FAKE-GPM")
		return p
	}

	t.Run("清单声明了却没给 --setup", func(t *testing.T) {
		dir := setupAssembly(t)
		out := filepath.Join(t.TempDir(), "out.zip")
		_, err := Build(Options{Dir: dir, Out: out, GOOS: "darwin", Self: fakeSelf(t)})
		if err == nil {
			t.Fatal("清单声明了 setup 却打包成功了 —— 发出去的包里没有 GUI-Setup")
		}
		if !strings.Contains(err.Error(), "--setup") {
			t.Errorf("报错得告诉打包方怎么办：%v", err)
		}
		if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
			t.Errorf("失败后留下了 %s", out)
		}
	})

	t.Run("给了 --setup 但清单没声明", func(t *testing.T) {
		dir := assembly(t) // 这份清单没有 setup 段
		out := filepath.Join(t.TempDir(), "out.zip")
		_, err := Build(Options{
			Dir: dir, Out: out, GOOS: "darwin", Self: fakeSelf(t),
			Setup: filepath.Join(dir, "GUI-Setup.app"),
		})
		if err == nil {
			t.Fatal("清单没声明 setup 却打包成功了")
		}
		if !strings.Contains(err.Error(), "setup") {
			t.Errorf("报错得点出是清单里没声明：%v", err)
		}
	})

	t.Run("名字对不上", func(t *testing.T) {
		dir := setupAssembly(t)
		other := filepath.Join(t.TempDir(), "Other.app")
		if err := os.MkdirAll(other, 0o755); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(t.TempDir(), "out.zip")
		_, err := Build(Options{
			Dir: dir, Out: out, GOOS: "darwin", Self: fakeSelf(t), Setup: other,
		})
		if err == nil {
			t.Fatal("清单写 GUI-Setup.app、给的是 Other.app，却打包成功了")
		}
		if !strings.Contains(err.Error(), "GUI-Setup.app") {
			t.Errorf("报错得说清清单里那个名字：%v", err)
		}
	})

	t.Run("形态反了：bundle 给了文件", func(t *testing.T) {
		dir := setupAssembly(t)
		file := filepath.Join(t.TempDir(), "GUI-Setup.app")
		writeFile(t, file, 0o755, "not a bundle")
		out := filepath.Join(t.TempDir(), "out.zip")
		if _, err := Build(Options{
			Dir: dir, Out: out, GOOS: "darwin", Self: fakeSelf(t), Setup: file,
		}); err == nil {
			t.Fatal("bundle 声明配一个普通文件，却打包成功了")
		}
	})

	t.Run("形态反了：exe 给了目录", func(t *testing.T) {
		dir := setupAssembly(t)
		if _, err := Build(Options{
			Dir: dir, Out: filepath.Join(t.TempDir(), "out.zip"), GOOS: "windows", Self: fakeSelf(t),
			Setup: filepath.Join(dir, "GUI-Setup.app"),
		}); err == nil {
			t.Fatal("exe 声明配一个目录，却打包成功了")
		}
	})

	t.Run("清单的 setup 没有这个平台", func(t *testing.T) {
		dir := setupAssembly(t)
		writeFile(t, filepath.Join(dir, "ad-manifest.yaml"), 0o644, `id: ai-desk
name: AI Desk
version: 1.2.3
entry:
  darwin:
    bundle: "AI Desk.app"
  linux:
    exe: ai-desk
setup:
  darwin:
    bundle: GUI-Setup.app
launch:
  cmd: ad
`)
		if _, err := Build(Options{
			Dir: dir, Out: filepath.Join(t.TempDir(), "out.zip"), GOOS: "linux", Self: fakeSelf(t),
		}); err == nil {
			t.Fatal("清单的 setup 里没有 linux，却给 linux 打包成功了")
		}
	})

	t.Run("setup 路径不能越出包根", func(t *testing.T) {
		dir := setupAssembly(t)
		writeFile(t, filepath.Join(dir, "ad-manifest.yaml"), 0o644, `id: ai-desk
name: AI Desk
version: 1.2.3
entry:
  darwin:
    bundle: "AI Desk.app"
setup:
  darwin:
    bundle: ../../GUI-Setup.app
launch:
  cmd: ad
`)
		if _, err := Build(Options{
			Dir: dir, Out: filepath.Join(t.TempDir(), "out.zip"), GOOS: "darwin", Self: fakeSelf(t),
		}); err == nil {
			t.Fatal("setup 路径越出了包根，却打包成功了")
		}
	})
}

// TestBuildSetupFixesOnlyTheMainExecutable：.app 里 0644 的文件不止主程序一个，
// 补 x 位不能一把抓 —— 只认 Info.plist 里 CFBundleExecutable 点名的那个。
//
// 这里刻意让 bundle 名（Setup Tool.app）与主程序名（gui-setup）不一致：
// 按目录名去猜会猜错，只有真读 plist 才对得上。
func TestBuildSetupFixesOnlyTheMainExecutable(t *testing.T) {
	dir := assembly(t)
	writeFile(t, filepath.Join(dir, "ad-manifest.yaml"), 0o644, `id: ai-desk
name: AI Desk
version: 1.2.3
entry:
  darwin:
    bundle: "AI Desk.app"
setup:
  darwin:
    bundle: "Setup Tool.app"
launch:
  cmd: ad
`)

	bundle := filepath.Join(dir, "Setup Tool.app")
	writeFile(t, filepath.Join(bundle, "Contents", "Info.plist"), 0o644, `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>CFBundleExecutable</key>
	<string>gui-setup</string>
</dict>
</plist>
`)
	writeFile(t, filepath.Join(bundle, "Contents", "MacOS", "gui-setup"), 0o644, "#!/bin/sh\n")
	writeFile(t, filepath.Join(bundle, "Contents", "Resources", "helper"), 0o644, "resource\n")

	out := filepath.Join(t.TempDir(), "out.zip")
	self := filepath.Join(t.TempDir(), "gpm")
	writeFile(t, self, 0o755, "FAKE-GPM")
	if _, err := Build(Options{
		Dir: dir, Out: out, GOOS: "darwin", Self: self, Setup: bundle,
	}); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "unpacked")
	if err := stage.Materialize(out, dst); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		rel    string
		wantX  bool
		reason string
	}{
		{filepath.Join("Setup Tool.app", "Contents", "MacOS", "gui-setup"), true, "plist 点名的就是它"},
		{filepath.Join("Setup Tool.app", "Contents", "Resources", "helper"), false, "资源文件"},
		{filepath.Join("Setup Tool.app", "Contents", "Info.plist"), false, "清单"},
	} {
		fi, err := os.Stat(filepath.Join(dst, tc.rel))
		if err != nil {
			t.Fatal(err)
		}
		// Windows 的文件系统没有 Unix 权限位，Materialize 出来一律是 0666，
		// 和 pack_test.go 里另外两处一样跳过；zip 里存的 mode 在别处断言。
		if runtime.GOOS == "windows" {
			continue
		}
		if gotX := fi.Mode().Perm()&0o111 != 0; gotX != tc.wantX {
			t.Errorf("%s（%s）的 x 位：得到 %v，想要 %v", tc.rel, tc.reason, fi.Mode(), tc.wantX)
		}
	}
}
