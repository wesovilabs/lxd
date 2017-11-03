// +build linux

package osarch

import (
	"github.com/wesovilabs/lxd/shared"
)

// ArchitectureGetLocal returns the local hardware architecture
func ArchitectureGetLocal() (string, error) {
	uname, err := shared.Uname()
	if err != nil {
		return ArchitectureDefault, err
	}

	return uname.Machine, nil
}
