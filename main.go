package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/cgroups/manager"
	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runc/libcontainer/system"
)

const (
	magicCmd   = "__init"
	cgroupName = "cmemstat"
)

func init() {
	args := os.Args[1:]

	// careful, this branch should always os.Exit or we fork bomb
	if len(args) >= 2 && strings.HasPrefix(args[0], magicCmd) {
		if strings.HasSuffix(args[0], "d") {
			slog.SetLogLoggerLevel(slog.LevelDebug)
		}

		childLogger := slog.With(slog.String("process", "child"))

		childLogger.Debug("starting", "args", args)

		path, err := exec.LookPath(args[1])
		if err != nil {
			childLogger.Error("failed retrieving path for binary", "args", args, "error", err)
			os.Exit(1)
		}

		childLogger.Debug("stopping")
		err = syscall.Kill(os.Getpid(), syscall.SIGSTOP)
		if err != nil {
			childLogger.Error("failed sending SIGSTOP", "pid", os.Getpid(), "error", err)
			os.Exit(2)
		}

		childLogger.Debug("continuing, starting exec")
		err = system.Exec(path, args[1:], os.Environ())
		if err != nil {
			childLogger.Error("exec failed", "path", path, "args", args[1:])
			os.Exit(3)
		}

		os.Exit(0)
	}
}

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "enable debug logs")
	var systemdCgroup bool
	flag.BoolVar(&systemdCgroup, "systemd-cgroup", true, "use Systemd cgroup driver to create rootless cgroups, see https://isogo.to/systemd-cgroup-driver")
	var cmdOut, cmdErr string
	flag.StringVar(&cmdOut, "cmdout", os.Stderr.Name(), "standard ouput file of the child command")
	flag.StringVar(&cmdErr, "cmderr", os.Stderr.Name(), "standard error file of the child command")
	var refreshPeriod time.Duration
	flag.DurationVar(&refreshPeriod, "refresh", 500*time.Millisecond, "polling period for memory stats")
	flag.Parse()

	parentLogger := slog.With(slog.String("process", "parent"))

	args := flag.CommandLine.Args()
	if len(args) < 1 {
		parentLogger.Error("not enough args, please provide a command to execute")
		flag.Usage()
		os.Exit(1)
	}

	magicCmdWithOptions := magicCmd
	if debug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
		// pass suffix "d" to child to add debug logs
		magicCmdWithOptions = magicCmdWithOptions + "d"
	}

	args = append([]string{magicCmdWithOptions}, args...)
	cmd := exec.Command("/proc/self/exe", args...)

	cmdOutFile, err := os.OpenFile(cmdOut, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		parentLogger.Error("failed to open out file", "file", cmdOut, "error", err)
		os.Exit(1)
	}
	cmd.Stdout = cmdOutFile

	cmdErrFile, err := os.OpenFile(cmdErr, os.O_TRUNC|os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		parentLogger.Error("failed to open err file", "file", cmdErr, "error", err)
		os.Exit(1)
	}
	cmd.Stderr = cmdErrFile

	err = cmd.Start()
	if err != nil {
		parentLogger.Error("failed to start the cmd", "error", err)
		os.Exit(1)
	}

	// Wait for child to have stopped itself. WUNTRACED, despite the name, also
	// returns if a child has stopped, but not especially traced via ptrace.
	var ws syscall.WaitStatus
	pid, err := syscall.Wait4(cmd.Process.Pid, &ws, syscall.WUNTRACED, nil)
	if err != nil {
		parentLogger.Error("failed to wait4", "error", err)
		os.Exit(1)
	}
	if pid != cmd.Process.Pid {
		panic("return from wait4 with wrong pid, this should never happen")
	}
	if ws.Exited() {
		parentLogger.Error("child exited early", "exit_code", ws.ExitStatus())
		os.Exit(1)
	}
	if !ws.Stopped() {
		parentLogger.Error("child didn't stopped itself")
		os.Exit(1)
	}

	// create the cgroup and place the child inside it
	cm, err := manager.New(&configs.Cgroup{
		Name:     cgroupName,
		Systemd:  systemdCgroup,
		Rootless: systemdCgroup,
	})
	if err != nil {
		parentLogger.Error("failed to create the cgroup manager", "error", err)
		os.Exit(1)
	}
	if cm.Exists() {
		parentLogger.Debug("cgroup already exists, destroying cgroup", "path", cm.GetPaths())
		err := cm.Destroy()
		if err != nil {
			parentLogger.Error("failed to destroy cgroup", "error", err)
			os.Exit(1)
		}
	}
	err = cm.Apply(cmd.Process.Pid)
	parentLogger.Debug("create cgroups and put child it in", "pid", cmd.Process.Pid, "paths", cm.GetPaths())
	if err != nil {
		parentLogger.Error("failed to apply cgroup", "pid", cmd.Process.Pid, "error", err)
		os.Exit(1)
	}
	defer cm.Destroy()

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	// spawn a goroutine for child termination
	deadChild := make(chan struct{})
	go func() {
		// wait in case child dies early
		err := cmd.Wait()
		if err != nil {
			parentLogger.Error("failed to wait for child", "pid", cmd.Process.Pid, "error", err)
		}
		close(deadChild)
	}()

	start := time.Now()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', tabwriter.AlignRight)
	if cgroups.IsCgroup2UnifiedMode() {
		go func() {
			fmt.Fprintln(w, "uptime\tanon\tfile\tkernel\tusage\tworkingset\t")
			for {
				stats, err := cm.GetStats()
				if err != nil {
					parentLogger.Error("failed to get stats", "error", err)
				} else {
					s := stats.MemoryStats.Stats
					fmt.Fprintf(w, "%10s\t%12d\t%12d\t%12d\t%12d\t%12d\t\n", time.Since(start).Round(time.Millisecond), s["anon"], s["file"], s["kernel"], stats.MemoryStats.Usage.Usage, stats.MemoryStats.Usage.Usage-s["inactive_file"])
					w.Flush()
				}
				time.Sleep(refreshPeriod)
			}
		}()
	} else {
		go func() {
			fmt.Fprintln(w, "uptime\trss\tcache\tkernel\tusage\tworkingset\t")
			for {
				stats, err := cm.GetStats()
				if err != nil {
					parentLogger.Error("failed to get stats", "error", err)
				} else {
					s := stats.MemoryStats.Stats
					fmt.Fprintf(w, "%10s\t%12d\t%12d\t%12d\t%12d\t%12d\t\n", time.Since(start).Round(time.Millisecond), s["rss"], s["cache"], stats.MemoryStats.KernelUsage.Usage, stats.MemoryStats.Usage.Usage, stats.MemoryStats.Usage.Usage-s["total_inactive_file"])
					w.Flush()
				}
				time.Sleep(refreshPeriod)
			}
		}()
	}

	// resume the child
	parentLogger.Debug("sending SIGCONT to child", "pid", cmd.Process.Pid)
	err = cmd.Process.Signal(syscall.SIGCONT)
	if err != nil {
		parentLogger.Error("failed to resume the child", "pid", cmd.Process.Pid, "error", err)
		os.Exit(1)
	}

	select {
	case <-deadChild:
		parentLogger.Debug("child exited early", "exit_code", cmd.ProcessState.ExitCode())
		return
	case <-shutdown:
		parentLogger.Debug("received interrupt signal, shutting down child", "pid", cmd.Process.Pid)
		// gracefully kill the child before shutting down
		err := syscall.Kill(cmd.Process.Pid, syscall.SIGINT)
		if err != nil {
			parentLogger.Error("failed to kill the child", "pid", cmd.Process.Pid, "error", err)
			os.Exit(1)
		}
		<-deadChild
		return
	}
}
