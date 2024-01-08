// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package exec2

import (
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
)

const (
	name          = "exec2"
	version       = "v2.0.0"
	handleVersion = 1
)

var info = &base.PluginInfoResponse{
	Type:              base.PluginTypeDriver,
	PluginApiVersions: []string{drivers.ApiVersion010},
	PluginVersion:     version,
	Name:              name,
}

var driverConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
	// TODO
})

var taskConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
	"command": hclspec.NewAttr("command", "string", true),
	"args":    hclspec.NewAttr("args", "list(string)", false),
	"unveil":  hclspec.NewAttr("unveil", "list(string)", false),
})

var capabilities = &drivers.Capabilities{
	SendSignals:         true,
	Exec:                false,
	FSIsolation:         drivers.FSIsolationNone,
	MustInitiateNetwork: false,
	MountConfigs:        drivers.MountConfigSupportNone,
	RemoteTasks:         false,
	NetIsolationModes: []drivers.NetIsolationMode{
		drivers.NetIsolationModeNone,
		drivers.NetIsolationModeHost,
		drivers.NetIsolationModeGroup,
	},
}

// Config represents the exec2 driver plugin configuration that gets set in
// the Nomad client configuration file.
type Config struct {
	// TODO
}

// TaskConfig represents the exec2 driver task configuration that gets set in
// a Nomad job file.
type TaskConfig struct {
	Command string   `codec:"command"`
	Args    []string `codec:"args"`
	Unveil  []string `codec:"unveil"`
}

// probably no need for a parseOptions; the exec task config is fairly simple
