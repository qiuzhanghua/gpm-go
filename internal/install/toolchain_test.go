package install

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/qiuzhanghua/gpm-go/internal/home"
	"github.com/qiuzhanghua/gpm-go/internal/ledger"
)

// ledgerPath 是这个家的账本：v3.7 起按家命名，<家目录名>-state.json。
func ledgerPath(root string) string { return (&home.Home{Root: root}).LedgerPath() }

// 工具链自举（D32 / §2.5.1）用的是运行时平台的目录名。
func toolchainDir() string { return runtime.GOOS + "_" + runtime.GOARCH }

// toolchainAssembly 造一个"带工具链"的包：清单声明 requires: [cot]，
// zip 里自带 tools/<os>_<arch>/cot。
//
// 那个 cot 是个脚本（Windows 上跳过这套用例）：它把参数记到 $HOME 下，
// 再照 cot 自己的样子往家里放一个 bin/cot —— 后半句是为了验证 D34：
// 工具链的东西不进 gpm 的账本，卸载也不动它。
func toolchainAssembly(t *testing.T, id, short, requires string, script string) string {
	t.Helper()
	asm := t.TempDir()
	writePayload(t, filepath.Join(asm, short+"-manifest.yaml"), `id: `+id+`
name: Demo
version: 0.1.0
requires: [`+requires+`]
entry:
  `+runtime.GOOS+`:
    exe: demo
launch:
  cmd: `+short+`
`, 0o644)
	writePayload(t, filepath.Join(asm, "payload", "demo"), "#!/bin/sh\nexit 0\n", 0o755)
	if script != "" {
		// Windows 上 gpm 找的是 cot.exe（install 的报错也是这么写的）。
		name := requires
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		p := filepath.Join(asm, "tools", toolchainDir(), name)
		writePayload(t, p, script, 0o755)
		// SHA256SUMS 要覆盖工具链；留空则走"没有 SHA256SUMS"的警告路径，
		// 这里干脆跳过校验，让用例只盯自举这件事。
	}
	return asm
}

// fakeHome 把 HOME 挪到临时目录：启动器、Applications 软链、~/cot 都不碰真家。
//
// USERPROFILE 必须一起设：Windows 上 os.UserHomeDir() 读的是它，不认 HOME ——
// 只设 HOME 会让隔离**静默失效**，用例把家解析成开发机真实的 ~/cot。实测后果是
// 「包里没有工具链就应当拒绝」这条用例在真家里找到真的 bin\cot.exe，于是跳过
// 自举、装成功了，还在真家里留下账本与启动器。
func fakeHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("COT_HOME", "")
	t.Setenv("TDP_HOME", "")
	return h
}

// installHome 是这次安装该落到的家：清单写了 requires: [cot]，
// 用户没设 COT_HOME，于是就是 ~/cot。
func installHome(fake string) string { return filepath.Join(fake, "cot") }

// TestInstallBootstrapsToolchain 是 M10 的主线：装 GUI 应用时先跑包里自带的
// 工具链（`cot i -s <家>`），再装应用本身，最后生成注入了工具链环境的启动器。
func TestInstallBootstrapsToolchain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("这套用例用 shell 脚本假装工具链")
	}
	home := fakeHome(t)
	root := installHome(home)

	// 假 cot：把参数记下来，再往家里放一个 bin/cot（工具链自己的东西）。
	// 家是第三个参数（`cot i -s <家>`），不是环境变量 —— cot 自己会认它。
	script := `#!/bin/sh
set -eu
home="$3"
printf '%s\n' "$@" > "$HOME/cot-args.txt"
mkdir -p "$home/bin"
printf '#!/bin/sh\necho cot\n' > "$home/bin/cot"
chmod +x "$home/bin/cot"
`
	asm := toolchainAssembly(t, "ai-desk", "ad", "cot", script)

	var log strings.Builder
	if err := Install(asm, Options{Yes: true, NoPath: true, Out: &log}); err != nil {
		t.Fatalf("安装失败：%v\n%s", err, log.String())
	}

	// 1) 工具链被真的跑了一次，参数是 `i -s <家>`。
	args, err := os.ReadFile(filepath.Join(home, "cot-args.txt"))
	if err != nil {
		t.Fatalf("没跑工具链：%v\n%s", err, log.String())
	}
	if got, want := string(args), "i\n-s\n"+root+"\n"; got != want {
		t.Errorf("工具链参数 = %q，想要 %q", got, want)
	}

	// 2) 应用照常落地并生成了启动器。
	launcher := filepath.Join(root, "bin", "ad")
	body, err := os.ReadFile(launcher)
	if err != nil {
		t.Fatalf("没有启动器：%v", err)
	}
	if !strings.Contains(string(body), "COT_HOME='"+root+"'") {
		t.Errorf("启动器没注入 COT_HOME：\n%s", body)
	}
	if !strings.Contains(string(body), "exec ") {
		t.Errorf("启动器没有 exec：\n%s", body)
	}

	// 3) 账本只记应用，不记工具链（D34）。
	led, err := ledger.Load(ledgerPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if len(led.Packages) != 1 {
		t.Fatalf("账本里有 %d 个包，想要 1 个：%+v", len(led.Packages), led.Packages)
	}
	if p := led.Packages[0]; p.ID != "ai-desk" || p.Cmd != "ad" {
		t.Errorf("账本里的包不对：%+v", p)
	}

	// 4) 卸载把应用清干净，工具链的东西留在原地（D34）。
	if err := Uninstall("ai-desk", UninstallOptions{Dir: root, Force: false, Yes: true, Out: io.Discard}); err != nil {
		t.Fatalf("卸载失败：%v", err)
	}
	if _, err := os.Stat(launcher); !os.IsNotExist(err) {
		t.Errorf("卸载后启动器还在：%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "bin", "cot")); err != nil {
		t.Errorf("卸载顺手删掉了工具链的东西：%v", err)
	}
}

// requires 说了要带工具链，包里却没有 —— 必须当场拒绝，而不是装出个
// 命令跑不起来的应用（F4 / A11 的反面）。
func TestInstallRejectsMissingToolchain(t *testing.T) {
	home := fakeHome(t)
	root := installHome(home)
	asm := toolchainAssembly(t, "ai-desk", "ad", "cot", "")

	err := Install(asm, Options{Yes: true, NoPath: true, Out: io.Discard})
	if err == nil {
		t.Fatal("包里没有工链却装成功了")
	}
	if !strings.Contains(err.Error(), "requires") {
		t.Errorf("报错没说清是 requires 的事：%v", err)
	}
	// 什么都没装成：这个家是这次刚建出来的，就该被收回去（F3 / A6）。
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Errorf("失败后留下了 %s：%v", root, statErr)
	}
}

// 自举那一步失败时不能留下半个应用。
func TestInstallRollsBackWhenToolchainFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("这套用例用 shell 脚本假装工具链")
	}
	home := fakeHome(t)
	root := installHome(home)
	asm := toolchainAssembly(t, "ai-desk", "ad", "cot", "#!/bin/sh\nexit 3\n")

	err := Install(asm, Options{Yes: true, NoPath: true, Out: io.Discard})
	if err == nil {
		t.Fatal("工具链退出码是 3，却装成功了")
	}
	if !strings.Contains(err.Error(), "cot") {
		t.Errorf("报错没提是哪家工具链：%v", err)
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Errorf("失败后留下了 %s：%v", root, statErr)
	}
}

// --with "" 表示这一次谁都不装：清单里的 requires 被覆盖掉。
func TestInstallWithEmptyOverridesRequires(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("这套用例用 shell 脚本假装工具链")
	}
	home := fakeHome(t)
	asm := toolchainAssembly(t, "ai-desk", "ad", "cot", "#!/bin/sh\nprintf ran > \"$HOME/ran\"\n")

	if err := Install(asm, Options{Yes: true, NoPath: true, Out: io.Discard, With: []string{}}); err != nil {
		t.Fatalf("安装失败：%v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "ran")); !os.IsNotExist(err) {
		t.Errorf("--with 空串却还是跑了工具链：%v", err)
	}
}

// 别人的 bin/ad 不能覆盖（FR-21 / A9）：认那行生成标记，不认路径。
// 这条闸跟工具链无关，所以清单不带 requires、家用 --dir 钉住 —— 这样三条腿都能跑。
func TestInstallRefusesForeignLauncher(t *testing.T) {
	home := fakeHome(t)
	root := installHome(home)
	asm := toolchainAssembly(t, "ai-desk", "ad", "", "")

	name := "ad"
	if runtime.GOOS == "windows" {
		name = "ad.cmd"
	}
	theirs := filepath.Join(root, "bin", name)
	writePayload(t, theirs, "echo 别人装的 ad\n", 0o755)

	err := Install(asm, Options{Dir: root, Yes: true, NoPath: true, Out: io.Discard})
	if err == nil {
		t.Fatal("覆盖了别人的 bin/ad")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("报错没告诉用户可以 --force：%v", err)
	}
	if b, _ := os.ReadFile(theirs); !strings.Contains(string(b), "别人装的") {
		t.Errorf("别人的文件被动过：%q", b)
	}

	// --force 才放行。
	if err := Install(asm, Options{Dir: root, Yes: true, NoPath: true, Out: io.Discard, Force: true}); err != nil {
		t.Fatalf("--force 之后仍然失败：%v", err)
	}
}

// 命令名被另一个包装走了：不能悄悄顶掉（A10）。
func TestInstallRefusesCmdOwnedByAnotherPackage(t *testing.T) {
	home := fakeHome(t)
	root := installHome(home)

	first := toolchainAssembly(t, "ai-desk", "ad", "", "")
	if err := Install(first, Options{Dir: root, Yes: true, NoPath: true, Out: io.Discard}); err != nil {
		t.Fatal(err)
	}

	// 另一个应用，同一个命令名 ad（id 不同，所以不是覆盖安装）。
	second := toolchainAssembly(t, "other-app", "ad", "", "")
	err := Install(second, Options{Dir: root, Yes: true, NoPath: true, Out: io.Discard})
	if err == nil {
		t.Fatal("命令名已经被 ai-desk 占着，却装成功了")
	}
	if !strings.Contains(err.Error(), "ai-desk") {
		t.Errorf("报错没说是谁占着：%v", err)
	}

	// 账本里还是原来那一个。
	led, err := ledger.Load(ledgerPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if len(led.Packages) != 1 || led.Packages[0].ID != "ai-desk" {
		t.Errorf("账本被改动了：%+v", led.Packages)
	}
}

// 简称不能叫 gpm：那是 gpm 自己占着的。
func TestInstallRefusesGpmAsCmd(t *testing.T) {
	fakeHome(t)
	asm := toolchainAssembly(t, "weird", "gpm", "", "")

	err := Install(asm, Options{Yes: true, NoPath: true, Out: io.Discard})
	if err == nil {
		t.Fatal("简称叫 gpm 却装成功了")
	}
	if !strings.Contains(err.Error(), "gpm") {
		t.Errorf("报错没提 gpm：%v", err)
	}
}

// 已经装好就不用再装（v3.8）：家的 bin/ 里已经有这家工具链的命令，说明这台
// 机器上早装过了 —— 用户自己装的那份版本说不定还新些，别去动它。
func TestInstallSkipsToolchainAlreadyInstalled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("这套用例用 shell 脚本假装工具链")
	}
	home := fakeHome(t)
	root := installHome(home)

	// 这台机器上早就有 cot 了。
	old := filepath.Join(root, "bin", "cot")
	writePayload(t, old, "#!/bin/sh\necho 老的 cot\n", 0o755)

	// 包里那份要是被跑了，就会留下这个印子。
	asm := toolchainAssembly(t, "ai-desk", "ad", "cot", "#!/bin/sh\nprintf ran > \"$HOME/ran\"\n")

	var log strings.Builder
	if err := Install(asm, Options{Yes: true, NoPath: true, Out: &log}); err != nil {
		t.Fatalf("安装失败：%v\n%s", err, log.String())
	}
	if _, err := os.Stat(filepath.Join(home, "ran")); !os.IsNotExist(err) {
		t.Errorf("已经装好了却又跑了一遍包里的工具链：%v", err)
	}
	if !strings.Contains(log.String(), "跳过") {
		t.Errorf("没告诉用户工具链那一步跳过了：\n%s", log.String())
	}
	if b, _ := os.ReadFile(old); !strings.Contains(string(b), "老的 cot") {
		t.Errorf("家里那份 cot 被动过：%q", b)
	}
	// 这一条只影响工具链那一步，应用照装。
	if _, err := os.Stat(filepath.Join(root, "bin", "ad")); err != nil {
		t.Errorf("应用没装上：%v", err)
	}
}

// 想强制重铺工具链，加 --force。
func TestInstallForceReinstallsToolchain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("这套用例用 shell 脚本假装工具链")
	}
	home := fakeHome(t)
	root := installHome(home)
	writePayload(t, filepath.Join(root, "bin", "cot"), "#!/bin/sh\necho 老的 cot\n", 0o755)

	asm := toolchainAssembly(t, "ai-desk", "ad", "cot", "#!/bin/sh\nprintf ran > \"$HOME/ran\"\n")

	if err := Install(asm, Options{Yes: true, NoPath: true, Out: io.Discard, Force: true}); err != nil {
		t.Fatalf("安装失败：%v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "ran")); err != nil {
		t.Errorf("--force 之后没重铺工具链：%v", err)
	}
}

// 失败回滚只管 GUI 那部分（v3.8 / D34）：工具链是它自己的东西，不回滚。
// 这里让工具链跑成功，再让 GUI 那一步撞上家里别人的同名入口。
func TestInstallKeepsToolchainWhenGuiPartFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("这套用例用 shell 脚本假装工具链")
	}
	home := fakeHome(t)
	root := installHome(home)
	script := `#!/bin/sh
set -eu
home="$3"
mkdir -p "$home/bin"
printf '#!/bin/sh\necho cot\n' > "$home/bin/cot"
chmod +x "$home/bin/cot"
`
	asm := toolchainAssembly(t, "ai-desk", "ad", "cot", script)

	// 家里已经有一个不是 gpm 装的同名入口（清单里入口叫 demo）。
	theirs := filepath.Join(root, "demo", "keep")
	writePayload(t, theirs, "别人的东西\n", 0o644)

	err := Install(asm, Options{Yes: true, NoPath: true, Out: io.Discard})
	if err == nil {
		t.Fatal("入口被别人的东西占着，却装成功了")
	}
	if !strings.Contains(err.Error(), "已经存在") {
		t.Errorf("报错没说清是撞了别人的东西：%v", err)
	}

	// 工具链写下的东西留着（它是它自己的东西，说不定以前就装好了）。
	if _, err := os.Stat(filepath.Join(root, "bin", "cot")); err != nil {
		t.Errorf("回滚把工具链的东西带走了：%v", err)
	}
	// 别人的东西也没动。
	if b, err := os.ReadFile(theirs); err != nil || string(b) != "别人的东西\n" {
		t.Errorf("别人的东西被动过：%q %v", b, err)
	}
	// GUI 那部分一个字节都没留下：没有启动器，账本里也没有这个包。
	if _, err := os.Stat(filepath.Join(root, "bin", "ad")); !os.IsNotExist(err) {
		t.Errorf("回滚没删掉启动器：%v", err)
	}
	led, err := ledger.Load(ledgerPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if len(led.Packages) != 0 {
		t.Errorf("这次失败却记了账：%+v", led.Packages)
	}
}

// 第二家往后各自的家（cot 住 $COT_HOME / ~/cot）在没装过它的机器上还不存在，
// 而自举要一个已经在那儿的目录：真 cot 用 is_dir() 分辨"把自个儿铺进这个家"
// 还是"装这个包文件"，目录不在就把路径当成包文件名，回一句 COT_HOME 未设置 ——
// 于是 requires: [tdp, cot] 这种"应用住 tdp、cot 回自己家"的包，在干净机器上
// 装不下去。gpm 既然派了这个家，就先把它建出来（2026-10-16，AI Desk 0.5.0 撞上）。
func TestInstallCreatesSecondToolchainHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("这套用例用 shell 脚本假装工具链")
	}
	home := fakeHome(t)
	root := filepath.Join(home, "tdp") // 第一家 = 应用自己的家
	cotHome := filepath.Join(home, "cot")

	// 假 cot：家不在就照真 cot 的样子拒绝。
	cot := `#!/bin/sh
set -eu
home="$3"
if [ ! -d "$home" ]; then
  echo "Error: COT_HOME 未设置，请先安装 cot" >&2
  exit 1
fi
mkdir -p "$home/bin"
printf '#!/bin/sh\necho cot\n' > "$home/bin/cot"
chmod +x "$home/bin/cot"
`
	asm := t.TempDir()
	writePayload(t, filepath.Join(asm, "ad-manifest.yaml"), `id: ai-desk
name: Demo
version: 0.1.0
requires: [tdp, cot]
entry:
  `+runtime.GOOS+`:
    exe: demo
launch:
  cmd: ad
`, 0o644)
	writePayload(t, filepath.Join(asm, "payload", "demo"), "#!/bin/sh\nexit 0\n", 0o755)
	// 假 tdp：自己建家（真 tdp 就会先打一句"已创建 TDP_HOME"）。
	writePayload(t, filepath.Join(asm, "tools", toolchainDir(), "tdp"),
		"#!/bin/sh\nset -eu\nmkdir -p \"$3/bin\"\n", 0o755)
	writePayload(t, filepath.Join(asm, "tools", toolchainDir(), "cot"), cot, 0o755)

	var log strings.Builder
	if err := Install(asm, Options{Yes: true, NoPath: true, Out: &log}); err != nil {
		t.Fatalf("安装失败：%v\n%s", err, log.String())
	}
	if _, err := os.Stat(filepath.Join(cotHome, "bin", "cot")); err != nil {
		t.Errorf("cot 没被铺进自己的家 %s：%v", cotHome, err)
	}
	// 应用还是住在第一家那儿。
	if _, err := os.Stat(filepath.Join(root, "bin", "ad")); err != nil {
		t.Errorf("应用没装进 %s：%v", root, err)
	}
}
