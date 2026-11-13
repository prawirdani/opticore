package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prawirdani/opticore/internal/config"
)

// pidFile is where serve records itself so `opticore stop` and `serve -d` can
// find one daemon without guessing. OPTICORE_PID_FILE overrides it; otherwise
// it follows the log file, or sits beside the binary.
func pidFile(cfgEnv func(string) string) string {
	if p := strings.TrimSpace(cfgEnv("OPTICORE_PID_FILE")); p != "" {
		return p
	}
	if p := strings.TrimSpace(cfgEnv("OPTICORE_LOG_FILE")); p != "" {
		return p + ".pid"
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "opticore.pid")
	}
	return "opticore.pid"
}

// writePID records this process, creating the directory if needed.
func writePID(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("pid file %s: %w", path, err)
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
}

// removePID deletes the pid file only when it still names this process, so a
// restart that already claimed it is left alone.
func removePID(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if strings.TrimSpace(string(b)) == strconv.Itoa(os.Getpid()) {
		os.Remove(path)
	}
}

// errAlreadyRunning reports a second serve for the same database, which the
// PostgreSQL advisory lock would refuse anyway; checking first gives a better
// message than the lock.
func errAlreadyRunning(pid int) error {
	return fmt.Errorf("opticore serve is already running (pid %d); stop it with `opticore stop`", pid)
}

// livePID returns the pid from path when that process is still running.
func livePID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 || !processAlive(pid) {
		return 0
	}
	return pid
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// isServe reports whether pid runs an opticore daemon, by reading its argv.
// Linux keeps it in /proc; elsewhere `ps` is the only portable source.
func isServe(pid int) bool {
	return isServeArgs(processArgs(pid))
}

// isServeArgs is the argv half of isServe, so it can be tested without a
// process: an opticore binary whose first argument is serve.
func isServeArgs(args []string) bool {
	if len(args) == 0 {
		return false
	}
	base := filepath.Base(strings.ReplaceAll(args[0], `\`, "/"))
	if base != "opticore" && base != "opticore.exe" {
		return false
	}
	for _, a := range args[1:] {
		if a == "serve" {
			return true
		}
	}
	return false
}

func processArgs(pid int) []string {
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); err == nil {
		var out []string
		for _, f := range strings.Split(string(b), "\x00") {
			if f != "" {
				out = append(out, f)
			}
		}
		return out
	}
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil
	}
	if f := strings.Fields(strings.TrimSpace(string(out))); len(f) > 0 {
		return f
	}
	return nil
}

// servePIDs lists running opticore daemons: the pid file first, then a scan of
// the process table, so `stop` also finds a daemon started before the pid file
// existed or with a different OPTICORE_PID_FILE.
func servePIDs(path string) []int {
	var out []int
	seen := map[int]bool{}
	add := func(pid int) {
		if pid > 0 && !seen[pid] && isServe(pid) {
			seen[pid] = true
			out = append(out, pid)
		}
	}
	add(livePID(path))
	if entries, err := os.ReadDir("/proc"); err == nil {
		for _, e := range entries {
			if pid, err := strconv.Atoi(e.Name()); err == nil {
				add(pid)
			}
		}
	}
	return out
}

// cmdStop stops every running opticore serve with SIGTERM and waits for the
// daemon's own graceful shutdown.
func cmdStop(env func(string) string, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: stop")
	}
	path := pidFile(env)
	pids := servePIDs(path)
	if len(pids) == 0 {
		fmt.Println("no opticore serve is running")
		removeIfStale(path)
		return nil
	}
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("stop pid %d: %w", pid, err)
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		alive := 0
		for _, pid := range pids {
			if processAlive(pid) {
				alive++
			}
		}
		if alive == 0 {
			for _, pid := range pids {
				fmt.Printf("stopped opticore serve (pid %d)\n", pid)
			}
			removeIfStale(path)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("still running after 30s: %v", pids)
}

// removeIfStale drops a pid file whose process is gone.
func removeIfStale(path string) {
	if livePID(path) == 0 {
		os.Remove(path)
	}
}

// serveDaemon starts serve in a detached child and returns once it is up. The
// child runs the same binary without -d, with its own session so a closing
// terminal or a Ctrl-C on the parent does not reach it. The pid file is
// written by the child, so it always names the real daemon.
func serveDaemon(cfg config.Config) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	path := pidFile(os.Getenv)
	if pid := livePID(path); pid != 0 {
		return errAlreadyRunning(pid)
	}
	args := []string{"serve"}
	if envFile := os.Getenv("OPTICORE_ENV_FILE"); envFile != "" {
		args = []string{"-env-file", envFile, "serve"}
	}

	// The child inherits the environment, which is where its configuration
	// comes from; real variables win over the env file in either case.
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = os.Environ()
	if cfg.LogFile == "" {
		devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer devnull.Close()
		cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
		fmt.Println("OPTICORE_LOG_FILE is not set: the daemon's logs are discarded")
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		return err
	}

	// Give it time to fail on the database lock or a bad listener, so -d
	// reports a broken start instead of claiming success.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pid := livePID(path); pid != 0 {
			fmt.Printf("opticore serve started (pid %d)\n", pid)
			if cfg.LogFile != "" {
				fmt.Printf("logs: %s\n", cfg.LogFile)
			}
			return nil
		}
		if !processAlive(pid) {
			return fmt.Errorf("serve exited during startup; check %s", logHint(cfg))
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("serve did not come up in 5s; check %s", logHint(cfg))
}

func logHint(cfg config.Config) string {
	if cfg.LogFile != "" {
		return cfg.LogFile
	}
	return "the startup output"
}
