package gocl

// Linux syscall numbers, per architecture.
//
// goa already owns the x86-64 table (asm.go: linuxSyscalls) because that is the
// architecture its assembler emits a `mov rax,N; syscall` stub for. Those
// numbers are x86-64's alone: the same syscall is a different number on every
// other Linux architecture, and reusing the x86-64 value is silently wrong --
// `movz x8, #1; svc #0` on AArch64 is not write(2), so the program runs to
// completion while producing no output. Hence this table.
//
// The numbering is the one the architecture's own kernel ABI defines:
//
//   - aarch64, riscv64, riscv32 use the "asm-generic" table, the arch-agnostic
//     numbering every architecture that did not keep a legacy one settled on.
//     (AArch64 write is 64, exit_group is 94.)
//   - arm (32-bit) kept the older ARM EABI numbers, where write is 4 and
//     exit_group is 248.
//   - x86_64 is answered from goa's own table, which is the authoritative copy.
//
// A syscall absent from a table is deliberately NOT given a number: the
// corresponding symbol is then left undefined and the link reports it by name,
// which is honest, rather than trapping with a wrong number. The tables
// therefore carry the calls the C library actually makes on a normal run; the
// rest of the library's syscalls are filled in as the port reaches them.

import "goa"

// genericSyscalls is the asm-generic numbering (AArch64 / RISC-V).
var genericSyscalls = map[string]int64{
	"read": 63, "write": 64, "readv": 65, "writev": 66,
	"close": 57, "stat": 79, "fstat": 80, "lstat": 78,
	"lseek": 62, "mmap": 222, "mprotect": 226, "munmap": 215, "brk": 214,
	"ioctl": 29, "rt_sigaction": 134, "rt_sigprocmask": 135, "sigaltstack": 132,
	"nanosleep": 101, "clock_gettime": 228, "gettimeofday": 169,
	"getpid": 172, "gettid": 178, "kill": 129, "exit": 93, "exit_group": 94,
	"getcwd": 17, "access": 21, "getdents64": 61, "chmod": 90,
	"unlinkat": 35, "rename": 82, "mkdir": 83, "rmdir": 84,
	"futex": 98, "sched_yield": 124, "clone": 220, "execve": 221, "wait4": 260,
	// asm-generic has no plain stat/lstat/getdents/select: 78 is readlinkat,
	// 79 is newfstatat and 142 is reboot. Giving them numbers here made a
	// stat() call land in readlinkat() and silently fail, so they are left
	// out -- the link then reports the missing name instead.
	"socket": 198, "bind": 200, "listen": 201, "connect": 203,
	"getsockname": 204, "getpeername": 205, "sendto": 206, "recvfrom": 207,
	"shutdown": 210, "accept": 242, "setsockopt": 208,
}

// arm32Syscalls is the ARM EABI numbering (32-bit arm).
var arm32Syscalls = map[string]int64{
	// 0 is restart_syscall on ARM EABI; read is 3. A read() built with 0
	// entered the restart helper instead and never returned any data, so
	// every read path (fread, scanf, getchar) came back empty with no error.
	"read": 3, "write": 4, "open": 5, "close": 6, "lseek": 19,
	"exit": 1, "exit_group": 248, "getpid": 20, "gettid": 224, "kill": 37,
	"ioctl": 54, "writev": 146, "brk": 45, "mmap": 192, "munmap": 91,
	"mprotect": 125, "rt_sigaction": 174, "rt_sigprocmask": 175,
	"nanosleep": 162, "clock_gettime": 263, "gettimeofday": 78, "getcwd": 12,
	"access": 33, "getdents64": 217, "chmod": 15, "rename": 38, "mkdir": 39,
	"rmdir": 40, "futex": 240, "clone": 120, "execve": 11,
	// 142 is _newselect on ARM EABI (and so is the right number for select);
	// sched_yield is 158. Sharing one number between two names was the tell.
	"sched_yield": 158,
	"wait4":       114, "select": 142, "stat": 106, "fstat": 108,
}

// archSyscallTable returns the name -> number table for an architecture.
// x86_64 is answered by archSyscallNumber straight from goa, not from here.
func archSyscallTable(arch string) map[string]int64 {
	switch arch {
	case "aarch64", "riscv64", "riscv32":
		return genericSyscalls
	case "arm", "armel":
		return arm32Syscalls
	}
	return nil
}

// archSyscallNumber returns the syscall number for name on arch.
//
// It reports false for a name the architecture's table does not carry, which is
// the caller's signal to leave the symbol undefined rather than invent a number.
func archSyscallNumber(arch, name string) (int64, bool) {
	if arch == "x86_64" {
		// goa's table is the x86-64 one and is authoritative -- it is what the
		// native back end's own stubs are generated from. Reading it here
		// rather than copying it keeps the two halves from disagreeing about
		// what `write` is; a second table would be a second thing to forget.
		return goa.LinuxSyscallNumber(name)
	}
	t := archSyscallTable(arch)
	if t == nil {
		return 0, false
	}
	n, ok := t[name]
	return n, ok
}
