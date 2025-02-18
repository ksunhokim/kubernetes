/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package flexvolume

import (
	"fmt"
	"os"

	"github.com/golang/glog"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/util"
)

type flexVolumeDetacher struct {
	plugin *flexVolumeAttachablePlugin
}

var _ volume.Detacher = &flexVolumeDetacher{}

var _ volume.DeviceUnmounter = &flexVolumeDetacher{}

// Detach is part of the volume.Detacher interface.
func (d *flexVolumeDetacher) Detach(volumeName string, hostName types.NodeName) error {

	call := d.plugin.NewDriverCall(detachCmd)
	call.Append(volumeName)
	call.Append(string(hostName))

	_, err := call.Run()
	if isCmdNotSupportedErr(err) {
		return (*detacherDefaults)(d).Detach(volumeName, hostName)
	}
	return err
}

// UnmountDevice is part of the volume.Detacher interface.
func (d *flexVolumeDetacher) UnmountDevice(deviceMountPath string) error {

	pathExists, pathErr := util.PathExists(deviceMountPath)
	if !pathExists {
		glog.Warningf("Warning: Unmount skipped because path does not exist: %v", deviceMountPath)
		return nil
	}
	if pathErr != nil && !util.IsCorruptedMnt(pathErr) {
		return fmt.Errorf("error checking path: %v", pathErr)
	}

	notmnt, err := isNotMounted(d.plugin.host.GetMounter(d.plugin.GetPluginName()), deviceMountPath)
	if err != nil {
		if util.IsCorruptedMnt(err) {
			notmnt = false // Corrupted error is assumed to be mounted.
		} else {
			return err
		}
	}

	if notmnt {
		glog.Warningf("Warning: Path: %v already unmounted", deviceMountPath)
	} else {
		call := d.plugin.NewDriverCall(unmountDeviceCmd)
		call.Append(deviceMountPath)

		_, err := call.Run()
		if isCmdNotSupportedErr(err) {
			err = (*detacherDefaults)(d).UnmountDevice(deviceMountPath)
		}
		if err != nil {
			return err
		}
	}

	// Flexvolume driver may remove the directory. Ignore if it does.
	if pathExists, pathErr := util.PathExists(deviceMountPath); pathErr != nil {
		return fmt.Errorf("error checking if path exists: %v", pathErr)
	} else if !pathExists {
		return nil
	}
	return os.Remove(deviceMountPath)
}
