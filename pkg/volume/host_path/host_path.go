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

package host_path

import (
	"fmt"
	"os"
	"regexp"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/kubernetes/pkg/util/mount"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/recyclerclient"
	"k8s.io/kubernetes/pkg/volume/validation"
)

// This is the primary entrypoint for volume plugins.
// The volumeConfig arg provides the ability to configure volume behavior.  It is implemented as a pointer to allow nils.
// The hostPathPlugin is used to store the volumeConfig and give it, when needed, to the func that Recycles.
// Tests that exercise recycling should not use this func but instead use ProbeRecyclablePlugins() to override default behavior.
func ProbeVolumePlugins(volumeConfig volume.VolumeConfig) []volume.VolumePlugin {
	return []volume.VolumePlugin{
		&hostPathPlugin{
			host:   nil,
			config: volumeConfig,
		},
	}
}

type hostPathPlugin struct {
	host   volume.VolumeHost
	config volume.VolumeConfig
}

var _ volume.VolumePlugin = &hostPathPlugin{}
var _ volume.PersistentVolumePlugin = &hostPathPlugin{}
var _ volume.RecyclableVolumePlugin = &hostPathPlugin{}
var _ volume.DeletableVolumePlugin = &hostPathPlugin{}
var _ volume.ProvisionableVolumePlugin = &hostPathPlugin{}

const (
	hostPathPluginName = "kubernetes.io/host-path"
)

func (plugin *hostPathPlugin) Init(host volume.VolumeHost) error {
	plugin.host = host
	return nil
}

func (plugin *hostPathPlugin) GetPluginName() string {
	return hostPathPluginName
}

func (plugin *hostPathPlugin) GetVolumeName(spec *volume.Spec) (string, error) {
	volumeSource, _, err := getVolumeSource(spec)
	if err != nil {
		return "", err
	}

	return volumeSource.Path, nil
}

func (plugin *hostPathPlugin) CanSupport(spec *volume.Spec) bool {
	return (spec.PersistentVolume != nil && spec.PersistentVolume.Spec.HostPath != nil) ||
		(spec.Volume != nil && spec.Volume.HostPath != nil)
}

func (plugin *hostPathPlugin) RequiresRemount() bool {
	return false
}

func (plugin *hostPathPlugin) SupportsMountOption() bool {
	return false
}

func (plugin *hostPathPlugin) SupportsBulkVolumeVerification() bool {
	return false
}

func (plugin *hostPathPlugin) GetAccessModes() []v1.PersistentVolumeAccessMode {
	return []v1.PersistentVolumeAccessMode{
		v1.ReadWriteOnce,
	}
}

func (plugin *hostPathPlugin) NewMounter(spec *volume.Spec, pod *v1.Pod, opts volume.VolumeOptions) (volume.Mounter, error) {
	hostPathVolumeSource, readOnly, err := getVolumeSource(spec)
	if err != nil {
		return nil, err
	}

	path := hostPathVolumeSource.Path
	pathType := new(v1.HostPathType)
	if hostPathVolumeSource.Type == nil {
		*pathType = v1.HostPathUnset
	} else {
		pathType = hostPathVolumeSource.Type
	}
	return &hostPathMounter{
		hostPath: &hostPath{path: path, pathType: pathType},
		readOnly: readOnly,
		mounter:  plugin.host.GetMounter(plugin.GetPluginName()),
	}, nil
}

func (plugin *hostPathPlugin) NewUnmounter(volName string, podUID types.UID) (volume.Unmounter, error) {
	return &hostPathUnmounter{&hostPath{
		path: "",
	}}, nil
}

// Recycle recycles/scrubs clean a HostPath volume.
// Recycle blocks until the pod has completed or any error occurs.
// HostPath recycling only works in single node clusters and is meant for testing purposes only.
func (plugin *hostPathPlugin) Recycle(pvName string, spec *volume.Spec, eventRecorder recyclerclient.RecycleEventRecorder) error {
	if spec.PersistentVolume == nil || spec.PersistentVolume.Spec.HostPath == nil {
		return fmt.Errorf("spec.PersistentVolume.Spec.HostPath is nil")
	}

	pod := plugin.config.RecyclerPodTemplate
	timeout := util.CalculateTimeoutForVolume(plugin.config.RecyclerMinimumTimeout, plugin.config.RecyclerTimeoutIncrement, spec.PersistentVolume)
	// overrides
	pod.Spec.ActiveDeadlineSeconds = &timeout
	pod.Spec.Volumes[0].VolumeSource = v1.VolumeSource{
		HostPath: &v1.HostPathVolumeSource{
			Path: spec.PersistentVolume.Spec.HostPath.Path,
		},
	}
	return recyclerclient.RecycleVolumeByWatchingPodUntilCompletion(pvName, pod, plugin.host.GetKubeClient(), eventRecorder)
}

func (plugin *hostPathPlugin) NewDeleter(spec *volume.Spec) (volume.Deleter, error) {
	return newDeleter(spec, plugin.host)
}

func (plugin *hostPathPlugin) NewProvisioner(options volume.VolumeOptions) (volume.Provisioner, error) {
	if !plugin.config.ProvisioningEnabled {
		return nil, fmt.Errorf("provisioning in volume plugin %q is disabled", plugin.GetPluginName())
	}
	return newProvisioner(options, plugin.host, plugin)
}

func (plugin *hostPathPlugin) ConstructVolumeSpec(volumeName, mountPath string) (*volume.Spec, error) {
	hostPathVolume := &v1.Volume{
		Name: volumeName,
		VolumeSource: v1.VolumeSource{
			HostPath: &v1.HostPathVolumeSource{
				Path: volumeName,
			},
		},
	}
	return volume.NewSpecFromVolume(hostPathVolume), nil
}

func newDeleter(spec *volume.Spec, host volume.VolumeHost) (volume.Deleter, error) {
	if spec.PersistentVolume != nil && spec.PersistentVolume.Spec.HostPath == nil {
		return nil, fmt.Errorf("spec.PersistentVolumeSource.HostPath is nil")
	}
	path := spec.PersistentVolume.Spec.HostPath.Path
	return &hostPathDeleter{name: spec.Name(), path: path, host: host}, nil
}

func newProvisioner(options volume.VolumeOptions, host volume.VolumeHost, plugin *hostPathPlugin) (volume.Provisioner, error) {
	return &hostPathProvisioner{options: options, host: host, plugin: plugin}, nil
}

// HostPath volumes represent a bare host file or directory mount.
// The direct at the specified path will be directly exposed to the container.
type hostPath struct {
	path     string
	pathType *v1.HostPathType
	volume.MetricsNil
}

func (hp *hostPath) GetPath() string {
	return hp.path
}

type hostPathMounter struct {
	*hostPath
	readOnly bool
	mounter  mount.Interface
}

var _ volume.Mounter = &hostPathMounter{}

func (b *hostPathMounter) GetAttributes() volume.Attributes {
	return volume.Attributes{
		ReadOnly:        b.readOnly,
		Managed:         false,
		SupportsSELinux: false,
	}
}

// Checks prior to mount operations to verify that the required components (binaries, etc.)
// to mount the volume are available on the underlying node.
// If not, it returns an error
func (b *hostPathMounter) CanMount() error {
	return nil
}

// SetUp does nothing.
func (b *hostPathMounter) SetUp(fsGroup *int64) error {
	err := validation.ValidatePathNoBacksteps(b.GetPath())
	if err != nil {
		return fmt.Errorf("invalid HostPath `%s`: %v", b.GetPath(), err)
	}

	if *b.pathType == v1.HostPathUnset {
		return nil
	}
	return checkType(b.GetPath(), b.pathType, b.mounter)
}

// SetUpAt does not make sense for host paths - probably programmer error.
func (b *hostPathMounter) SetUpAt(dir string, fsGroup *int64) error {
	return fmt.Errorf("not available for host paths")
}

func (b *hostPathMounter) GetPath() string {
	return b.path
}

type hostPathUnmounter struct {
	*hostPath
}

var _ volume.Unmounter = &hostPathUnmounter{}

// TearDown does nothing.
func (c *hostPathUnmounter) TearDown() error {
	return nil
}

// TearDownAt does not make sense for host paths - probably programmer error.
func (c *hostPathUnmounter) TearDownAt(dir string) error {
	return fmt.Errorf("not available for host paths")
}

// hostPathProvisioner implements a Provisioner for the HostPath plugin
// This implementation is meant for testing only and only works in a single node cluster.
type hostPathProvisioner struct {
	host    volume.VolumeHost
	options volume.VolumeOptions
	plugin  *hostPathPlugin
}

// Create for hostPath simply creates a local /tmp/hostpath_pv/%s directory as a new PersistentVolume.
// This Provisioner is meant for development and testing only and WILL NOT WORK in a multi-node cluster.
func (r *hostPathProvisioner) Provision(selectedNode *v1.Node, allowedTopologies []v1.TopologySelectorTerm) (*v1.PersistentVolume, error) {
	if util.CheckPersistentVolumeClaimModeBlock(r.options.PVC) {
		return nil, fmt.Errorf("%s does not support block volume provisioning", r.plugin.GetPluginName())
	}

	fullpath := fmt.Sprintf("/tmp/hostpath_pv/%s", uuid.NewUUID())

	capacity := r.options.PVC.Spec.Resources.Requests[v1.ResourceName(v1.ResourceStorage)]
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: r.options.PVName,
			Annotations: map[string]string{
				util.VolumeDynamicallyCreatedByKey: "hostpath-dynamic-provisioner",
			},
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: r.options.PersistentVolumeReclaimPolicy,
			AccessModes:                   r.options.PVC.Spec.AccessModes,
			Capacity: v1.ResourceList{
				v1.ResourceName(v1.ResourceStorage): capacity,
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: fullpath,
				},
			},
		},
	}
	if len(r.options.PVC.Spec.AccessModes) == 0 {
		pv.Spec.AccessModes = r.plugin.GetAccessModes()
	}

	return pv, os.MkdirAll(pv.Spec.HostPath.Path, 0750)
}

// hostPathDeleter deletes a hostPath PV from the cluster.
// This deleter only works on a single host cluster and is for testing purposes only.
type hostPathDeleter struct {
	name string
	path string
	host volume.VolumeHost
	volume.MetricsNil
}

func (r *hostPathDeleter) GetPath() string {
	return r.path
}

// Delete for hostPath removes the local directory so long as it is beneath /tmp/*.
// THIS IS FOR TESTING AND LOCAL DEVELOPMENT ONLY!  This message should scare you away from using
// this deleter for anything other than development and testing.
func (r *hostPathDeleter) Delete() error {
	regexp := regexp.MustCompile("/tmp/.+")
	if !regexp.MatchString(r.GetPath()) {
		return fmt.Errorf("host_path deleter only supports /tmp/.+ but received provided %s", r.GetPath())
	}
	return os.RemoveAll(r.GetPath())
}

func getVolumeSource(spec *volume.Spec) (*v1.HostPathVolumeSource, bool, error) {
	if spec.Volume != nil && spec.Volume.HostPath != nil {
		return spec.Volume.HostPath, spec.ReadOnly, nil
	} else if spec.PersistentVolume != nil &&
		spec.PersistentVolume.Spec.HostPath != nil {
		return spec.PersistentVolume.Spec.HostPath, spec.ReadOnly, nil
	}

	return nil, false, fmt.Errorf("spec does not reference an HostPath volume type")
}

type hostPathTypeChecker interface {
	Exists() bool
	IsFile() bool
	MakeFile() error
	IsDir() bool
	MakeDir() error
	IsBlock() bool
	IsChar() bool
	IsSocket() bool
	GetPath() string
}

type fileTypeChecker struct {
	path    string
	exists  bool
	mounter mount.Interface
}

func (ftc *fileTypeChecker) Exists() bool {
	exists, err := ftc.mounter.ExistsPath(ftc.path)
	return exists && err == nil
}

func (ftc *fileTypeChecker) IsFile() bool {
	if !ftc.Exists() {
		return false
	}
	return !ftc.IsDir()
}

func (ftc *fileTypeChecker) MakeFile() error {
	return ftc.mounter.MakeFile(ftc.path)
}

func (ftc *fileTypeChecker) IsDir() bool {
	if !ftc.Exists() {
		return false
	}
	pathType, err := ftc.mounter.GetFileType(ftc.path)
	if err != nil {
		return false
	}
	return string(pathType) == string(v1.HostPathDirectory)
}

func (ftc *fileTypeChecker) MakeDir() error {
	return ftc.mounter.MakeDir(ftc.path)
}

func (ftc *fileTypeChecker) IsBlock() bool {
	blkDevType, err := ftc.mounter.GetFileType(ftc.path)
	if err != nil {
		return false
	}
	return string(blkDevType) == string(v1.HostPathBlockDev)
}

func (ftc *fileTypeChecker) IsChar() bool {
	charDevType, err := ftc.mounter.GetFileType(ftc.path)
	if err != nil {
		return false
	}
	return string(charDevType) == string(v1.HostPathCharDev)
}

func (ftc *fileTypeChecker) IsSocket() bool {
	socketType, err := ftc.mounter.GetFileType(ftc.path)
	if err != nil {
		return false
	}
	return string(socketType) == string(v1.HostPathSocket)
}

func (ftc *fileTypeChecker) GetPath() string {
	return ftc.path
}

func newFileTypeChecker(path string, mounter mount.Interface) hostPathTypeChecker {
	return &fileTypeChecker{path: path, mounter: mounter}
}

// checkType checks whether the given path is the exact pathType
func checkType(path string, pathType *v1.HostPathType, mounter mount.Interface) error {
	return checkTypeInternal(newFileTypeChecker(path, mounter), pathType)
}

func checkTypeInternal(ftc hostPathTypeChecker, pathType *v1.HostPathType) error {
	switch *pathType {
	case v1.HostPathDirectoryOrCreate:
		if !ftc.Exists() {
			return ftc.MakeDir()
		}
		fallthrough
	case v1.HostPathDirectory:
		if !ftc.IsDir() {
			return fmt.Errorf("hostPath type check failed: %s is not a directory", ftc.GetPath())
		}
	case v1.HostPathFileOrCreate:
		if !ftc.Exists() {
			return ftc.MakeFile()
		}
		fallthrough
	case v1.HostPathFile:
		if !ftc.IsFile() {
			return fmt.Errorf("hostPath type check failed: %s is not a file", ftc.GetPath())
		}
	case v1.HostPathSocket:
		if !ftc.IsSocket() {
			return fmt.Errorf("hostPath type check failed: %s is not a socket file", ftc.GetPath())
		}
	case v1.HostPathCharDev:
		if !ftc.IsChar() {
			return fmt.Errorf("hostPath type check failed: %s is not a character device", ftc.GetPath())
		}
	case v1.HostPathBlockDev:
		if !ftc.IsBlock() {
			return fmt.Errorf("hostPath type check failed: %s is not a block device", ftc.GetPath())
		}
	default:
		return fmt.Errorf("%s is an invalid volume type", *pathType)
	}

	return nil
}
