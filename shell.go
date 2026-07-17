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
	name string   // tool/display name: "bash", "zsh", "powershell", "pwsh", "cmd"
	exe  string   // executable path
	args []string // args before the command string, e.g. ["-c"] or ["/c"]
}

// detectedShell is set at startup by detectShell().
var detectedShell shellInfo

// detectShell determines which shell to use for the shell tool.
//
// Detection strategy (similar to Claude Code and Codex):
//  1. $SHELL env var — set by the terminal to the user's login shell.
//     On Windows, Git Bash sets this; native terminals don't.
//  2. Platform default — PowerShell on Windows, bash on Unix.
//  3. Ultimate fallback — cmd on Windows, /bin/sh on Unix.
func detectShell() shellInfo {
	// 1. Try $SHELL env var.
	if shellPath := os.Getenv("SHELL"); shellPath != "" {
		if s := shellFromPath(shellPath); s != nil {
			slog.Info("detected shell from $SHELL", "name", s.name, "exe", s.exe)
			return *s
		}
	}

	// 2. Platform default.
	if runtime.GOOS == "windows" {
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
	} else {
		// On Unix, try bash then zsh.
		if p, err := exec.LookPath("bash"); err == nil {
			s := shellInfo{"bash", p, []string{"-c"}}
			slog.Info("detected shell (unix default)", "name", s.name, "exe", s.exe)
			return s
		}
		if p, err := exec.LookPath("zsh"); err == nil {
			s := shellInfo{"zsh", p, []string{"-c"}}
			slog.Info("detected shell (unix default)", "name", s.name, "exe", s.exe)
			return s
		}
	}

	// 3. Ultimate fallback.
	if runtime.GOOS == "windows" {
		s := shellInfo{"cmd", "cmd", []string{"/c"}}
		slog.Info("detected shell (fallback)", "name", s.name, "exe", s.exe)
		return s
	}
	s := shellInfo{"sh", "/bin/sh", []string{"-c"}}
	slog.Info("detected shell (fallback)", "name", s.name, "exe", s.exe)
	return s
}

// shellFromPath maps a shell binary path (e.g. "/bin/zsh", "pwsh") to a
// shellInfo, or nil if the shell type is unrecognized or the binary doesn't
// exist.
func shellFromPath(path string) *shellInfo {
	name := strings.ToLower(filepath.Base(path))
	// Strip .exe on Windows.
	name = strings.TrimSuffix(name, ".exe")

	switch name {
	case "bash":
		return &shellInfo{"bash", path, []string{"-c"}}
	case "zsh":
		return &shellInfo{"zsh", path, []string{"-c"}}
	case "sh":
		return &shellInfo{"sh", path, []string{"-c"}}
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
