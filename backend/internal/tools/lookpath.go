package tools

import "os/exec"

func defaultLookPath(name string) (string, error) { return exec.LookPath(name) }
