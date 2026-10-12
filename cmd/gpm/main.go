// 命令 gpm 是一个 GUI 应用安装器：把分发包装到安装器指定的家目录，接好终端
// 命令与图形入口，并且能用 gpm uninstall 干净地撤掉。
//
// 命令行与（将来的）图形界面是同一层内核的两个壳。
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/qiuzhanghua/gpm-go/internal/home"
	"github.com/qiuzhanghua/gpm-go/internal/install"
	"github.com/qiuzhanghua/gpm-go/internal/pack"
)

// reorder 把选项挪到位置参数前面。
//
// 标准库的 flag 一旦遇到第一个非选项参数就停止解析，于是
// `gpm install . --dir ~/ad` 会把 --dir 当成第二个位置参数。这里不打算
// 教训用户，直接把参数重排一次；`--` 之后的一律当位置参数。
func reorder(fs *flag.FlagSet, args []string) []string {
	takesValue := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		if _, isBool := f.Value.(interface{ IsBoolFlag() bool }); !isBool {
			takesValue[f.Name] = true
		}
	})
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i+1:]...)
			break
		}
		if len(a) > 1 && a[0] == '-' {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if strings.Contains(name, "=") || !takesValue[name] {
				continue
			}
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		rest = append(rest, a)
	}
	return append(flags, rest...)
}

// version 是变量不是常量，好让发布流程用 -ldflags "-X main.version=..." 覆盖。
// 这个默认值只给本地 go build 用；发布产物一律由 tag 注入真版本号。
var version = "0.6.6-dev"

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "install":
		err = cmdInstall(os.Args[2:])
	case "uninstall", "remove", "rm":
		err = cmdUninstall(os.Args[2:])
	case "list", "ls":
		err = cmdList(os.Args[2:])
	case "where":
		err = cmdWhere(os.Args[2:])
	case "pack":
		err = cmdPack(os.Args[2:])
	case "env":
		err = cmdEnv(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("gpm %s\n", version)
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "gpm: %v\n", err)
		os.Exit(1)
	}
}

func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	dir := fs.String("dir", "", "装到哪个家（留空则按清单的 requires 决定）")
	with := fs.String("with", "", "覆盖清单里的 requires，如 --with cot,tdp；--with 空串表示不装工具链")
	yes := fs.Bool("yes", false, "不询问，直接做 PATH 集成")
	noPath := fs.Bool("no-path", false, "完全不碰 PATH")
	skip := fs.Bool("skip-verify", false, "跳过 SHA256SUMS 校验（只用于调试）")
	force := fs.Bool("force", false, "应用正在运行、命令名被别人占着、或强推 <家>/bin/gpm 里那份（版本读不出来时），也照做（不推荐）")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "用法: gpm install <目录或 .zip> [选项]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("需要且只需要一个参数：分发包路径")
	}
	// `--with ""` 与「没给 --with」是两件事：前者是「谁都不装」。
	withSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "with" {
			withSet = true
		}
	})
	withList := splitList(*with)
	if withSet && withList == nil {
		withList = []string{}
	}
	return install.Install(fs.Arg(0), install.Options{
		Dir:        *dir,
		With:       withList,
		Yes:        *yes,
		NoPath:     *noPath,
		SkipVerify: *skip,
		Force:      *force,
		// 收尾时要不要把 <家>/bin/gpm 里那份旧的换成本进程，靠这个版本号
		// （v3.11、D41）。发布产物由 -ldflags 把它注成真版本号。
		SelfVersion: version,
	})
}

// splitList 把 `--with cot,tdp` 切成 ["cot" "tdp"]。
//
// 区分「没给这个选项」（nil，听清单的）与「给了空串」（空切片，谁都不装）：
// 打包方调试「不装工具链」时正要用后者。
func splitList(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	dir := fs.String("dir", "", "家目录（留空则从 gpm 自己的位置推断）")
	yes := fs.Bool("yes", false, "不询问，直接卸载（脚本里必须给）")
	force := fs.Bool("force", false, "要删的那个应用正在运行也照做（不推荐）")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "用法: gpm uninstall <id> [选项]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("需要且只需要一个参数：包 id")
	}
	return install.Uninstall(fs.Arg(0), install.UninstallOptions{
		Dir:   *dir,
		Force: *force,
		Yes:   *yes,
	})
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	dir := fs.String("dir", "", "家目录（留空则从 gpm 自己的位置推断）")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "用法: gpm list [选项]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	return install.List(*dir, os.Stdout)
}

func cmdWhere(args []string) error {
	fs := flag.NewFlagSet("where", flag.ExitOnError)
	dir := fs.String("dir", "", "家目录（留空则从 gpm 自己的位置推断）")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "用法: gpm where <id> [选项]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("需要且只需要一个参数：包 id")
	}
	return install.Where(*dir, fs.Arg(0), os.Stdout)
}

func cmdPack(args []string) error {
	fs := flag.NewFlagSet("pack", flag.ExitOnError)
	out := fs.String("out", "", "输出 zip 路径（默认 dist/<id>-<version>-<os>-<arch>.zip）")
	goos := fs.String("os", "", "目标平台（默认当前平台）")
	goarch := fs.String("arch", "", "目标架构（默认当前架构）")
	self := fs.String("gpm", "", "要嵌进包里的 gpm 可执行文件（默认当前进程）")
	ddir := fs.String("default-dir", "", "烘进 install.sh/install.cmd/install.ps1 的默认安装根，如 ~/cot（留空则按清单 requires 决定）")
	setup := fs.String("setup", "", "随包的 GUI-Setup 的路径（清单 setup: 段声明的那份）")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "用法: gpm pack <含 <简称>-manifest.yaml 与 payload/ 的目录> [选项]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("需要且只需要一个参数：装配目录")
	}
	p, err := pack.Build(pack.Options{
		Dir:        fs.Arg(0),
		Out:        *out,
		GOOS:       *goos,
		GOARCH:     *goarch,
		Self:       *self,
		DefaultDir: *ddir,
		Setup:      *setup,
		Log:        os.Stdout,
	})
	if err != nil {
		return err
	}
	fmt.Printf("已生成 %s\n", p)
	return nil
}

func cmdEnv(args []string) error {
	fs := flag.NewFlagSet("env", flag.ExitOnError)
	dir := fs.String("dir", "", "家目录（留空则从 gpm 自己的位置推断）")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "用法: gpm env [选项]   # 打印把 bin/ 加进 PATH 的 shell 片段")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	h, err := home.Resolve(*dir)
	if err != nil {
		return err
	}
	fmt.Printf("export PATH=\"%s:$PATH\"\n", h.Bin())
	return nil
}

func usage(w *os.File) {
	fmt.Fprintf(w, `gpm %s —— GUI 应用安装器

用法:
  gpm install <目录或 .zip> [--dir PATH] [--with cot,tdp] [--yes] [--no-path] [--skip-verify] [--force]
  gpm list
  gpm where <id>
  gpm uninstall <id> [--dir PATH] [--yes] [--force]
  gpm pack <装配目录> [--out PATH] [--os OS] [--arch ARCH] [--gpm 可执行文件] [--default-dir PATH] [--setup PATH]
  gpm env
  gpm version

家目录按这个顺序确定（D21）—— 两条链，因为有两件事要问：

安装那一次（gpm install，home.ResolveInstall）：
  --dir
  > 清单里 requires 第一家的家（$COT_HOME / $TDP_HOME，缺省 ~/cot、~/tdp）
  > 从 gpm 自己的位置推断
  > 平台数据目录/<简称>（macOS ~/Library/Application Support，Windows %%LOCALAPPDATA%%，Linux ~/.local/share）
  > 当前目录

装完之后（gpm list / where / uninstall / env，home.Resolve）：
  --dir
  > 从 gpm 自己的位置推断
  > $COT_HOME / $TDP_HOME
  > 当前目录

两条链只差两处：装完之后「自己住哪个家」排在环境变量前面 —— 用户敲的是
<某个家>/bin/gpm，那个位置比 shell 里的变量更具体（变量很可能是另一个家
留下的）；而平台数据目录那一档只属于安装：那是 requires 为空时的落脚点，
装完之后不该再从那里猜。

装到哪儿通常由安装器（install.sh / install.cmd）传进来，那个值来自
gpm pack --default-dir 或清单里的 requires；gpm 自己只认上面这两条链。
清单里没有 requires 时，GUI 应用没有工具链可以借住，就走平台惯例。

「从 gpm 自己的位置推断」是给装完之后用的：带 GUI 的应用住在家的顶层
（<家目录>/<入口名>，v3.6 起），而 gpm 与终端启动器这类没有图形界面的小
东西住在 <家目录>/bin。
于是 <家目录>/bin/gpm 这个位置本身就把家目录说出来了 —— 用户在新终端里
敲 gpm list / gpm where / gpm uninstall 时不必带 --dir，也不必让 shell
一直替 gpm 记着什么环境变量。判据要求所在目录正好叫 bin、文件名正好是 gpm、
且上一级有账本（<家目录名>-state.json），免得把 /usr/local/bin/gpm 这种地方误当成家目录。

清单里的 requires（cot / tdp）说的是：这个包自带那家工具链（zip 里的
tools/<os>_<arch>/），装完要让它的命令也能用。装的时候会在那个家里跑一次
「<名字> i -s <家>」，那一步不联网；工具链的东西不进 gpm 的账本，卸载也不动它。
`, version)
}
