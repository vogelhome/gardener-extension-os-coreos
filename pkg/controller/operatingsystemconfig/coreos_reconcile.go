// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package operatingsystemconfig

import (
	"context"
	_ "embed"
	"encoding/base64"
	"fmt"
	"strconv"

	actuatorutil "github.com/gardener/gardener/extensions/pkg/controller/operatingsystemconfig/oscommon/actuator"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"

	"github.com/gardener/gardener-extension-os-coreos/pkg/controller/operatingsystemconfig/coreos"
)

var (
	coreOSCloudInitCommand = "/usr/bin/coreos-cloudinit --from-file="
)

func (a *actuator) legacyReconcile(ctx context.Context, config *extensionsv1alpha1.OperatingSystemConfig) ([]byte, *string, []string, []string, error) {
	cloudConfig, units, files, err := a.cloudConfigFromOperatingSystemConfig(ctx, config)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("could not generate cloud config: %v", err)
	}

	var command *string
	if path := config.Spec.ReloadConfigFilePath; path != nil {
		cmd := coreOSCloudInitCommand + *path
		command = &cmd
	}

	return []byte(cloudConfig), command, units, files, nil
}

func (a *actuator) cloudConfigFromOperatingSystemConfig(ctx context.Context, config *extensionsv1alpha1.OperatingSystemConfig) (string, []string, []string, error) {
	cloudConfig := &coreos.CloudConfig{
		CoreOS: coreos.Config{
			Update: coreos.Update{
				RebootStrategy: "off",
			},
			Units: []coreos.Unit{
				{
					Name:    "update-engine.service",
					Mask:    true,
					Command: "stop",
				},
				{
					Name:    "locksmithd.service",
					Mask:    true,
					Command: "stop",
				},
			},
		},
	}

	// blacklist sctp kernel module
	if config.Spec.Purpose == extensionsv1alpha1.OperatingSystemConfigPurposeReconcile {
		cloudConfig.WriteFiles = []coreos.File{
			{
				Encoding:           "b64",
				Content:            base64.StdEncoding.EncodeToString([]byte("install sctp /bin/true")),
				Owner:              "root",
				Path:               "/etc/modprobe.d/sctp.conf",
				RawFilePermissions: "0644",
			},
		}
	}

	unitNames := make([]string, 0, len(config.Spec.Units))
	for _, unit := range config.Spec.Units {
		unitNames = append(unitNames, unit.Name)

		u := coreos.Unit{Name: unit.Name}

		if unit.Command != nil {
			u.Command = string(*unit.Command)
		}
		if unit.Enable != nil {
			u.Enable = *unit.Enable
		}
		if unit.Content != nil {
			u.Content = *unit.Content
		}

		for _, dropIn := range unit.DropIns {
			u.DropIns = append(u.DropIns, coreos.UnitDropIn{
				Name:    dropIn.Name,
				Content: dropIn.Content,
			})
		}

		cloudConfig.CoreOS.Units = append(cloudConfig.CoreOS.Units, u)
	}

	filePaths := make([]string, 0, len(config.Spec.Files))
	for _, file := range config.Spec.Files {
		filePaths = append(filePaths, file.Path)
		f := coreos.File{
			Path: file.Path,
		}

		permissions := extensionsv1alpha1.OperatingSystemConfigDefaultFilePermission
		if p := file.Permissions; p != nil {
			permissions = *p
		}
		f.RawFilePermissions = strconv.FormatInt(int64(permissions), 8)

		rawContent, err := actuatorutil.DataForFileContent(ctx, a.client, config.Namespace, &file.Content)
		if err != nil {
			return "", nil, nil, err
		}

		if file.Content.TransmitUnencoded != nil && *file.Content.TransmitUnencoded {
			f.Content = string(rawContent)
			f.Encoding = ""
		} else {
			f.Encoding = "b64"
			f.Content = base64.StdEncoding.EncodeToString(rawContent)
		}

		cloudConfig.WriteFiles = append(cloudConfig.WriteFiles, f)
	}

	if isContainerdEnabled(config.Spec.CRIConfig) && config.Spec.Purpose == extensionsv1alpha1.OperatingSystemConfigPurposeProvision {
		cloudConfig.CoreOS.Units = append(
			cloudConfig.CoreOS.Units,
			coreos.Unit{
				Name:    "run-command.service",
				Command: "start",
				Enable:  true,
				Content: `[Unit]
Description=Oneshot unit used to run a script on node start-up.
Before=containerd.service kubelet.service
[Service]
Type=oneshot
EnvironmentFile=/etc/environment
ExecStart=/opt/bin/run-command.sh
[Install]
WantedBy=containerd.service kubelet.service
`,
			})

		unitNames = append(unitNames, "run-command.service")

		cloudConfig.WriteFiles = append(
			cloudConfig.WriteFiles,
			coreos.File{
				Path:               "/etc/systemd/system/containerd.service.d/11-exec_config.conf",
				RawFilePermissions: "0644",
				Content: `[Service]
SyslogIdentifier=containerd
ExecStart=
ExecStart=/bin/bash -c 'PATH="/run/torcx/unpack/docker/bin:$PATH" /run/torcx/unpack/docker/bin/containerd --config /etc/containerd/config.toml'
`,
			},
			coreos.File{
				Path:               "/opt/bin/run-command.sh",
				RawFilePermissions: "0755",
				Content:            containerdTemplateContent,
			})

	}

	names, err := enableCGroupsV2(cloudConfig)
	if err != nil {
		return "", nil, nil, err
	}
	unitNames = append(unitNames, names...)

	data, err := cloudConfig.String()
	if err != nil {
		return "", nil, nil, err
	}

	return data, unitNames, filePaths, nil
}

func isContainerdEnabled(criConfig *extensionsv1alpha1.CRIConfig) bool {
	if criConfig == nil {
		return false
	}

	return criConfig.Name == extensionsv1alpha1.CRINameContainerD
}

func enableCGroupsV2(cloudConfig *coreos.CloudConfig) ([]string, error) {
	var additionalUnitNames []string

	cloudConfig.CoreOS.Units = append(
		cloudConfig.CoreOS.Units,
		coreos.Unit{
			Name:    "enable-cgroupsv2.service",
			Command: "start",
			Enable:  true,
			Content: `[Unit]
Description=Oneshot unit used to patch the kubelet config for cgroupsv2.
Before=containerd.service kubelet.service
[Service]
Type=oneshot
EnvironmentFile=/etc/environment
ExecStart=/opt/bin/configure-cgroupsv2.sh
[Install]
WantedBy=containerd.service kubelet.service
`,
		})
	additionalUnitNames = append(additionalUnitNames, "enable-cgroupsv2.service")

	cloudConfig.WriteFiles = append(
		cloudConfig.WriteFiles,
		coreos.File{
			Path:               "/opt/bin/configure-cgroupsv2.sh",
			RawFilePermissions: "0755",
			Content:            cgroupsv2TemplateContent,
		})

	return additionalUnitNames, nil
}
