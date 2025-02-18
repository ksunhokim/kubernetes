/*
Copyright 2016 The Kubernetes Authors.

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

package azure_dd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2018-04-01/compute"
	"github.com/golang/glog"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/kubernetes/pkg/cloudprovider/providers/azure"
	"k8s.io/kubernetes/pkg/util/keymutex"
	"k8s.io/kubernetes/pkg/util/mount"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/util"
)

type azureDiskDetacher struct {
	plugin *azureDataDiskPlugin
	cloud  *azure.Cloud
}

type azureDiskAttacher struct {
	plugin *azureDataDiskPlugin
	cloud  *azure.Cloud
}

var _ volume.Attacher = &azureDiskAttacher{}
var _ volume.Detacher = &azureDiskDetacher{}

var _ volume.DeviceMounter = &azureDiskAttacher{}
var _ volume.DeviceUnmounter = &azureDiskDetacher{}

// acquire lock to get an lun number
var getLunMutex = keymutex.NewHashed(0)

// Attach attaches a volume.Spec to an Azure VM referenced by NodeName, returning the disk's LUN
func (a *azureDiskAttacher) Attach(spec *volume.Spec, nodeName types.NodeName) (string, error) {
	volumeSource, _, err := getVolumeSource(spec)
	if err != nil {
		glog.Warningf("failed to get azure disk spec (%v)", err)
		return "", err
	}

	instanceid, err := a.cloud.InstanceID(context.TODO(), nodeName)
	if err != nil {
		glog.Warningf("failed to get azure instance id (%v)", err)
		return "", fmt.Errorf("failed to get azure instance id for node %q (%v)", nodeName, err)
	}

	diskController, err := getDiskController(a.plugin.host)
	if err != nil {
		return "", err
	}

	lun, err := diskController.GetDiskLun(volumeSource.DiskName, volumeSource.DataDiskURI, nodeName)
	if err == cloudprovider.InstanceNotFound {
		// Log error and continue with attach
		glog.Warningf(
			"Error checking if volume is already attached to current node (%q). Will continue and try attach anyway. err=%v",
			instanceid, err)
	}

	if err == nil {
		// Volume is already attached to node.
		glog.V(2).Infof("Attach operation is successful. volume %q is already attached to node %q at lun %d.", volumeSource.DiskName, instanceid, lun)
	} else {
		glog.V(2).Infof("GetDiskLun returned: %v. Initiating attaching volume %q to node %q.", err, volumeSource.DataDiskURI, nodeName)
		getLunMutex.LockKey(instanceid)
		defer getLunMutex.UnlockKey(instanceid)

		lun, err = diskController.GetNextDiskLun(nodeName)
		if err != nil {
			glog.Warningf("no LUN available for instance %q (%v)", nodeName, err)
			return "", fmt.Errorf("all LUNs are used, cannot attach volume %q to instance %q (%v)", volumeSource.DiskName, instanceid, err)
		}
		glog.V(2).Infof("Trying to attach volume %q lun %d to node %q.", volumeSource.DataDiskURI, lun, nodeName)
		isManagedDisk := (*volumeSource.Kind == v1.AzureManagedDisk)
		err = diskController.AttachDisk(isManagedDisk, volumeSource.DiskName, volumeSource.DataDiskURI, nodeName, lun, compute.CachingTypes(*volumeSource.CachingMode))
		if err == nil {
			glog.V(2).Infof("Attach operation successful: volume %q attached to node %q.", volumeSource.DataDiskURI, nodeName)
		} else {
			glog.V(2).Infof("Attach volume %q to instance %q failed with %v", volumeSource.DataDiskURI, instanceid, err)
			return "", fmt.Errorf("attach volume %q to instance %q failed with %v", volumeSource.DiskName, instanceid, err)
		}
	}

	return strconv.Itoa(int(lun)), err
}

func (a *azureDiskAttacher) VolumesAreAttached(specs []*volume.Spec, nodeName types.NodeName) (map[*volume.Spec]bool, error) {
	volumesAttachedCheck := make(map[*volume.Spec]bool)
	volumeSpecMap := make(map[string]*volume.Spec)
	volumeIDList := []string{}
	for _, spec := range specs {
		volumeSource, _, err := getVolumeSource(spec)
		if err != nil {
			glog.Errorf("azureDisk - Error getting volume (%q) source : %v", spec.Name(), err)
			continue
		}

		volumeIDList = append(volumeIDList, volumeSource.DiskName)
		volumesAttachedCheck[spec] = true
		volumeSpecMap[volumeSource.DiskName] = spec
	}

	diskController, err := getDiskController(a.plugin.host)
	if err != nil {
		return nil, err
	}
	attachedResult, err := diskController.DisksAreAttached(volumeIDList, nodeName)
	if err != nil {
		// Log error and continue with attach
		glog.Errorf(
			"azureDisk - Error checking if volumes (%v) are attached to current node (%q). err=%v",
			volumeIDList, nodeName, err)
		return volumesAttachedCheck, err
	}

	for volumeID, attached := range attachedResult {
		if !attached {
			spec := volumeSpecMap[volumeID]
			volumesAttachedCheck[spec] = false
			glog.V(2).Infof("azureDisk - VolumesAreAttached: check volume %q (specName: %q) is no longer attached", volumeID, spec.Name())
		}
	}
	return volumesAttachedCheck, nil
}

func (a *azureDiskAttacher) WaitForAttach(spec *volume.Spec, devicePath string, _ *v1.Pod, timeout time.Duration) (string, error) {
	volumeSource, _, err := getVolumeSource(spec)
	if err != nil {
		return "", err
	}

	diskController, err := getDiskController(a.plugin.host)
	if err != nil {
		return "", err
	}

	nodeName := types.NodeName(a.plugin.host.GetHostName())
	diskName := volumeSource.DiskName

	var lun int32
	if runtime.GOOS == "windows" {
		glog.V(2).Infof("azureDisk - WaitForAttach: begin to GetDiskLun by diskName(%s), DataDiskURI(%s), nodeName(%s), devicePath(%s)",
			diskName, volumeSource.DataDiskURI, nodeName, devicePath)
		lun, err = diskController.GetDiskLun(diskName, volumeSource.DataDiskURI, nodeName)
		if err != nil {
			return "", err
		}
		glog.V(2).Infof("azureDisk - WaitForAttach: GetDiskLun succeeded, got lun(%v)", lun)
	} else {
		lun, err = getDiskLUN(devicePath)
		if err != nil {
			return "", err
		}
	}

	exec := a.plugin.host.GetExec(a.plugin.GetPluginName())

	io := &osIOHandler{}
	scsiHostRescan(io, exec)

	newDevicePath := ""

	err = wait.Poll(1*time.Second, timeout, func() (bool, error) {
		if newDevicePath, err = findDiskByLun(int(lun), io, exec); err != nil {
			return false, fmt.Errorf("azureDisk - WaitForAttach ticker failed node (%s) disk (%s) lun(%v) err(%s)", nodeName, diskName, lun, err)
		}

		// did we find it?
		if newDevicePath != "" {
			return true, nil
		}

		return false, fmt.Errorf("azureDisk - WaitForAttach failed within timeout node (%s) diskId:(%s) lun:(%v)", nodeName, diskName, lun)
	})

	return newDevicePath, err
}

// to avoid name conflicts (similar *.vhd name)
// we use hash diskUri and we use it as device mount target.
// this is generalized for both managed and blob disks
// we also prefix the hash with m/b based on disk kind
func (a *azureDiskAttacher) GetDeviceMountPath(spec *volume.Spec) (string, error) {
	volumeSource, _, err := getVolumeSource(spec)
	if err != nil {
		return "", err
	}

	if volumeSource.Kind == nil { // this spec was constructed from info on the node
		pdPath := filepath.Join(a.plugin.host.GetPluginDir(azureDataDiskPluginName), mount.MountsInGlobalPDPath, volumeSource.DataDiskURI)
		return pdPath, nil
	}

	isManagedDisk := (*volumeSource.Kind == v1.AzureManagedDisk)
	return makeGlobalPDPath(a.plugin.host, volumeSource.DataDiskURI, isManagedDisk)
}

func (attacher *azureDiskAttacher) MountDevice(spec *volume.Spec, devicePath string, deviceMountPath string) error {
	mounter := attacher.plugin.host.GetMounter(azureDataDiskPluginName)
	notMnt, err := mounter.IsLikelyNotMountPoint(deviceMountPath)

	if err != nil {
		if os.IsNotExist(err) {
			dir := deviceMountPath
			if runtime.GOOS == "windows" {
				// in windows, as we use mklink, only need to MkdirAll for parent directory
				dir = filepath.Dir(deviceMountPath)
			}
			if err := os.MkdirAll(dir, 0750); err != nil {
				return fmt.Errorf("azureDisk - mountDevice:CreateDirectory failed with %s", err)
			}
			notMnt = true
		} else {
			return fmt.Errorf("azureDisk - mountDevice:IsLikelyNotMountPoint failed with %s", err)
		}
	}

	if !notMnt {
		// testing original mount point, make sure the mount link is valid
		if _, err := (&osIOHandler{}).ReadDir(deviceMountPath); err != nil {
			// mount link is invalid, now unmount and remount later
			glog.Warningf("azureDisk - ReadDir %s failed with %v, unmount this directory", deviceMountPath, err)
			if err := mounter.Unmount(deviceMountPath); err != nil {
				glog.Errorf("azureDisk - Unmount deviceMountPath %s failed with %v", deviceMountPath, err)
				return err
			}
			notMnt = true
		}
	}

	volumeSource, _, err := getVolumeSource(spec)
	if err != nil {
		return err
	}

	options := []string{}
	if notMnt {
		diskMounter := util.NewSafeFormatAndMountFromHost(azureDataDiskPluginName, attacher.plugin.host)
		mountOptions := util.MountOptionFromSpec(spec, options...)
		err = diskMounter.FormatAndMount(devicePath, deviceMountPath, *volumeSource.FSType, mountOptions)
		if err != nil {
			if cleanErr := os.Remove(deviceMountPath); cleanErr != nil {
				return fmt.Errorf("azureDisk - mountDevice:FormatAndMount failed with %s and clean up failed with :%v", err, cleanErr)
			}
			return fmt.Errorf("azureDisk - mountDevice:FormatAndMount failed with %s", err)
		}
	}
	return nil
}

// Detach detaches disk from Azure VM.
func (d *azureDiskDetacher) Detach(diskURI string, nodeName types.NodeName) error {
	if diskURI == "" {
		return fmt.Errorf("invalid disk to detach: %q", diskURI)
	}

	instanceid, err := d.cloud.InstanceID(context.TODO(), nodeName)
	if err != nil {
		glog.Warningf("no instance id for node %q, skip detaching (%v)", nodeName, err)
		return nil
	}

	glog.V(2).Infof("detach %v from node %q", diskURI, nodeName)

	diskController, err := getDiskController(d.plugin.host)
	if err != nil {
		return err
	}

	getLunMutex.LockKey(instanceid)
	defer getLunMutex.UnlockKey(instanceid)

	err = diskController.DetachDiskByName("", diskURI, nodeName)
	if err != nil {
		glog.Errorf("failed to detach azure disk %q, err %v", diskURI, err)
	}

	glog.V(2).Infof("azureDisk - disk:%s was detached from node:%v", diskURI, nodeName)
	return err
}

// UnmountDevice unmounts the volume on the node
func (detacher *azureDiskDetacher) UnmountDevice(deviceMountPath string) error {
	err := util.UnmountPath(deviceMountPath, detacher.plugin.host.GetMounter(detacher.plugin.GetPluginName()))
	if err == nil {
		glog.V(2).Infof("azureDisk - Device %s was unmounted", deviceMountPath)
	} else {
		glog.Warningf("azureDisk - Device %s failed to unmount with error: %s", deviceMountPath, err.Error())
	}
	return err
}
