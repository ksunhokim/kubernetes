/*
Copyright 2014 The Kubernetes Authors.

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

package awsebs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/kubernetes/pkg/cloudprovider/providers/aws"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/util/mount"
	kstrings "k8s.io/kubernetes/pkg/util/strings"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/util"
)

// ProbeVolumePlugins is the primary entrypoint for volume plugins.
func ProbeVolumePlugins() []volume.VolumePlugin {
	return []volume.VolumePlugin{&awsElasticBlockStorePlugin{nil}}
}

type awsElasticBlockStorePlugin struct {
	host volume.VolumeHost
}

var _ volume.VolumePlugin = &awsElasticBlockStorePlugin{}
var _ volume.PersistentVolumePlugin = &awsElasticBlockStorePlugin{}
var _ volume.DeletableVolumePlugin = &awsElasticBlockStorePlugin{}
var _ volume.ProvisionableVolumePlugin = &awsElasticBlockStorePlugin{}

const (
	awsElasticBlockStorePluginName = "kubernetes.io/aws-ebs"
	awsURLNamePrefix               = "aws://"
)

func getPath(uid types.UID, volName string, host volume.VolumeHost) string {
	return host.GetPodVolumeDir(uid, kstrings.EscapeQualifiedNameForDisk(awsElasticBlockStorePluginName), volName)
}

func (plugin *awsElasticBlockStorePlugin) Init(host volume.VolumeHost) error {
	plugin.host = host
	return nil
}

func (plugin *awsElasticBlockStorePlugin) GetPluginName() string {
	return awsElasticBlockStorePluginName
}

func (plugin *awsElasticBlockStorePlugin) GetVolumeName(spec *volume.Spec) (string, error) {
	volumeSource, _, err := getVolumeSource(spec)
	if err != nil {
		return "", err
	}

	return volumeSource.VolumeID, nil
}

func (plugin *awsElasticBlockStorePlugin) CanSupport(spec *volume.Spec) bool {
	return (spec.PersistentVolume != nil && spec.PersistentVolume.Spec.AWSElasticBlockStore != nil) ||
		(spec.Volume != nil && spec.Volume.AWSElasticBlockStore != nil)
}

func (plugin *awsElasticBlockStorePlugin) RequiresRemount() bool {
	return false
}

func (plugin *awsElasticBlockStorePlugin) SupportsMountOption() bool {
	return true
}

func (plugin *awsElasticBlockStorePlugin) SupportsBulkVolumeVerification() bool {
	return true
}

func (plugin *awsElasticBlockStorePlugin) GetVolumeLimits() (map[string]int64, error) {
	volumeLimits := map[string]int64{
		util.EBSVolumeLimitKey: util.DefaultMaxEBSVolumes,
	}
	cloud := plugin.host.GetCloudProvider()

	// if we can't fetch cloudprovider we return an error
	// hoping external CCM or admin can set it. Returning
	// default values from here will mean, no one can
	// override them.
	if cloud == nil {
		return nil, fmt.Errorf("no cloudprovider present")
	}

	if cloud.ProviderName() != aws.ProviderName {
		return nil, fmt.Errorf("expected aws cloud, found %s", cloud.ProviderName())
	}

	instances, ok := cloud.Instances()
	if !ok {
		glog.V(3).Infof("Failed to get instances from cloud provider")
		return volumeLimits, nil
	}

	instanceType, err := instances.InstanceType(context.TODO(), plugin.host.GetNodeName())
	if err != nil {
		glog.Errorf("Failed to get instance type from AWS cloud provider")
		return volumeLimits, nil
	}

	if ok, _ := regexp.MatchString(util.EBSNitroLimitRegex, instanceType); ok {
		volumeLimits[util.EBSVolumeLimitKey] = util.DefaultMaxEBSNitroVolumeLimit
	}

	return volumeLimits, nil
}

func (plugin *awsElasticBlockStorePlugin) VolumeLimitKey(spec *volume.Spec) string {
	return util.EBSVolumeLimitKey
}

func (plugin *awsElasticBlockStorePlugin) GetAccessModes() []v1.PersistentVolumeAccessMode {
	return []v1.PersistentVolumeAccessMode{
		v1.ReadWriteOnce,
	}
}

func (plugin *awsElasticBlockStorePlugin) NewMounter(spec *volume.Spec, pod *v1.Pod, _ volume.VolumeOptions) (volume.Mounter, error) {
	// Inject real implementations here, test through the internal function.
	return plugin.newMounterInternal(spec, pod.UID, &AWSDiskUtil{}, plugin.host.GetMounter(plugin.GetPluginName()))
}

func (plugin *awsElasticBlockStorePlugin) newMounterInternal(spec *volume.Spec, podUID types.UID, manager ebsManager, mounter mount.Interface) (volume.Mounter, error) {
	// EBSs used directly in a pod have a ReadOnly flag set by the pod author.
	// EBSs used as a PersistentVolume gets the ReadOnly flag indirectly through the persistent-claim volume used to mount the PV
	ebs, readOnly, err := getVolumeSource(spec)
	if err != nil {
		return nil, err
	}

	volumeID := aws.KubernetesVolumeID(ebs.VolumeID)
	fsType := ebs.FSType
	partition := ""
	if ebs.Partition != 0 {
		partition = strconv.Itoa(int(ebs.Partition))
	}

	return &awsElasticBlockStoreMounter{
		awsElasticBlockStore: &awsElasticBlockStore{
			podUID:          podUID,
			volName:         spec.Name(),
			volumeID:        volumeID,
			partition:       partition,
			manager:         manager,
			mounter:         mounter,
			plugin:          plugin,
			MetricsProvider: volume.NewMetricsStatFS(getPath(podUID, spec.Name(), plugin.host)),
		},
		fsType:       fsType,
		readOnly:     readOnly,
		diskMounter:  util.NewSafeFormatAndMountFromHost(plugin.GetPluginName(), plugin.host),
		mountOptions: util.MountOptionFromSpec(spec),
	}, nil
}

func (plugin *awsElasticBlockStorePlugin) NewUnmounter(volName string, podUID types.UID) (volume.Unmounter, error) {
	// Inject real implementations here, test through the internal function.
	return plugin.newUnmounterInternal(volName, podUID, &AWSDiskUtil{}, plugin.host.GetMounter(plugin.GetPluginName()))
}

func (plugin *awsElasticBlockStorePlugin) newUnmounterInternal(volName string, podUID types.UID, manager ebsManager, mounter mount.Interface) (volume.Unmounter, error) {
	return &awsElasticBlockStoreUnmounter{&awsElasticBlockStore{
		podUID:          podUID,
		volName:         volName,
		manager:         manager,
		mounter:         mounter,
		plugin:          plugin,
		MetricsProvider: volume.NewMetricsStatFS(getPath(podUID, volName, plugin.host)),
	}}, nil
}

func (plugin *awsElasticBlockStorePlugin) NewDeleter(spec *volume.Spec) (volume.Deleter, error) {
	return plugin.newDeleterInternal(spec, &AWSDiskUtil{})
}

func (plugin *awsElasticBlockStorePlugin) newDeleterInternal(spec *volume.Spec, manager ebsManager) (volume.Deleter, error) {
	if spec.PersistentVolume != nil && spec.PersistentVolume.Spec.AWSElasticBlockStore == nil {
		glog.Errorf("spec.PersistentVolumeSource.AWSElasticBlockStore is nil")
		return nil, fmt.Errorf("spec.PersistentVolumeSource.AWSElasticBlockStore is nil")
	}
	return &awsElasticBlockStoreDeleter{
		awsElasticBlockStore: &awsElasticBlockStore{
			volName:  spec.Name(),
			volumeID: aws.KubernetesVolumeID(spec.PersistentVolume.Spec.AWSElasticBlockStore.VolumeID),
			manager:  manager,
			plugin:   plugin,
		}}, nil
}

func (plugin *awsElasticBlockStorePlugin) NewProvisioner(options volume.VolumeOptions) (volume.Provisioner, error) {
	return plugin.newProvisionerInternal(options, &AWSDiskUtil{})
}

func (plugin *awsElasticBlockStorePlugin) newProvisionerInternal(options volume.VolumeOptions, manager ebsManager) (volume.Provisioner, error) {
	return &awsElasticBlockStoreProvisioner{
		awsElasticBlockStore: &awsElasticBlockStore{
			manager: manager,
			plugin:  plugin,
		},
		options: options,
	}, nil
}

func getVolumeSource(
	spec *volume.Spec) (*v1.AWSElasticBlockStoreVolumeSource, bool, error) {
	if spec.Volume != nil && spec.Volume.AWSElasticBlockStore != nil {
		return spec.Volume.AWSElasticBlockStore, spec.Volume.AWSElasticBlockStore.ReadOnly, nil
	} else if spec.PersistentVolume != nil &&
		spec.PersistentVolume.Spec.AWSElasticBlockStore != nil {
		return spec.PersistentVolume.Spec.AWSElasticBlockStore, spec.ReadOnly, nil
	}

	return nil, false, fmt.Errorf("spec does not reference an AWS EBS volume type")
}

func (plugin *awsElasticBlockStorePlugin) ConstructVolumeSpec(volName, mountPath string) (*volume.Spec, error) {
	mounter := plugin.host.GetMounter(plugin.GetPluginName())
	pluginDir := plugin.host.GetPluginDir(plugin.GetPluginName())
	volumeID, err := mounter.GetDeviceNameFromMount(mountPath, pluginDir)
	if err != nil {
		return nil, err
	}
	// This is a workaround to fix the issue in converting aws volume id from globalPDPath
	// There are three aws volume id formats and their volumeID from GetDeviceNameFromMount() are:
	// aws:///vol-1234 (aws/vol-1234)
	// aws://us-east-1/vol-1234 (aws/us-east-1/vol-1234)
	// vol-1234 (vol-1234)
	// This code is for converting volume id to aws style volume id for the first two cases.
	sourceName := volumeID
	if strings.HasPrefix(volumeID, "aws/") {
		names := strings.Split(volumeID, "/")
		length := len(names)
		if length < 2 || length > 3 {
			return nil, fmt.Errorf("failed to get AWS volume id from mount path %q: invalid volume name format %q", mountPath, volumeID)
		}
		volName := names[length-1]
		if !strings.HasPrefix(volName, "vol-") {
			return nil, fmt.Errorf("invalid volume name format for AWS volume (%q) retrieved from mount path %q", volName, mountPath)
		}
		if length == 2 {
			sourceName = awsURLNamePrefix + "" + "/" + volName // empty zone label
		}
		if length == 3 {
			sourceName = awsURLNamePrefix + names[1] + "/" + volName // names[1] is the zone label
		}
		glog.V(4).Infof("Convert aws volume name from %q to %q ", volumeID, sourceName)
	}

	awsVolume := &v1.Volume{
		Name: volName,
		VolumeSource: v1.VolumeSource{
			AWSElasticBlockStore: &v1.AWSElasticBlockStoreVolumeSource{
				VolumeID: sourceName,
			},
		},
	}
	return volume.NewSpecFromVolume(awsVolume), nil
}

func (plugin *awsElasticBlockStorePlugin) RequiresFSResize() bool {
	return true
}

func (plugin *awsElasticBlockStorePlugin) ExpandVolumeDevice(
	spec *volume.Spec,
	newSize resource.Quantity,
	oldSize resource.Quantity) (resource.Quantity, error) {
	var awsVolume aws.Volumes

	awsVolume, err := getCloudProvider(plugin.host.GetCloudProvider())

	if err != nil {
		return oldSize, err
	}
	// we don't expect to receive this call for non PVs
	rawVolumeName := spec.PersistentVolume.Spec.AWSElasticBlockStore.VolumeID
	volumeID := aws.KubernetesVolumeID(rawVolumeName)

	if volumeID == "" {
		return oldSize, fmt.Errorf("invalid volume id for %s", spec.Name())
	}
	return awsVolume.ResizeDisk(volumeID, oldSize, newSize)
}

var _ volume.ExpandableVolumePlugin = &awsElasticBlockStorePlugin{}
var _ volume.VolumePluginWithAttachLimits = &awsElasticBlockStorePlugin{}

// Abstract interface to PD operations.
type ebsManager interface {
	CreateVolume(provisioner *awsElasticBlockStoreProvisioner, node *v1.Node, allowedTopologies []v1.TopologySelectorTerm) (volumeID aws.KubernetesVolumeID, volumeSizeGB int, labels map[string]string, fstype string, err error)
	// Deletes a volume
	DeleteVolume(deleter *awsElasticBlockStoreDeleter) error
}

// awsElasticBlockStore volumes are disk resources provided by Amazon Web Services
// that are attached to the kubelet's host machine and exposed to the pod.
type awsElasticBlockStore struct {
	volName string
	podUID  types.UID
	// Unique id of the PD, used to find the disk resource in the provider.
	volumeID aws.KubernetesVolumeID
	// Specifies the partition to mount
	partition string
	// Utility interface that provides API calls to the provider to attach/detach disks.
	manager ebsManager
	// Mounter interface that provides system calls to mount the global path to the pod local path.
	mounter mount.Interface
	plugin  *awsElasticBlockStorePlugin
	volume.MetricsProvider
}

type awsElasticBlockStoreMounter struct {
	*awsElasticBlockStore
	// Filesystem type, optional.
	fsType string
	// Specifies whether the disk will be attached as read-only.
	readOnly bool
	// diskMounter provides the interface that is used to mount the actual block device.
	diskMounter  *mount.SafeFormatAndMount
	mountOptions []string
}

var _ volume.Mounter = &awsElasticBlockStoreMounter{}

func (b *awsElasticBlockStoreMounter) GetAttributes() volume.Attributes {
	return volume.Attributes{
		ReadOnly:        b.readOnly,
		Managed:         !b.readOnly,
		SupportsSELinux: true,
	}
}

// Checks prior to mount operations to verify that the required components (binaries, etc.)
// to mount the volume are available on the underlying node.
// If not, it returns an error
func (b *awsElasticBlockStoreMounter) CanMount() error {
	return nil
}

// SetUp attaches the disk and bind mounts to the volume path.
func (b *awsElasticBlockStoreMounter) SetUp(fsGroup *int64) error {
	return b.SetUpAt(b.GetPath(), fsGroup)
}

// SetUpAt attaches the disk and bind mounts to the volume path.
func (b *awsElasticBlockStoreMounter) SetUpAt(dir string, fsGroup *int64) error {
	// TODO: handle failed mounts here.
	notMnt, err := b.mounter.IsLikelyNotMountPoint(dir)
	glog.V(4).Infof("PersistentDisk set up: %s %v %v", dir, !notMnt, err)
	if err != nil && !os.IsNotExist(err) {
		glog.Errorf("cannot validate mount point: %s %v", dir, err)
		return err
	}
	if !notMnt {
		return nil
	}

	globalPDPath := makeGlobalPDPath(b.plugin.host, b.volumeID)

	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}

	// Perform a bind mount to the full path to allow duplicate mounts of the same PD.
	options := []string{"bind"}
	if b.readOnly {
		options = append(options, "ro")
	}
	mountOptions := util.JoinMountOptions(options, b.mountOptions)
	err = b.mounter.Mount(globalPDPath, dir, "", mountOptions)
	if err != nil {
		notMnt, mntErr := b.mounter.IsLikelyNotMountPoint(dir)
		if mntErr != nil {
			glog.Errorf("IsLikelyNotMountPoint check failed for %s: %v", dir, mntErr)
			return err
		}
		if !notMnt {
			if mntErr = b.mounter.Unmount(dir); mntErr != nil {
				glog.Errorf("failed to unmount %s: %v", dir, mntErr)
				return err
			}
			notMnt, mntErr := b.mounter.IsLikelyNotMountPoint(dir)
			if mntErr != nil {
				glog.Errorf("IsLikelyNotMountPoint check failed for %s: %v", dir, mntErr)
				return err
			}
			if !notMnt {
				// This is very odd, we don't expect it.  We'll try again next sync loop.
				glog.Errorf("%s is still mounted, despite call to unmount().  Will try again next sync loop.", dir)
				return err
			}
		}
		os.Remove(dir)
		glog.Errorf("Mount of disk %s failed: %v", dir, err)
		return err
	}

	if !b.readOnly {
		volume.SetVolumeOwnership(b, fsGroup)
	}

	glog.V(4).Infof("Successfully mounted %s", dir)
	return nil
}

func makeGlobalPDPath(host volume.VolumeHost, volumeID aws.KubernetesVolumeID) string {
	// Clean up the URI to be more fs-friendly
	name := string(volumeID)
	name = strings.Replace(name, "://", "/", -1)
	return filepath.Join(host.GetPluginDir(awsElasticBlockStorePluginName), mount.MountsInGlobalPDPath, name)
}

// Reverses the mapping done in makeGlobalPDPath
func getVolumeIDFromGlobalMount(host volume.VolumeHost, globalPath string) (string, error) {
	basePath := filepath.Join(host.GetPluginDir(awsElasticBlockStorePluginName), mount.MountsInGlobalPDPath)
	rel, err := filepath.Rel(basePath, globalPath)
	if err != nil {
		glog.Errorf("Failed to get volume id from global mount %s - %v", globalPath, err)
		return "", err
	}
	if strings.Contains(rel, "../") {
		glog.Errorf("Unexpected mount path: %s", globalPath)
		return "", fmt.Errorf("unexpected mount path: " + globalPath)
	}
	// Reverse the :// replacement done in makeGlobalPDPath
	volumeID := rel
	if strings.HasPrefix(volumeID, "aws/") {
		volumeID = strings.Replace(volumeID, "aws/", "aws://", 1)
	}
	glog.V(2).Info("Mapping mount dir ", globalPath, " to volumeID ", volumeID)
	return volumeID, nil
}

func (ebs *awsElasticBlockStore) GetPath() string {
	return getPath(ebs.podUID, ebs.volName, ebs.plugin.host)
}

type awsElasticBlockStoreUnmounter struct {
	*awsElasticBlockStore
}

var _ volume.Unmounter = &awsElasticBlockStoreUnmounter{}

// Unmounts the bind mount, and detaches the disk only if the PD
// resource was the last reference to that disk on the kubelet.
func (c *awsElasticBlockStoreUnmounter) TearDown() error {
	return c.TearDownAt(c.GetPath())
}

// Unmounts the bind mount
func (c *awsElasticBlockStoreUnmounter) TearDownAt(dir string) error {
	return util.UnmountPath(dir, c.mounter)
}

type awsElasticBlockStoreDeleter struct {
	*awsElasticBlockStore
}

var _ volume.Deleter = &awsElasticBlockStoreDeleter{}

func (d *awsElasticBlockStoreDeleter) GetPath() string {
	return getPath(d.podUID, d.volName, d.plugin.host)
}

func (d *awsElasticBlockStoreDeleter) Delete() error {
	return d.manager.DeleteVolume(d)
}

type awsElasticBlockStoreProvisioner struct {
	*awsElasticBlockStore
	options   volume.VolumeOptions
	namespace string
}

var _ volume.Provisioner = &awsElasticBlockStoreProvisioner{}

func (c *awsElasticBlockStoreProvisioner) Provision(selectedNode *v1.Node, allowedTopologies []v1.TopologySelectorTerm) (*v1.PersistentVolume, error) {
	if !util.AccessModesContainedInAll(c.plugin.GetAccessModes(), c.options.PVC.Spec.AccessModes) {
		return nil, fmt.Errorf("invalid AccessModes %v: only AccessModes %v are supported", c.options.PVC.Spec.AccessModes, c.plugin.GetAccessModes())
	}

	volumeID, sizeGB, labels, fstype, err := c.manager.CreateVolume(c, selectedNode, allowedTopologies)
	if err != nil {
		glog.Errorf("Provision failed: %v", err)
		return nil, err
	}

	if fstype == "" {
		fstype = "ext4"
	}

	var volumeMode *v1.PersistentVolumeMode
	if utilfeature.DefaultFeatureGate.Enabled(features.BlockVolume) {
		volumeMode = c.options.PVC.Spec.VolumeMode
		if volumeMode != nil && *volumeMode == v1.PersistentVolumeBlock {
			// Block volumes should not have any FSType
			fstype = ""
		}
	}

	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:   c.options.PVName,
			Labels: map[string]string{},
			Annotations: map[string]string{
				util.VolumeDynamicallyCreatedByKey: "aws-ebs-dynamic-provisioner",
			},
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: c.options.PersistentVolumeReclaimPolicy,
			AccessModes:                   c.options.PVC.Spec.AccessModes,
			Capacity: v1.ResourceList{
				v1.ResourceName(v1.ResourceStorage): resource.MustParse(fmt.Sprintf("%dGi", sizeGB)),
			},
			VolumeMode: volumeMode,
			PersistentVolumeSource: v1.PersistentVolumeSource{
				AWSElasticBlockStore: &v1.AWSElasticBlockStoreVolumeSource{
					VolumeID:  string(volumeID),
					FSType:    fstype,
					Partition: 0,
					ReadOnly:  false,
				},
			},
			MountOptions: c.options.MountOptions,
		},
	}

	if len(c.options.PVC.Spec.AccessModes) == 0 {
		pv.Spec.AccessModes = c.plugin.GetAccessModes()
	}

	requirements := make([]v1.NodeSelectorRequirement, 0)
	if len(labels) != 0 {
		if pv.Labels == nil {
			pv.Labels = make(map[string]string)
		}
		for k, v := range labels {
			pv.Labels[k] = v
			requirements = append(requirements, v1.NodeSelectorRequirement{Key: k, Operator: v1.NodeSelectorOpIn, Values: []string{v}})
		}
	}

	if utilfeature.DefaultFeatureGate.Enabled(features.VolumeScheduling) {
		pv.Spec.NodeAffinity = new(v1.VolumeNodeAffinity)
		pv.Spec.NodeAffinity.Required = new(v1.NodeSelector)
		pv.Spec.NodeAffinity.Required.NodeSelectorTerms = make([]v1.NodeSelectorTerm, 1)
		pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions = requirements
	}

	return pv, nil
}
