package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sagernet/sing-box/experimental/multiwan/buildinfo"
	"github.com/sagernet/sing-box/experimental/multiwan/config"
	"github.com/sagernet/sing-box/experimental/multiwan/control"
	"github.com/sagernet/sing-box/experimental/multiwan/doctor"
	"github.com/sagernet/sing-box/experimental/multiwan/engine"
	"github.com/sagernet/sing-box/experimental/multiwan/kernel"
	"github.com/sagernet/sing-box/experimental/multiwan/netif"
	"github.com/sagernet/sing-box/experimental/multiwan/session"
	"github.com/sagernet/sing-box/experimental/multiwan/telemetry"
)

const (
	builtinConfig = "/etc/config/swanix"
	runDir        = "/var/run/swanix"
)

// defaultConfig can be redirected with SWANIX_CONFIG (used by tests and for
// running against a staged copy).
var defaultConfig = func() string {
	if p := os.Getenv("SWANIX_CONFIG"); p != "" {
		return p
	}
	return builtinConfig
}()

const usage = `用法: swanix <命令> [选项]

  check     [-c 文件]            校验配置，错误带字段路径
  doctor    [-c 文件] [--json]   检查系统是否可以安全接管
  preview   [-c 文件]            显示将要安装的 nft 规则与路由，不写入任何内容
  run       [-c 文件]            运行守护进程（由 procd 启动）
  status    [--json]             查看运行状态
  history   [--json]             最近一小时的吞吐历史（内存环形缓冲）
  apply     [-c 文件] [--timeout 90s] [--verify 40s]
                                 切换到 swanix；验证通过（--verify 内有线路通过探测）后进入确认时限
  confirm                        确认并保留切换
  rollback                       撤回本次切换
  reconcile                      通知守护进程重新读取接口状态
  remove    [-c 文件]            撤销 swanix 写入内核的表、规则与路由（停用后清理用）
  features  [--json]             报告编译进来的能力
  version   [--json]             版本与构建信息
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "version":
		err = cmdVersion(rest, stdout)
	case "features":
		err = cmdFeatures(rest, stdout)
	case "check":
		err = cmdCheck(rest, stdout)
	case "doctor":
		err = cmdDoctor(rest, stdout)
	case "preview":
		err = cmdPreview(rest, stdout)
	case "run":
		err = cmdRun(rest)
	case "status":
		err = cmdStatus(rest, stdout)
	case "history":
		err = cmdHistory(rest, stdout)
	case "apply":
		err = cmdApply(rest, stdout)
	case "confirm":
		err = cmdConfirm(stdout)
	case "rollback":
		err = cmdRollback(stdout)
	case "reconcile":
		err = cmdReconcile(rest)
	case "guard":
		err = cmdGuard(rest)
	case "remove":
		err = cmdRemove(rest)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "未知命令 %q\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "swanix:", err)
		return 1
	}
	return 0
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func flagSet(name string) (*flag.FlagSet, *string, *bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg := fs.String("c", defaultConfig, "")
	js := fs.Bool("json", false, "")
	return fs, cfg, js
}

func loadConfig(path string) (*config.Config, error) {
	cfg, err := config.LoadFile(path)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(profile); err != nil {
		return nil, err
	}
	return cfg, nil
}

func cmdVersion(args []string, out io.Writer) error {
	fs, _, js := flagSet("version")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r := buildinfo.Collect(proxyFeatures())
	if *js {
		return printJSON(out, r)
	}
	fmt.Fprintf(out, "swanix %s (%s)\n", r.Version, r.Profile)
	fmt.Fprintf(out, "fork commit:     %s\n", r.Commit)
	fmt.Fprintf(out, "upstream:        sing-box %s (%s)\n", r.Upstream, r.UpstreamCommit)
	fmt.Fprintf(out, "go:              %s %s/%s cgo=%s\n", r.GoVersion, r.OS, r.Arch, r.CGO)
	if r.OpenWrt != "" {
		fmt.Fprintf(out, "openwrt series:  %s\n", r.OpenWrt)
	}
	if len(r.Verified) == 0 {
		fmt.Fprintln(out, "verified scope:  none recorded for this build")
	} else {
		fmt.Fprintf(out, "verified scope:  %s\n", strings.Join(r.Verified, ", "))
	}
	return nil
}

func cmdFeatures(args []string, out io.Writer) error {
	fs, _, js := flagSet("features")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r := buildinfo.Collect(proxyFeatures())
	if *js {
		return printJSON(out, r)
	}
	fmt.Fprintf(out, "profile: %s\nmultiwan: %s\n", r.Profile, strings.Join(r.MultiWAN, ", "))
	if len(r.Proxy) == 0 {
		fmt.Fprintln(out, "proxy:    (not compiled in)")
	} else {
		fmt.Fprintf(out, "proxy:    %s\n", strings.Join(r.Proxy, ", "))
	}
	fmt.Fprintf(out, "build tags: %s\n", strings.Join(r.BuildTags, ","))
	return nil
}

func cmdCheck(args []string, out io.Writer) error {
	fs, path, _ := flagSet("check")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	if cfg.Proxy.Enabled {
		if err := checkProxy(cfg); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "OK: %s — %d 条 WAN，%d 个策略，%d 条规则（%s 构建）\n", *path, len(cfg.WANs), len(cfg.Policies), len(cfg.Rules), profile.Name)
	return nil
}

func doctorEnv(cfg *config.Config) *doctor.Env {
	_, err := os.Stat(cfg.Kernel.ControlSocket)
	return &doctor.Env{Cfg: cfg, Runner: kernel.ExecRunner{}, Netif: netif.NewUbusSource(), SwanixRunning: err == nil}
}

func cmdDoctor(args []string, out io.Writer) error {
	fs, path, js := flagSet("doctor")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	checks := doctor.Run(doctorEnv(cfg))
	if *js {
		if err := printJSON(out, checks); err != nil {
			return err
		}
	} else {
		for _, c := range checks {
			fmt.Fprintf(out, "[%-4s] %s: %s\n", c.Level, c.Name, c.Detail)
			if c.Fix != "" && c.Level != doctor.OK {
				fmt.Fprintf(out, "       → %s\n", c.Fix)
			}
		}
	}
	if doctor.HasFailure(checks) {
		return fmt.Errorf("doctor 发现阻止启用的问题")
	}
	return nil
}

func cmdPreview(args []string, out io.Writer) error {
	fs, path, _ := flagSet("preview")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	in, snap, err := engine.Preview(cfg, netif.NewUbusSource())
	if err != nil {
		return err
	}
	rs, err := in.Render()
	if err != nil {
		return err
	}
	plan, err := in.BuildRoutePlan()
	if err != nil {
		return err
	}
	for _, p := range snap.Problems {
		fmt.Fprintln(out, "# 问题:", p)
	}
	for _, w := range rs.Warnings {
		fmt.Fprintln(out, "# 提示:", w)
	}
	fmt.Fprintln(out, "# --- ip rule ---")
	for _, r := range plan.Rules {
		fmt.Fprintf(out, "ip -%d rule add %s priority %d %s\n", r.Family, r.Match, r.Priority, r.Action)
	}
	fmt.Fprintln(out, "# --- ip route ---")
	for _, r := range plan.Routes {
		fmt.Fprintf(out, "ip -%d route replace %s table %d\n", r.Family, r.Spec, r.Table)
	}
	fmt.Fprintln(out, "# --- nft ---")
	fmt.Fprint(out, rs.Script)
	return nil
}

func cmdRun(args []string) error {
	fs, path, _ := flagSet("run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	if !cfg.Global.Enabled {
		fmt.Fprintln(os.Stderr, "swanix: global.enabled=0，不启动")
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runDaemon(ctx, cfg)
}

func socketPath() string {
	if cfg, err := config.LoadFile(defaultConfig); err == nil {
		return cfg.Kernel.ControlSocket
	}
	return config.Defaults().Kernel.ControlSocket
}

func cmdStatus(args []string, out io.Writer) error {
	fs, _, js := flagSet("status")
	if err := fs.Parse(args); err != nil {
		return err
	}
	raw, err := control.Call(socketPath(), control.MethodStatus)
	if err != nil {
		return err
	}
	if *js {
		_, err := fmt.Fprintln(out, string(raw))
		return err
	}
	var st telemetry.Status
	if err := json.Unmarshal(raw, &st); err != nil {
		return err
	}
	fmt.Fprintf(out, "swanix %s (%s)  模式: %s  接管: %v\n", st.Version, st.Profile, st.Mode, st.Managing)
	for _, w := range st.WANs {
		fmt.Fprintf(out, "\n%s  [%s/%s]  容量 ↓%.0f/↑%.0f Mbps  实时 ↓%.1f/↑%.1f Mbps  已分配新连接 %d\n",
			w.Name, w.Network, w.Device, w.DownloadMbps, w.UploadMbps, w.RXMbps, w.TXMbps, w.Assigned)
		for _, f := range w.Families {
			fmt.Fprintf(out, "  IPv%d %-10s RTT %.0f ms (基线 %.0f)  探测失败率 %.0f%%  新连接份额 %v  %s\n",
				f.Family, f.State, f.RTTms, f.BaseMs, f.ProbeFailureRate*100, f.Share, f.Reason)
		}
	}
	if st.Rejected > 0 {
		fmt.Fprintf(out, "\n被拒绝的新连接: %d\n", st.Rejected)
	}
	for _, p := range st.Problems {
		fmt.Fprintln(out, "问题:", p)
	}
	return nil
}

func cmdHistory(args []string, out io.Writer) error {
	fs, _, _ := flagSet("history")
	if err := fs.Parse(args); err != nil {
		return err
	}
	raw, err := control.Call(socketPath(), control.MethodHistory)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(raw))
	return err
}

func cmdReconcile(args []string) error {
	_, err := control.Call(socketPath(), control.MethodPoke)
	return err
}

// verifyBudget is how long apply waits for a line to pass a real probe (--verify).
var verifyBudget = session.VerifyBudget

func newSession(cfgPath string) *session.Session {
	h := session.Hooks{
		Verify:       func() error { return verifyRunning(socketPath(), verifyBudget) },
		RemoveKernel: func() error { return removeKernel(cfgPath) },
		SpawnGuard:   func() (int, error) { return spawnGuard(cfgPath) },
		KillGuard:    func(pid int) { _ = syscall.Kill(pid, syscall.SIGTERM) },
	}
	return session.New(kernel.ExecRunner{}, runDir, cfgPath, h)
}

// verifyRunning waits for the daemon to take over and for at least one WAN to
// pass real probes, so a broken switch is rolled back before the operator is
// asked to confirm.
func verifyRunning(sock string, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	var last error
	for time.Now().Before(deadline) {
		raw, err := control.Call(sock, control.MethodStatus)
		if err == nil {
			var st telemetry.Status
			if err := json.Unmarshal(raw, &st); err == nil {
				if !st.Managing {
					return fmt.Errorf("守护进程未处于 manage 模式（startup_mode=%s）", st.Mode)
				}
				for _, w := range st.WANs {
					for _, f := range w.Families {
						if f.Family == 4 && (f.State == "healthy" || f.State == "recovering" || f.State == "degraded") {
							return nil
						}
					}
				}
				last = fmt.Errorf("还没有任何 WAN 通过探测")
			}
		} else {
			last = err
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("验证超时: %v", last)
}

func removeKernel(cfgPath string) error {
	cfg, err := config.LoadFile(cfgPath)
	if err != nil {
		return err
	}
	return kernel.NewApplier().Remove(&kernel.Input{Cfg: cfg})
}

func spawnGuard(cfgPath string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(exe, "guard", "-c", cfgPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// the guard outlives this process: keep its diagnostics in a file instead of a closed pipe
	if err := os.MkdirAll(runDir, 0o750); err == nil {
		if lf, err := os.OpenFile(filepath.Join(runDir, "guard.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			cmd.Stdout, cmd.Stderr = lf, lf
			defer lf.Close()
		}
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

func cmdApply(args []string, out io.Writer) error {
	fs, path, _ := flagSet("apply")
	timeout := fs.Duration("timeout", 0, "")
	verify := fs.Duration("verify", session.VerifyBudget, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *verify < 5*time.Second || *verify > 10*time.Minute {
		return fmt.Errorf("--verify 必须在 5s 与 10m 之间")
	}
	verifyBudget = *verify
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	if !cfg.Global.Enabled || cfg.Global.StartupMode != config.StartupManage {
		return fmt.Errorf("apply 需要 global.enabled=1 且 startup_mode=manage；observe 模式不写入内核，直接 run 即可")
	}
	checks := doctor.Run(doctorEnv(cfg))
	if doctor.HasFailure(checks) {
		for _, c := range checks {
			if c.Level == doctor.Fail {
				fmt.Fprintf(out, "[fail] %s: %s\n       → %s\n", c.Name, c.Detail, c.Fix)
			}
		}
		return fmt.Errorf("doctor 未通过，拒绝切换")
	}
	d := *timeout
	if d == 0 {
		d = time.Duration(cfg.Global.ConfirmTimeout) * time.Second
	}
	sess := newSession(*path)
	sess.Verify = verifyBudget
	st, err := sess.Apply(d)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "已切换到 swanix。请在 %s 前运行 `swanix confirm`，否则将自动回滚。\n", st.Deadline.Format("15:04:05"))
	return nil
}

func cmdConfirm(out io.Writer) error {
	if err := newSession(defaultConfig).Confirm(); err != nil {
		return err
	}
	fmt.Fprintln(out, "已确认，swanix 已设为开机自启。")
	return nil
}

func cmdRollback(out io.Writer) error {
	if err := newSession(defaultConfig).Rollback("手动回滚"); err != nil {
		return err
	}
	fmt.Fprintln(out, "已回滚。")
	return nil
}

func cmdGuard(args []string) error {
	fs, path, _ := flagSet("guard")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, err := newSession(*path).Guard()
	return err
}

func cmdRemove(args []string) error {
	fs, path, _ := flagSet("remove")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return removeKernel(*path)
}
