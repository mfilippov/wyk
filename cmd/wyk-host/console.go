//go:build windows

package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// The console side of install and uninstall: how this exe reports to the
// person who started it. Nothing here touches the distro.

// usage says how to install and exits with code. Started by a double click,
// it waits for Enter: the console opened for it closes with it.
func usage(code int) {
	exe := filepath.Base(os.Args[0]) // as downloaded, wyk-<version>-<arch>.exe
	fmt.Fprintf(os.Stderr, `usage: %[1]s install [-d distro] [-u user] [-no-import]
       %[1]s uninstall [-d distro] [-u user]
       %[1]s version
Run it from PowerShell or cmd. Once installed, WSL starts it on its own.
`, exe)
	quit(code)
}

// fail reports an error and quits.
func fail(format string, v ...any) {
	log.Printf(format, v...)
	quit(1)
}

// quit exits with code. In a console opened just for this process, it first
// waits for Enter, so what it printed can be read before the console closes.
func quit(code int) {
	if ownConsole() {
		fmt.Fprint(os.Stderr, "\nPress Enter to close.")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n') // any outcome closes
	}
	os.Exit(code)
}

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	user32   = syscall.NewLazyDLL("user32.dll")
)

// ownConsole reports whether this process is alone in a visible console
// that was opened for it, as when Explorer or a browser's downloads list
// starts it. A hidden one has nobody to press Enter. WSL interop gives the
// Windows programs it starts a console of their own too, but waits for them.
func ownConsole() bool {
	var pids [2]uint32
	n, _, _ := kernel32.NewProc("GetConsoleProcessList").Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids))) //nolint:gosec // G103: the API's out array
	if n != 1 || startedFromWSL() {
		return false
	}
	w, _, _ := kernel32.NewProc("GetConsoleWindow").Call()
	visible, _, _ := user32.NewProc("IsWindowVisible").Call(w)
	return w != 0 && visible != 0
}

// startedFromWSL reports whether WSL interop started this process.
func startedFromWSL() bool {
	parent := strings.ToLower(parentName())
	return parent == "wsl.exe" || parent == "wslhost.exe"
}

// parentName is the exe name of the parent process, or "" if unknown. It
// trusts the parent pid, which a process started after the parent exited
// could have taken over.
func parentName() string {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return ""
	}
	defer func() { _ = syscall.CloseHandle(snap) }() // nothing to do about it
	self := uint32(os.Getpid())                      //nolint:gosec // G115: Windows process ids are DWORDs
	var parent uint32
	names := map[uint32]string{}
	e := syscall.ProcessEntry32{Size: uint32(unsafe.Sizeof(syscall.ProcessEntry32{}))}
	for err = syscall.Process32First(snap, &e); err == nil; err = syscall.Process32Next(snap, &e) {
		names[e.ProcessID] = syscall.UTF16ToString(e.ExeFile[:])
		if e.ProcessID == self {
			parent = e.ParentProcessID
		}
	}
	return names[parent]
}
