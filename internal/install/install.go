// Package install 是 gpm 的内核：安装、列举、卸载。
//
// 命令行与（将来的）图形界面只是这层的两个壳，走的必须是同一条流水线。
package install

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/qiuzhanghua/gpm-go/internal/home"
	"github.com/qiuzhanghua/gpm-go/internal/integrate"
	"github.com/qiuzhanghua/gpm-go/internal/ledger"
	"github.com/qiuzhanghua/gpm-go/internal/manifest"
	"github.com/qiuzhanghua/gpm-go/internal/proc"
	"github.com/qiuzhanghua/gpm-go/internal/stage"
)

// Options 控制一次安装。
type Options struct {
	Dir        string    // --dir，覆盖清单给出的家
	With       []string  // --with，覆盖清单里的 requires
	Yes        bool      // 不询问，直接做 PATH 集成
	NoPath     bool      // 完全跳过 PATH 集成
	SkipVerify bool      // 跳过 SHA256SUMS 校验（只用于调试）
	Force      bool      // 应用正在运行、命令名被别人占着、或强推 gpm 自己那份，也照做
	In         io.Reader // 默认 os.Stdin
	Out        io.Writer // 默认 os.Stdout

	// SelfVersion 是**正在运行的这份 gpm** 自己的版本（cmd/gpm 的 main.version）。
	//
	// 收尾时要不要拿当前进程去顶掉 <家>/bin/gpm 里那份旧的，全靠它（v3.11、D41）；
	// 留空表示"不知道自己是哪个版本"，那就谁都不动 —— 拿不准时不动，是这个字段
	// 唯一的保守方向。
	SelfVersion string
}

// Install 安装一个分发包（目录或 .zip）。
func Install(src string, opt Options) error {
	out := opt.Out
	if out == nil {
		out = os.Stdout
	}
	in := opt.In
	if in == nil {
		in = os.Stdin
	}

	// 家在哪由清单里的 requires 决定（D21），所以得先隔着包装看一眼清单。
	// 只看那几百字节，几 GB 的载荷等家定下来、staging 建好之后再解。
	peek, err := manifest.Peek(src)
	if err != nil {
		return err
	}
	requires := peek.Requires
	if opt.With != nil {
		requires = opt.With
	}

	h, err := home.ResolveInstall(opt.Dir, peek.Short(), requires)
	if err != nil {
		return err
	}
	freshHome := !h.Exists() // 这个家是这次刚建出来的？失败时好把空骨架收回去
	if err := h.Ensure(); err != nil {
		return err
	}

	// staging/ 只在安装期间有意义：它是下面那个解包目录的父目录，装完
	// （成功或失败）里外都是空的，留着只会让每个家目录多一个看不懂的空目录。
	// 注册在 `defer os.RemoveAll(unpack)` **之前** —— defer 后进先出，
	// 先注册的后跑，正好等解包目录清完再看这一层空不空。
	defer h.DropStagingIfEmpty()

	unpack := filepath.Join(h.Staging(), fmt.Sprintf("unpack-%d", time.Now().UnixNano()))
	if err := os.RemoveAll(unpack); err != nil {
		return err
	}
	defer os.RemoveAll(unpack)

	if err := stage.Materialize(src, unpack); err != nil {
		return err
	}

	verified := false
	if opt.SkipVerify {
		fmt.Fprintln(out, "已按 --skip-verify 跳过校验。")
	} else {
		v, err := stage.VerifySums(unpack, manifest.PayloadDir, manifest.ToolsDir)
		if err != nil {
			return err
		}
		verified = v
		if !v {
			fmt.Fprintln(out, "警告：包里没有 SHA256SUMS，本次安装标记为 unverified。")
		}
	}

	m, err := manifest.Load(unpack)
	if err != nil {
		return err
	}
	if err := m.Validate(h.GOOS); err != nil {
		return err
	}
	entry, _ := m.EntryFor(h.GOOS)

	payloadRoot := filepath.Join(unpack, manifest.PayloadDir)
	srcEntry := filepath.Join(payloadRoot, filepath.FromSlash(entry.Rel()))
	if _, err := os.Lstat(srcEntry); err != nil {
		return fmt.Errorf("清单声明的入口在包里找不到：payload/%s", entry.Rel())
	}

	tcs := toolchainsFor(h, requires)

	led, err := ledger.Load(h.LedgerPath())
	if err != nil {
		return err
	}
	if err := checkLauncherName(h, led, m, opt.Force); err != nil {
		return err
	}

	// 工具链先装（F3 的顺序）：它要是装不上，应用这边一个字节都还没动，
	// 用户看到的是「什么都没发生」，而不是半个应用。
	//
	// 反过来，这一步写下的东西不进下面那个退栈（v3.8）：工具链归它自己管
	// （D34），后面哪一步失败、回滚，只回滚 GUI 那部分 —— cot 说不定用户早
	// 就装好了，凭什么因为这次装 GUI 不成把它删掉。
	if err := installToolchains(unpack, tcs, opt.Force, out); err != nil {
		if freshHome {
			// 先把 staging 里那半份载荷清掉，DropIfEmpty 才收得回去
			// —— 它只删空目录，免得不小心带走别人的东西；工具链自己写下的
			// 东西也一律留着（v3.8）。
			os.RemoveAll(unpack)
			h.DropIfEmpty()
		}
		return err
	}

	if prev := led.Find(m.ID); prev != nil {
		if err := checkNotRunning(prev, opt.Force, "重新运行一次 gpm install", out); err != nil {
			return err
		}
		fmt.Fprintf(out, "检测到已装过的 %s %s，先卸掉旧版本。\n", prev.Name, prev.Version)
		if err := remove(h, led, prev.ID, out); err != nil {
			return err
		}
	}

	// v3.6：GUI 的实体直接落在家目录下（跟 bin/、lib/、账本平级）。
	// lib/<id>_<版本>_<平台>/ 那套是命令行插件的位置，GUI 程序不放那儿。
	top, err := payloadTop(payloadRoot, entry)
	if err != nil {
		return err
	}
	pkgDir := h.AppDir(top)
	if err := checkAppDir(h, pkgDir, opt.Force); err != nil {
		return err
	}

	// 这个退栈只管 GUI 那部分（v3.8）：入口、启动器、图形入口、PATH 标记块。
	// 工具链写下的东西不在这儿 —— 它归工具链自己管（D34）。
	var rollback []func()
	fail := func(err error) error {
		for i := len(rollback) - 1; i >= 0; i-- {
			rollback[i]()
		}
		return err
	}
	// payload 根下就入口那一个条目，所以收回来的正好是它。
	rollback = append(rollback, func() { os.RemoveAll(pkgDir) })

	if err := os.RemoveAll(pkgDir); err != nil {
		return err
	}
	if err := stage.CopyTree(payloadRoot, h.Root); err != nil {
		return fail(err)
	}
	entryAbs := filepath.Join(h.Root, filepath.FromSlash(entry.Rel()))
	if entry.Kind() == "exe" {
		if err := os.Chmod(entryAbs, 0o755); err != nil {
			return fail(err)
		}
	}

	// v3.10（D40）：把下载器打在包上的 quarantine 标记从落地产物上清掉。
	// 复制走的是 stage.CopyTree（逐字节写、不搬扩展属性），所以正常路径上
	// 这里是空转；但"碰巧"不是契约 —— 哪天复制换成 ditto，标记就会跟着
	// 进家目录，症状是"装好了却双击打不开"，看着跟安装毫无关系。
	// 失败不判定安装失败：R1 的正解是签名 + 公证，这一步只是过渡期兜底。
	if err := integrate.StripQuarantine(entryAbs); err != nil {
		fmt.Fprintf(out, "提示：没能清掉下载标记（%v）。手动执行：\n  xattr -dr com.apple.quarantine %q\n", err, pkgDir)
	}
	fmt.Fprintf(out, "已释放到 %s\n", pkgDir)

	launcher, err := integrate.Launcher(integrate.Spec{
		BinDir:     h.Bin(),
		Cmd:        m.Launch.Cmd,
		EntryAbs:   entryAbs,
		Kind:       entry.Kind(),
		GOOS:       h.GOOS,
		Mode:       m.Mode(),
		Toolchains: tcs,
	})
	if err != nil {
		return fail(err)
	}
	rollback = append(rollback, func() { os.Remove(launcher) })
	fmt.Fprintf(out, "已生成终端启动器 %s\n", launcher)

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	var links []ledger.Link
	link, note, err := integrate.AppLink(h.GOOS, homeDir, m.ID, m.Name, entryAbs, launcher)
	if err != nil {
		return fail(err)
	}
	if note != "" {
		fmt.Fprintln(out, note)
	}
	if link != nil {
		links = append(links, *link)
		lp := link.Path
		rollback = append(rollback, func() { os.Remove(lp) })
		fmt.Fprintf(out, "已建立图形入口 %s\n", link.Path)
	}

	var paths []ledger.PathEdit
	switch {
	case opt.NoPath:
		fmt.Fprintln(out, "已按 --no-path 跳过 PATH 集成。")
	case h.GOOS == "windows":
		// Windows 的 PATH 在注册表里，不在任何文件里，所以走另一条路。
		if askPath(out, in, opt.Yes, m.Launch.Cmd, h.Bin(), h.GOOS, nil) {
			changed, err := integrate.InstallWindowsPath(h.Bin())
			if err != nil {
				return fail(err)
			}
			if changed {
				// 伪路径没有「那个文件原本在不在」这回事，但账本是给人看的：
				// 能走到这里就说明这条 PATH 记录确实是这次加进去的，所以记 true。
				// 卸载时它被单独处理（见 Uninstall），不看这个值。
				paths = append(paths, ledger.PathEdit{Path: integrate.WindowsPathEditPath, Created: true})
				rollback = append(rollback, func() { integrate.RemoveWindowsPath(h.Bin()) })
				fmt.Fprintf(out, "已写入用户 PATH（注册表 HKCU\\Environment），并广播了环境变更通知。\n")
			} else {
				fmt.Fprintf(out, "用户 PATH 里已经有 %s，没有重复添加。\n", h.Bin())
			}
		} else {
			fmt.Fprintf(out, "已跳过 PATH 集成。想让当前这个 cmd 立刻能用，执行：\n  set \"PATH=%s;%%PATH%%\"\n", h.Bin())
		}
	default:
		files := integrate.ShellProfiles(h.GOOS, homeDir)
		switch {
		case askPath(out, in, opt.Yes, m.Launch.Cmd, h.Bin(), h.GOOS, files):
			edits, err := integrate.InstallPathBlock(files, h.Bin(), homeDir)
			if err != nil {
				return fail(err)
			}
			paths = edits
			rollback = append(rollback, func() { integrate.RemovePathBlock(files) })
			for _, e := range edits {
				fmt.Fprintf(out, "已写入 PATH 标记块 %s\n", e.Path)
			}
		default:
			fmt.Fprintf(out, "已跳过 PATH 集成。想让当前这个终端立刻能用，执行：\n  export PATH=\"%s:$PATH\"\n", h.Bin())
		}
	}

	self, err := installSelf(h.Bin(), opt.SelfVersion, opt.Force)
	if err != nil {
		fmt.Fprintf(out, "提示：没能把 gpm 自己拷进 %s：%v\n", h.Bin(), err)
	}
	// v3.11（D41）：家里那份比这份旧就换掉，并说清楚换了什么。
	// 新装（那儿本来没有）不吱声 —— 那是从 v3.5 起就有的默认行为。
	switch {
	case self.Replaced && self.OldVer != "":
		fmt.Fprintf(out, "已把 %s 从 gpm %s 换成 gpm %s。\n", self.Dest, self.OldVer, opt.SelfVersion)
	case self.Replaced:
		fmt.Fprintf(out, "已按 --force 覆盖 %s（原来那份的版本读不出来）。\n", self.Dest)
	case self.Kept && self.OldVer != "":
		fmt.Fprintf(out, "提示：%s 已经有一个 gpm %s，没有这份新，这次没覆盖它。\n", self.Dest, self.OldVer)
	case self.Kept:
		fmt.Fprintf(out, "提示：%s 已经有一个 gpm，版本读不出来，这次没覆盖它；要强制替换就加 --force。\n", self.Dest)
	}

	led.Put(ledger.Package{
		ID:          m.ID,
		Name:        m.Name,
		Version:     m.Version,
		Platform:    h.Platform(),
		InstalledAt: time.Now(),
		Dir:         pkgDir,
		Entry:       entryAbs,
		EntryKind:   entry.Kind(),
		Cmd:         m.Launch.Cmd,
		Launcher:    launcher,
		Links:       links,
		PathEdits:   paths,
		Verified:    verified,
	})
	// 只记「我们自己放进去的那一份」。原来就在那儿的 gpm 属于用户，
	// 卸载最后一个包时不能顺手把它删掉（见 installSelf）。
	//
	// v3.11 起"替换掉旧的"也算我们放的：那份文件确实是这次由我们写下去的，
	// 卸载时按同一条规矩处置（见 Uninstall 里对 Self 的判断）。
	if self.Wrote {
		led.Self = self.Dest
	}
	if err := led.Save(h.LedgerPath()); err != nil {
		return fail(err)
	}

	fmt.Fprintf(out, "\n%s %s 装好了。\n  位置：%s\n  命令：%s\n", m.Name, m.Version, pkgDir, m.Launch.Cmd)
	return nil
}

// toolchainsFor 算出这次要装、要注入哪几家工具链，各自的家在哪。
//
// 第一家就是应用自己住的那个家（D21：家按 requires 的第一家取），工具链
// 也住进去 —— 这正是「GUI 与命令行工具同住一家」的意思（R14）。第二家
// 往后各自回自己的家（cot 住 $COT_HOME，tdp 住 $TDP_HOME）。
func toolchainsFor(h *home.Home, requires []string) []integrate.Toolchain {
	var out []integrate.Toolchain
	for i, r := range requires {
		root := h.Root
		if i > 0 {
			if v := home.RequireHome(r); v != "" {
				root = v
			}
		}
		out = append(out, integrate.Toolchain{Name: r, Home: root})
	}
	return out
}

// installToolchains 把 zip 自带的工具链铺进它的家。
//
// 跑的是包里的 tools/<os>_<arch>/<name>，命令是 `<name> i -s <家>`：cot
// （与同一个框架的 tdp）的 install 只要一个目录参数，-s 是 --silence。
// 这一步不联网 —— 工具链的二进制就在包里，它只把自个儿铺进家目录（C3）。
//
// 已经装好就跳过（v3.8）：家的 bin/ 里已经有这家工具链的命令，就说明这台
// 机器上早装过了 —— 很可能是用户自己装的，版本说不定比包里这份还新，没必要
// 再搬一遍。要强制重铺，加 --force。
//
// 归属边界（D34）：这一步写下的东西不进 gpm 的账本，卸载也不回放。那是
// 工具链自己的东西，归它自己的命令管（cot use / cot rm）。gpm 只负责把
// 它搬来；装到一半失败时，除了刚建出来的空骨架，gpm 不去猜哪些是它的。
func installToolchains(unpack string, tcs []integrate.Toolchain, force bool, out io.Writer) error {
	if len(tcs) == 0 {
		return nil
	}
	rel := filepath.Join(manifest.ToolsDir, runtime.GOOS+"_"+runtime.GOARCH)
	for _, t := range tcs {
		name := t.Name
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		if !force {
			if fi, err := os.Stat(filepath.Join(t.Home, "bin", name)); err == nil && !fi.IsDir() {
				fmt.Fprintf(out, "已经装好 %s（%s），跳过。\n", t.Name, filepath.Join(t.Home, "bin", name))
				continue
			}
		}
		exe := filepath.Join(unpack, rel, name)
		if _, err := os.Stat(exe); err != nil {
			return fmt.Errorf("清单声明 requires: [%s]，但包里没有 %s —— 打这个包的人忘了把工具链放进去",
				t.Name, filepath.Join(rel, name))
		}
		if err := os.Chmod(exe, 0o755); err != nil {
			return err
		}
		// 家得先在：第一家就是应用自己的家，那目录 gpm 本来就会建；第二家
		// 往后各自的家（cot 住 $COT_HOME / ~/cot）在还没装过它的机器上根本
		// 不存在，而自举要一个已经在那儿的目录 —— cot 用 is_dir() 分辨
		// "把自个儿铺进这个家"还是"装这个包文件"，目录不在就把路径当成包
		// 文件名，回一句 COT_HOME 未设置（tdp 会自己建，cot 不会）。
		if err := os.MkdirAll(t.Home, 0o755); err != nil {
			return err
		}
		fmt.Fprintf(out, "正在把 %s 铺进 %s（用包里自带的那一份）…\n", t.Name, t.Home)
		cmd := exec.Command(exe, "i", "-s", t.Home)
		cmd.Dir = unpack
		cmd.Stdout = out
		cmd.Stderr = out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("装 %s 失败：%w（它自己留下的东西 gpm 不动，见 D34）", t.Name, err)
		}
	}
	return nil
}

// payloadTop 校验 payload/ 根下只有入口那一个条目，并返回它的顶层名字。
//
// v3.6 起应用的落点是 <家>/<顶层名>，账本里记的 Dir 也只有一个，
// 所以载荷必须"一个包 = 一个入口"，要带陪衬文件就跟入口一起装进一个目录。
func payloadTop(payloadRoot string, entry manifest.Entry) (string, error) {
	items, err := os.ReadDir(payloadRoot)
	if err != nil {
		return "", err
	}
	top := strings.Split(filepath.ToSlash(entry.Rel()), "/")[0]
	names := make([]string, 0, len(items))
	for _, it := range items {
		names = append(names, it.Name())
	}
	if len(items) != 1 || items[0].Name() != top {
		return "", fmt.Errorf("payload/ 根下只放入口那一个东西：清单说入口是 %q，payload/ 里却有 [%s]。要带别的文件，就跟入口一起装进一个目录",
			entry.Rel(), strings.Join(names, ", "))
	}
	return top, nil
}

// checkAppDir 挡住往家里已经有的东西上摊：v3.6 起应用实体就住在
// 家目录下，那里同时住着 gpm / 工具链自己的骨架。
func checkAppDir(h *home.Home, dest string, force bool) error {
	// 账本的新旧两个名字都算家的骨架：旧的那个可能还躺在家里等着迁移
	// （v3.7 换名），让一个入口把它顶掉就等于把账本弄丢。
	for _, own := range []string{h.Bin(), h.Lib(), h.Staging(), h.LedgerPath(), h.LegacyLedgerPath(), h.Log()} {
		if samePath(dest, own, h.GOOS) {
			return fmt.Errorf("入口不能叫 %q —— 那是这个家自己的东西，换个名字重打包", filepath.Base(dest))
		}
	}
	if _, err := os.Lstat(dest); err == nil {
		if !force {
			return fmt.Errorf("%s 已经存在，而且不是 gpm 装的：不动别人的东西（确实要覆盖，加 --force）", dest)
		}
	}
	return nil
}

// samePath 比两个路径是不是同一个东西。macOS 与 Windows 的文件系统默认
// 不区分大小写，Bin/ 和 bin/ 在那儿是同一个目录。
func samePath(a, b, goos string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if goos == "windows" || goos == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// checkLauncherName 在写启动器之前挡住重名（FR-21）。
//
// 两道闸：账本里已经有别的包在用这个名字（跨 id 的比较），以及
// <家>/bin/<cmd> 那儿已经有个不是 gpm 写的文件。任一命中都停手，
// 要强行覆盖得明说 --force。
func checkLauncherName(h *home.Home, led *ledger.Ledger, m *manifest.Manifest, force bool) error {
	cmd := m.Launch.Cmd
	if cmd == "gpm" {
		return fmt.Errorf("简称 %q 是 gpm 自己用的，请换一个", cmd)
	}
	if other := led.FindByCmd(cmd); other != nil && other.ID != m.ID && !force {
		return fmt.Errorf("命令名 %q 已经属于 %s（%s）。换个简称，或者先 `gpm uninstall %s`",
			cmd, other.Name, other.ID, other.ID)
	}
	p := integrate.LauncherPath(h.Bin(), cmd, h.GOOS)
	exists, ours, err := integrate.OwnedByGpm(p)
	if err != nil {
		return err
	}
	if exists && !ours && !force {
		return fmt.Errorf("%s 已经存在，而且不是 gpm 写的：不动别人的东西（确实要覆盖，加 --force）", p)
	}
	return nil
}

// List 打印已经装了什么。
func List(dir string, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	h, err := home.Resolve(dir)
	if err != nil {
		return err
	}
	led, err := ledger.Load(h.LedgerPath())
	if err != nil {
		return err
	}
	if len(led.Packages) == 0 {
		fmt.Fprintf(out, "%s 里还没装东西。\n", h.Root)
		return nil
	}
	fmt.Fprintf(out, "安装位置：%s\n", h.Root)
	for _, p := range led.Packages {
		flag := ""
		if !p.Verified {
			flag = "  [unverified]"
		}
		fmt.Fprintf(out, "  %s  %s  %s%s\n     入口：%s\n     命令：%s\n",
			p.ID, p.Version, p.Platform, flag, p.Entry, p.Cmd)
	}
	return nil
}

// Where 打印某个包的所有落点。
func Where(dir, id string, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	h, err := home.Resolve(dir)
	if err != nil {
		return err
	}
	led, err := ledger.Load(h.LedgerPath())
	if err != nil {
		return err
	}
	p := led.Find(id)
	if p == nil {
		return fmt.Errorf("账本里没有 %q", id)
	}
	fmt.Fprintf(out, "id        %s\nname      %s\nversion   %s\nplatform  %s\n安装目录  %s\n入口      %s\n启动器    %s\n",
		p.ID, p.Name, p.Version, p.Platform, p.Dir, p.Entry, p.Launcher)
	for _, l := range p.Links {
		fmt.Fprintf(out, "图形入口  %s\n", l.Path)
	}
	for _, e := range p.PathEdits {
		fmt.Fprintf(out, "PATH 块   %s\n", e.Path)
	}
	fmt.Fprintf(out, "安装时间  %s\n", p.InstalledAt.Format(time.RFC3339))
	return nil
}

// UninstallOptions 控制一次卸载。
type UninstallOptions struct {
	Dir   string    // --dir，覆盖从 gpm 自己的位置推断出来的家
	Force bool      // 应用正在运行也照做（不推荐）
	Yes   bool      // 不询问，直接卸载（脚本里必须给）
	In    io.Reader // 默认 os.Stdin
	Out   io.Writer // 默认 os.Stdout
}

// Uninstall 回放账本，把某个包留下的东西全部摘掉。
//
// 删掉的东西回不来，所以动手之前先问一次（D39）：交互终端里把要删的摊开、
// 问一句 [y/N]，--yes 跳过；非交互环境（脚本、管道、双击）不给 --yes 就
// 什么都不删 —— 跟安装时请求 PATH 许可（askPath）用同一套口径。
func Uninstall(id string, opt UninstallOptions) error {
	out := opt.Out
	if out == nil {
		out = os.Stdout
	}
	in := opt.In
	if in == nil {
		in = os.Stdin
	}
	h, err := home.Resolve(opt.Dir)
	if err != nil {
		return err
	}
	led, err := ledger.Load(h.LedgerPath())
	if err != nil {
		return err
	}
	p := led.Find(id)
	if p == nil {
		return fmt.Errorf("账本里没有 %q", id)
	}
	if err := checkNotRunning(p, opt.Force, "重新运行一次 gpm uninstall", out); err != nil {
		return err
	}
	if !askUninstall(out, in, opt.Yes, p) {
		fmt.Fprintf(out, "已取消，%q 什么都没删。\n", id)
		return nil
	}
	if err := remove(h, led, id, out); err != nil {
		return err
	}
	// 账本里记着的那份 gpm，只有在这个家是 gpm 自己的时候才删。当前
	// shell 要是已经站在某家工具链里（COT_HOME / TDP_HOME 有值），
	// 这个家就归 cot/tdp 管，那份 gpm 留着 —— 它很可能就是用户手上
	// 正在敲的那一个。想删就先 unset 再卸，或者直接 rm。
	if len(led.Packages) == 0 && led.Self != "" {
		if name, val, ok := home.ActiveToolchainEnv(); ok {
			fmt.Fprintf(out, "保留 %s：环境里 %s=%s，这个家归工具链管，gpm 自己这一份不删。\n",
				led.Self, name, val)
		} else if err := os.Remove(led.Self); err == nil {
			fmt.Fprintf(out, "已删除 %s\n", led.Self)
			led.Self = ""
		}
	}
	if err := led.Save(h.LedgerPath()); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s 已卸载。目录 %s 还在（里面可能还有你放的东西）。\n", id, h.Root)
	return nil
}

// askUninstall 把「要删什么」摊开，再请求一次许可（D39）。
//
// 跟安装时请求 PATH 许可（askPath）同一套口径：非交互环境里不替用户
// 猜意思，必须明确 --yes 才动手。少删一次只是麻烦，多删一次回不来。
func askUninstall(out io.Writer, in io.Reader, yes bool, p *ledger.Package) bool {
	if yes {
		return true
	}
	fmt.Fprintf(out, "\n要卸载的是：\n  %s %s（%s）\n", p.Name, p.Version, p.ID)
	fmt.Fprintf(out, "  入口      %s\n", p.Entry)
	if p.Launcher != "" {
		fmt.Fprintf(out, "  启动器    %s\n", p.Launcher)
	}
	for _, l := range p.Links {
		fmt.Fprintf(out, "  图形入口  %s\n", l.Path)
	}
	for _, e := range p.PathEdits {
		fmt.Fprintf(out, "  PATH 块   %s\n", e.Path)
	}
	fmt.Fprintln(out, "这些是 gpm 自己装下的东西，删掉就回不来了。")

	if !isTerminal(in) {
		fmt.Fprintln(out, "当前不是交互终端，没有动手。确认要删就重跑一次并明确同意：")
		fmt.Fprintf(out, "  gpm uninstall %s --yes\n", p.ID)
		return false
	}
	fmt.Fprint(out, "是否继续？[y/N] ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		fmt.Fprintln(out, "没有读到你的输入，什么都没删。确认要删就重跑一次并明确同意：")
		fmt.Fprintf(out, "  gpm uninstall %s --yes\n", p.ID)
		return false
	}
	s := strings.ToLower(strings.TrimSpace(line))
	return s == "y" || s == "yes"
}

func remove(h *home.Home, led *ledger.Ledger, id string, out io.Writer) error {
	p := led.Find(id)
	if p == nil {
		return fmt.Errorf("账本里没有 %q", id)
	}
	for _, l := range p.Links {
		if err := os.Remove(l.Path); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				fmt.Fprintf(out, "警告：删不掉 %s：%v\n", l.Path, err)
			}
		} else {
			fmt.Fprintf(out, "已删除 %s\n", l.Path)
		}
	}
	if p.Launcher != "" {
		if err := os.Remove(p.Launcher); err == nil {
			fmt.Fprintf(out, "已删除 %s\n", p.Launcher)
		}
	}
	if p.Dir != "" {
		if err := os.RemoveAll(p.Dir); err == nil {
			fmt.Fprintf(out, "已删除 %s\n", p.Dir)
		}
	}
	if len(p.PathEdits) > 0 {
		// Windows 的 PATH 是一条注册表记录，账本里用伪路径表示；
		// 其余的才是 shell 配置文件里的标记块。两者不能混在一起处理。
		var files []string
		windowsPath := false
		for _, e := range p.PathEdits {
			if e.Path == integrate.WindowsPathEditPath {
				windowsPath = true
				continue
			}
			files = append(files, e.Path)
		}
		if windowsPath {
			if changed, err := integrate.RemoveWindowsPath(h.Bin()); err != nil {
				fmt.Fprintf(out, "警告：摘除注册表 PATH 失败：%v\n", err)
			} else if changed {
				fmt.Fprintln(out, "已从用户 PATH（注册表 HKCU\\Environment）里摘除")
			}
		}
		if len(files) > 0 {
			changed, err := integrate.RemovePathBlock(files)
			if err != nil {
				fmt.Fprintf(out, "警告：摘除 PATH 标记块失败：%v\n", err)
			}
			for _, f := range changed {
				fmt.Fprintf(out, "已摘除 PATH 标记块 %s\n", f)
			}
		}
		for _, e := range p.PathEdits {
			if !e.Created || e.Path == integrate.WindowsPathEditPath {
				continue
			}
			if b, err := os.ReadFile(e.Path); err == nil && strings.TrimSpace(string(b)) == "" {
				if err := os.Remove(e.Path); err == nil {
					fmt.Fprintf(out, "已删除（是我们建的空文件） %s\n", e.Path)
				}
			}
		}
	}
	led.Remove(id)
	return nil
}

// checkNotRunning 在动一个已装应用的目录之前，先看看它是不是正开着。
//
// 返回错误时调用方必须停手。真让它过去会出两种事，见 internal/proc 的开头。
//
// 查不出来的时候不拦：宁可漏拦一次，也不要因为「不知道」把安装挡死 ——
// 未来多一个平台、或者某个受限环境不让枚举进程，用户还能照常干活。
func checkNotRunning(p *ledger.Package, force bool, again string, out io.Writer) error {
	if p == nil || p.Dir == "" {
		return nil
	}
	procs, err := proc.Find(p.Dir)
	if err != nil {
		fmt.Fprintf(out, "提示：没能确认 %s 是不是正在运行（%v），这次不拦。\n", p.Name, err)
		return nil
	}
	if len(procs) == 0 {
		return nil
	}
	if force {
		fmt.Fprintf(out, "警告：%s 正在运行（%s），按 --force 继续。\n", p.Name, procsText(procs))
		return nil
	}
	fmt.Fprintf(out, "\n%s 正在运行（%s），现在动它，它脚下的文件会被换掉或抽走。\n", p.Name, procsText(procs))
	fmt.Fprintln(out, "它自己不会马上退出，但之后读到的资源、动态库、拉起的子进程都可能是另一份（版本混用）；")
	fmt.Fprintln(out, "在 macOS 上还会留下一个幽灵进程：那个位置一直指着这个旧进程，你下次点图标只会把它唤到前台，拿不到新的。")
	fmt.Fprintf(out, "请先退出 %s，再%s。\n", p.Name, again)
	fmt.Fprintln(out, "确定不在乎（比如在脚本里批量处理），加 --force。")
	return fmt.Errorf("%s 正在运行，没动手", p.Name)
}

// procsText 把进程列成给人看的一行。太多就截断：这里不需要完整清单。
func procsText(procs []proc.Process) string {
	const show = 3
	var parts []string
	for _, p := range procs {
		if p.Exe != "" {
			parts = append(parts, fmt.Sprintf("进程 %d：%s", p.PID, p.Exe))
		} else {
			parts = append(parts, fmt.Sprintf("进程 %d", p.PID))
		}
		if len(parts) == show && len(procs) > show {
			parts = append(parts, fmt.Sprintf("共 %d 个", len(procs)))
			break
		}
	}
	return strings.Join(parts, "，")
}

// selfResult 是一次 installSelf 的结果，调用方靠它写提示、决定账本记不记。
type selfResult struct {
	Dest     string // <家>/bin/gpm（Windows 上是 gpm.exe）
	Wrote    bool   // 这次真的写了一份（新装、或替换掉旧的）：卸载最后一个包时该由我们删掉
	Replaced bool   // 写下去的那份是**替换**来的（原来那儿有一份）——用来区分"新装"
	Kept     bool   // 那儿本来就有一份，我们没动它
	OldVer   string // 被替换掉的、或留下来的那一份的版本；读不出来是空串
}

// installSelf 把正在运行的 gpm 拷进 <家目录>/bin，保证之后 list/uninstall 还找得到它。
//
// 那儿已经有一份时**比一次版本**（v3.11、D41）：包里这份更新就换掉它，同版本或更旧
// 就原样留着。在这之前是一律不覆盖 —— 那确实防住了"装个旧包把新 gpm 降级"，但也让
// gpm 自己永远升不上去（O7：装新包既不会覆盖 <bin>/gpm，gpm 也没有升级命令）。
//
// 版本是**问出来的**（跑那儿那份的 `--version`），不是从账本里读的：账本记的是我们
// 上次放的那一份，用户随手把那个文件换掉之后账本就不作数了，而比错的方向恰好是最坏
// 的一种 —— 拿新的盖掉更新的。问不出来（不是可执行文件、跑不起来、输出认不出）就
// 什么都不做：宁可漏升一次，也不猜。
//
// 按用户的裁决，替换**不看那个文件是谁放的**：不管是用户自己搁的还是我们上次放的，
// 只要版本更旧就换（外来的覆盖风险记在 DESIGN R19）。force 是给"读不出来但就是想换"
// 留的口子（--force）。
func installSelf(binDir, selfVersion string, force bool) (selfResult, error) {
	exe, err := os.Executable()
	if err != nil {
		return selfResult{}, err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dest := filepath.Join(binDir, "gpm")
	if runtime.GOOS == "windows" {
		dest += ".exe"
	}
	res := selfResult{Dest: dest}
	if filepath.Clean(exe) == filepath.Clean(dest) {
		return res, nil // 运行的就是它，无事可做
	}
	if fi, err := os.Stat(dest); err == nil && !fi.IsDir() {
		if v, ok := querySelfVersion(dest); ok {
			res.OldVer = v.String()
		}
		if !force && !selfIsNewer(selfVersion, res.OldVer) {
			res.Kept = true
			return res, nil // 不比这份新（或读不出来），原样留着
		}
		res.Replaced = true
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return selfResult{}, err
	}
	in, err := os.Open(exe)
	if err != nil {
		return selfResult{}, err
	}
	defer in.Close()
	tmp := dest + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return selfResult{}, err
	}
	if _, err := io.Copy(f, in); err != nil {
		f.Close()
		os.Remove(tmp)
		return selfResult{}, err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return selfResult{}, err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return selfResult{}, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return selfResult{}, err
	}
	res.Wrote = true
	return res, nil
}

// retryHint 是「想把 PATH 集成也做上，该重跑哪条命令」的写法。
//
// 按平台分开写：Windows 上包里的二进制叫 gpm.exe，用户也是解压后在同一个目录里
// 双击 install.cmd（或跑 install.ps1）的，`./gpm install . --yes` 在 cmd 里根本敲不出来。
func retryHint(goos string) string {
	if goos == "windows" {
		return "gpm.exe install . --yes"
	}
	return "./gpm install . --yes"
}

// askPath 用人话请求一次许可。非交互环境（不是终端）时直接跳过。
//
// files 只对类 Unix 有意义；Windows 上 PATH 在注册表里，没有文件可列。
func askPath(out io.Writer, in io.Reader, yes bool, cmd, binDir, goos string, files []string) bool {
	if yes {
		return true
	}
	if !isTerminal(in) {
		fmt.Fprintln(out, "当前不是交互终端，跳过 PATH 集成（软件本身照装）。")
		return false
	}
	if goos == "windows" {
		fmt.Fprintf(out, "\n为了让命令 %q 在终端里能用，gpm 需要把\n  %s\n加进你的用户 PATH（注册表 HKCU\\Environment）。随时可以用 gpm uninstall 撤销。\n", cmd, binDir)
	} else {
		fmt.Fprintf(out, "\n为了让命令 %q 在终端里能用，gpm 需要把\n  %s\n加进 PATH。做法是在下面这些文件末尾追加一小段标记块（随时可用 gpm uninstall 撤销）：\n", cmd, binDir)
		for _, f := range files {
			fmt.Fprintf(out, "  %s\n", f)
		}
	}
	fmt.Fprint(out, "是否继续？[y/N] ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		// 一个字节都没读到：从脚本里跑的、双击运行、或者 stdin 是 /dev/null。
		// 这时候别假装用户回答了「不」，要把发生了什么、以及怎么才能装全说清楚。
		fmt.Fprintf(out, "没有读到你的输入，已跳过 PATH 集成。\n想让命令 %q 在终端里能用，重跑一次并明确同意：\n  %s\n", cmd, retryHint(goos))
		return false
	}
	s := strings.ToLower(strings.TrimSpace(line))
	return s == "y" || s == "yes"
}

func isTerminal(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
