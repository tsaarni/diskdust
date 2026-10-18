//go:build linux && (amd64 || s390x)

package main

import "syscall"

const fstatatTrap = syscall.SYS_NEWFSTATAT
