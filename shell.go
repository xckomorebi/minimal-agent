package main

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// shellInfo describes the shell used by the shell tool.
type shellInfo struct {
	name string   // tool/display name: "bash", "powershell", "pwsh", "cmd", "sh"
	exe  string   // executable path
	args []string // args before the command string, e.g. ["-c"] or ["/c"]
}

// detectedShell is set at startup by detectShell().
var detectedShell shellInfo

// detectShell determines which shell to use for the shell tool.
//
// Detection strategy:
//   - Unix: bash (POSIX standard, near-universal), fallback /bin/sh.
//     The agent always uses bash regardless of the user's login shell.
//   - Windows: $SHELL env var (set by Git Bash), then pwsh, powershell,
//     fallback cmd.
func detectShell() shellInfo {
	if runtime.GOOS == "windows" {
		// Git Bash sets $SHELL; native Windows terminals don't.
		if shellPath := os.Getenv("SHELL"); shellPath != "" {
			if s := shellFromPath(shellPath); s != nil {
				slog.Info("detected shell from $SHELL", "name", s.name, "exe", s.exe)
				return *s
			}
		}
		if p, err := exec.LookPath("pwsh"); err == nil {
			s := shellInfo{"pwsh", p, []string{"-NoProfile", "-Command"}}
			slog.Info("detected shell (windows default)", "name", s.name, "exe", s.exe)
			return s
		}
		if p, err := exec.LookPath("powershell"); err == nil {
			s := shellInfo{"powershell", p, []string{"-NoProfile", "-Command"}}
			slog.Info("detected shell (windows default)", "name", s.name, "exe", s.exe)
			return s
		}
		s := shellInfo{"cmd", "cmd", []string{"/c"}}
		slog.Info("detected shell (fallback)", "name", s.name, "exe", s.exe)
		return s
	}

	// Unix: bash is the POSIX standard and near-universal.
	if p, err := exec.LookPath("bash"); err == nil {
		s := shellInfo{"bash", p, []string{"-c"}}
		slog.Info("detected shell", "name", s.name, "exe", s.exe)
		return s
	}
	s := shellInfo{"sh", "/bin/sh", []string{"-c"}}
	slog.Info("detected shell (fallback)", "name", s.name, "exe", s.exe)
	return s
}

// shellFromPath maps a shell binary path (e.g. "bash", "pwsh") to a
// shellInfo, or nil if the shell type is unrecognized. Used on Windows to
// detect Git Bash via the $SHELL env var.
func shellFromPath(path string) *shellInfo {
	name := strings.ToLower(filepath.Base(path))
	// Strip .exe on Windows.
	name = strings.TrimSuffix(name, ".exe")

	switch name {
	case "bash":
		return &shellInfo{"bash", path, []string{"-c"}}
	case "pwsh":
		return &shellInfo{"pwsh", path, []string{"-NoProfile", "-Command"}}
	case "powershell":
		return &shellInfo{"powershell", path, []string{"-NoProfile", "-Command"}}
	case "cmd":
		return &shellInfo{"cmd", path, []string{"/c"}}
	}
	return nil
}

// osName returns a human-readable OS name for the system prompt.
func osName() string {
	switch runtime.GOOS {
	case "windows":
		return "Windows"
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	default:
		return runtime.GOOS
	}
}
