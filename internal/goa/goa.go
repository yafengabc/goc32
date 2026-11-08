// Package goa is a stub for the two assembler queries the IR translator
// makes. The TinyOS back ends never take the Linux-syscall path, so this
// package exists only to keep the trimmed IR module self-contained.
package goa

func IsLinuxSyscall(name string) bool { return false }

func LinuxSyscallNumber(name string) (int64, bool) { return 0, false }
