// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

//go:build windows

package docker

import (
	"errors"

	docker "github.com/fsouza/go-dockerclient"
)

// Currently Windows containers don't support host ip in port binding.
func getPortBinding(ip string, port string) docker.PortBinding {
	return docker.PortBinding{HostIP: "", HostPort: port}
}

func tweakCapabilities(basics, adds, drops []string) ([]string, error) {
	return nil, nil
}

var containerAdminErrMsg = "running container as ContainerAdmin is unsafe; change the container user, set task configuration to privileged or enable windows_allow_insecure_container_admin to disable this check"

func validateImageUser(user, taskUser string, taskDriverConfig *TaskConfig, driverConfig *DriverConfig) error {
	// we're only interested in the case where isolation is set to "process"
	// (it's also the default) and when windows_allow_insecure_container_admin
	// is explicitly set to true in the config
	if driverConfig.WindowsAllowInsecureContainerAdmin || taskDriverConfig.Isolation == "hyper-v" {
		return nil
	}

	if user == "ContainerAdmin" && (taskUser == "ContainerAdmin" || taskUser == "") && !taskDriverConfig.Privileged {
		return errors.New(containerAdminErrMsg)
	}
	return nil
}
