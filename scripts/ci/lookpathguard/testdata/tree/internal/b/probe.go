package b

import "os/exec"

func haveZenity() bool {
	_, err := exec.LookPath("zenity")
	return err == nil
}
