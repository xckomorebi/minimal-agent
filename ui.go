package main

import (
	"fmt"
	"strings"
)

func dim(s string) string  { return "\033[2m\033[3m" + s + "\033[0m" }
func youPrefix() string    { return "\033[1m\033[36myou>\033[0m " }
func agentPrefix() string  { return "\033[1m\033[32magent>\033[0m " }
func thinkingPrefix() string { return "\033[1m\033[35mthinking>\033[0m " }
func toolDot() string      { return "\033[1m\033[33m●\033[0m " }
func toolLabel(name string) string { return "\033[1m\033[33m" + name + "\033[0m" }
func red(s string) string  { return "\033[31m" + s + "\033[0m" }
func green(s string) string { return "\033[1m\033[32m" + s + "\033[0m" }

func onOff(v bool) string {
	if v {
		return green("on")
	}
	return red("off")
}

func sourceLabel(fromSession, fromGlobal bool) string {
	if fromSession {
		return dim("(session)")
	}
	if fromGlobal {
		return dim("(config file)")
	}
	return dim("(default)")
}

func banner(model, session string) {
	lines := []string{
		"minimal-agent",
		"model   : " + model,
		"session : " + session,
		"Ctrl-C to quit",
	}
	width := 0
	for _, l := range lines {
		if len(l) > width {
			width = len(l)
		}
	}
	width += 4
	pad := func(s string) string {
		return "  " + s + strings.Repeat(" ", width-2-len(s))
	}
	top := "╭" + strings.Repeat("─", width) + "╮"
	btm := "╰" + strings.Repeat("─", width) + "╯"
	fmt.Println("\n" + top)
	for _, l := range lines {
		fmt.Println("│" + pad(l) + "│")
	}
	fmt.Println(btm)
}
