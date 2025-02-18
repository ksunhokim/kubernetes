/*
Copyright 2015 The Kubernetes Authors.

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

package util

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	"k8s.io/kubernetes/pkg/features"
	kubeletapis "k8s.io/kubernetes/pkg/kubelet/apis"
	"k8s.io/kubernetes/pkg/util/mount"
	utilstrings "k8s.io/kubernetes/pkg/util/strings"
	"k8s.io/kubernetes/pkg/volume"

	"reflect"

	"hash/fnv"
	"math/rand"
	"strconv"

	"k8s.io/apimachinery/pkg/api/resource"
	utypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/volume/util/types"
	"k8s.io/kubernetes/pkg/volume/util/volumepathhandler"
)

const (
	// GB - GigaByte size
	GB = 1000 * 1000 * 1000
	// GIB - GibiByte size
	GIB = 1024 * 1024 * 1024

	readyFileName = "ready"

	// ControllerManagedAttachAnnotation is the key of the annotation on Node
	// objects that indicates attach/detach operations for the node should be
	// managed by the attach/detach controller
	ControllerManagedAttachAnnotation string = "volumes.kubernetes.io/controller-managed-attach-detach"

	// KeepTerminatedPodVolumesAnnotation is the key of the annotation on Node
	// that decides if pod volumes are unmounted when pod is terminated
	KeepTerminatedPodVolumesAnnotation string = "volumes.kubernetes.io/keep-terminated-pod-volumes"

	// VolumeGidAnnotationKey is the of the annotation on the PersistentVolume
	// object that specifies a supplemental GID.
	VolumeGidAnnotationKey = "pv.beta.kubernetes.io/gid"

	// VolumeDynamicallyCreatedByKey is the key of the annotation on PersistentVolume
	// object created dynamically
	VolumeDynamicallyCreatedByKey = "kubernetes.io/createdby"
)

// VolumeZoneConfig contains config information about zonal volume.
type VolumeZoneConfig struct {
	ZonePresent                bool
	ZonesPresent               bool
	ReplicaZoneFromNodePresent bool
	Zone                       string
	Zones                      string
	ReplicaZoneFromNode        string
}

// IsReady checks for the existence of a regular file
// called 'ready' in the given directory and returns
// true if that file exists.
func IsReady(dir string) bool {
	readyFile := path.Join(dir, readyFileName)
	s, err := os.Stat(readyFile)
	if err != nil {
		return false
	}

	if !s.Mode().IsRegular() {
		glog.Errorf("ready-file is not a file: %s", readyFile)
		return false
	}

	return true
}

// SetReady creates a file called 'ready' in the given
// directory.  It logs an error if the file cannot be
// created.
func SetReady(dir string) {
	if err := os.MkdirAll(dir, 0750); err != nil && !os.IsExist(err) {
		glog.Errorf("Can't mkdir %s: %v", dir, err)
		return
	}

	readyFile := path.Join(dir, readyFileName)
	file, err := os.Create(readyFile)
	if err != nil {
		glog.Errorf("Can't touch %s: %v", readyFile, err)
		return
	}
	file.Close()
}

// UnmountPath is a common unmount routine that unmounts the given path and
// deletes the remaining directory if successful.
func UnmountPath(mountPath string, mounter mount.Interface) error {
	return UnmountMountPoint(mountPath, mounter, false /* extensiveMountPointCheck */)
}

// UnmountMountPoint is a common unmount routine that unmounts the given path and
// deletes the remaining directory if successful.
// if extensiveMountPointCheck is true
// IsNotMountPoint will be called instead of IsLikelyNotMountPoint.
// IsNotMountPoint is more expensive but properly handles bind mounts.
func UnmountMountPoint(mountPath string, mounter mount.Interface, extensiveMountPointCheck bool) error {
	pathExists, pathErr := PathExists(mountPath)
	if !pathExists {
		glog.Warningf("Warning: Unmount skipped because path does not exist: %v", mountPath)
		return nil
	}
	corruptedMnt := IsCorruptedMnt(pathErr)
	if pathErr != nil && !corruptedMnt {
		return fmt.Errorf("error checking path: %v", pathErr)
	}
	return doUnmountMountPoint(mountPath, mounter, extensiveMountPointCheck, corruptedMnt)
}

// doUnmountMountPoint is a common unmount routine that unmounts the given path and
// deletes the remaining directory if successful.
// if extensiveMountPointCheck is true
// IsNotMountPoint will be called instead of IsLikelyNotMountPoint.
// IsNotMountPoint is more expensive but properly handles bind mounts.
// if corruptedMnt is true, it means that the mountPath is a corrupted mountpoint, Take it as an argument for convenience of testing
func doUnmountMountPoint(mountPath string, mounter mount.Interface, extensiveMountPointCheck bool, corruptedMnt bool) error {
	if !corruptedMnt {
		var notMnt bool
		var err error
		if extensiveMountPointCheck {
			notMnt, err = mount.IsNotMountPoint(mounter, mountPath)
		} else {
			notMnt, err = mounter.IsLikelyNotMountPoint(mountPath)
		}

		if err != nil {
			return err
		}

		if notMnt {
			glog.Warningf("Warning: %q is not a mountpoint, deleting", mountPath)
			return os.Remove(mountPath)
		}
	}

	// Unmount the mount path
	glog.V(4).Infof("%q is a mountpoint, unmounting", mountPath)
	if err := mounter.Unmount(mountPath); err != nil {
		return err
	}
	notMnt, mntErr := mounter.IsLikelyNotMountPoint(mountPath)
	if mntErr != nil {
		return mntErr
	}
	if notMnt {
		glog.V(4).Infof("%q is unmounted, deleting the directory", mountPath)
		return os.Remove(mountPath)
	}
	return fmt.Errorf("failed to unmount path %v", mountPath)
}

// PathExists returns true if the specified path exists.
func PathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	} else if os.IsNotExist(err) {
		return false, nil
	} else if IsCorruptedMnt(err) {
		return true, err
	} else {
		return false, err
	}
}

// IsCorruptedMnt return true if err is about corrupted mount point
func IsCorruptedMnt(err error) bool {
	if err == nil {
		return false
	}
	var underlyingError error
	switch pe := err.(type) {
	case nil:
		return false
	case *os.PathError:
		underlyingError = pe.Err
	case *os.LinkError:
		underlyingError = pe.Err
	case *os.SyscallError:
		underlyingError = pe.Err
	}

	return underlyingError == syscall.ENOTCONN || underlyingError == syscall.ESTALE || underlyingError == syscall.EIO
}

// GetSecretForPod locates secret by name in the pod's namespace and returns secret map
func GetSecretForPod(pod *v1.Pod, secretName string, kubeClient clientset.Interface) (map[string]string, error) {
	secret := make(map[string]string)
	if kubeClient == nil {
		return secret, fmt.Errorf("cannot get kube client")
	}
	secrets, err := kubeClient.CoreV1().Secrets(pod.Namespace).Get(secretName, metav1.GetOptions{})
	if err != nil {
		return secret, err
	}
	for name, data := range secrets.Data {
		secret[name] = string(data)
	}
	return secret, nil
}

// GetSecretForPV locates secret by name and namespace, verifies the secret type, and returns secret map
func GetSecretForPV(secretNamespace, secretName, volumePluginName string, kubeClient clientset.Interface) (map[string]string, error) {
	secret := make(map[string]string)
	if kubeClient == nil {
		return secret, fmt.Errorf("cannot get kube client")
	}
	secrets, err := kubeClient.CoreV1().Secrets(secretNamespace).Get(secretName, metav1.GetOptions{})
	if err != nil {
		return secret, err
	}
	if secrets.Type != v1.SecretType(volumePluginName) {
		return secret, fmt.Errorf("cannot get secret of type %s", volumePluginName)
	}
	for name, data := range secrets.Data {
		secret[name] = string(data)
	}
	return secret, nil
}

// GetClassForVolume locates storage class by persistent volume
func GetClassForVolume(kubeClient clientset.Interface, pv *v1.PersistentVolume) (*storage.StorageClass, error) {
	if kubeClient == nil {
		return nil, fmt.Errorf("cannot get kube client")
	}
	className := v1helper.GetPersistentVolumeClass(pv)
	if className == "" {
		return nil, fmt.Errorf("volume has no storage class")
	}

	class, err := kubeClient.StorageV1().StorageClasses().Get(className, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return class, nil
}

// CheckNodeAffinity looks at the PV node affinity, and checks if the node has the same corresponding labels
// This ensures that we don't mount a volume that doesn't belong to this node
func CheckNodeAffinity(pv *v1.PersistentVolume, nodeLabels map[string]string) error {
	return checkVolumeNodeAffinity(pv, nodeLabels)
}

func checkVolumeNodeAffinity(pv *v1.PersistentVolume, nodeLabels map[string]string) error {
	if pv.Spec.NodeAffinity == nil {
		return nil
	}

	if pv.Spec.NodeAffinity.Required != nil {
		terms := pv.Spec.NodeAffinity.Required.NodeSelectorTerms
		glog.V(10).Infof("Match for Required node selector terms %+v", terms)
		if !v1helper.MatchNodeSelectorTerms(terms, labels.Set(nodeLabels), nil) {
			return fmt.Errorf("no matching NodeSelectorTerms")
		}
	}

	return nil
}

// LoadPodFromFile will read, decode, and return a Pod from a file.
func LoadPodFromFile(filePath string) (*v1.Pod, error) {
	if filePath == "" {
		return nil, fmt.Errorf("file path not specified")
	}
	podDef, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file path %s: %+v", filePath, err)
	}
	if len(podDef) == 0 {
		return nil, fmt.Errorf("file was empty: %s", filePath)
	}
	pod := &v1.Pod{}

	codec := legacyscheme.Codecs.UniversalDecoder()
	if err := runtime.DecodeInto(codec, podDef, pod); err != nil {
		return nil, fmt.Errorf("failed decoding file: %v", err)
	}
	return pod, nil
}

// SelectZoneForVolume is a wrapper around SelectZonesForVolume
// to select a single zone for a volume based on parameters
func SelectZoneForVolume(zoneParameterPresent, zonesParameterPresent bool, zoneParameter string, zonesParameter, zonesWithNodes sets.String, node *v1.Node, allowedTopologies []v1.TopologySelectorTerm, pvcName string) (string, error) {
	zones, err := SelectZonesForVolume(zoneParameterPresent, zonesParameterPresent, zoneParameter, zonesParameter, zonesWithNodes, node, allowedTopologies, pvcName, 1)
	if err != nil {
		return "", err
	}
	zone, ok := zones.PopAny()
	if !ok {
		return "", fmt.Errorf("could not determine a zone to provision volume in")
	}
	return zone, nil
}

// SelectZonesForVolume selects zones for a volume based on several factors:
// node.zone, allowedTopologies, zone/zones parameters from storageclass,
// zones with active nodes from the cluster. The number of zones = replicas.
func SelectZonesForVolume(zoneParameterPresent, zonesParameterPresent bool, zoneParameter string, zonesParameter, zonesWithNodes sets.String, node *v1.Node, allowedTopologies []v1.TopologySelectorTerm, pvcName string, numReplicas uint32) (sets.String, error) {
	if zoneParameterPresent && zonesParameterPresent {
		return nil, fmt.Errorf("both zone and zones StorageClass parameters must not be used at the same time")
	}

	var zoneFromNode string
	// pick one zone from node if present
	if node != nil {
		// VolumeScheduling implicit since node is not nil
		if zoneParameterPresent || zonesParameterPresent {
			return nil, fmt.Errorf("zone[s] cannot be specified in StorageClass if VolumeBindingMode is set to WaitForFirstConsumer. Please specify allowedTopologies in StorageClass for constraining zones")
		}

		// pick node's zone for one of the replicas
		var ok bool
		zoneFromNode, ok = node.ObjectMeta.Labels[kubeletapis.LabelZoneFailureDomain]
		if !ok {
			return nil, fmt.Errorf("%s Label for node missing", kubeletapis.LabelZoneFailureDomain)
		}
		// if single replica volume and node with zone found, return immediately
		if numReplicas == 1 {
			return sets.NewString(zoneFromNode), nil
		}
	}

	// pick zone from allowedZones if specified
	allowedZones, err := ZonesFromAllowedTopologies(allowedTopologies)
	if err != nil {
		return nil, err
	}

	if (len(allowedTopologies) > 0) && (allowedZones.Len() == 0) {
		return nil, fmt.Errorf("no matchLabelExpressions with %s key found in allowedTopologies. Please specify matchLabelExpressions with %s key", kubeletapis.LabelZoneFailureDomain, kubeletapis.LabelZoneFailureDomain)
	}

	if allowedZones.Len() > 0 {
		// VolumeScheduling implicit since allowedZones present
		if zoneParameterPresent || zonesParameterPresent {
			return nil, fmt.Errorf("zone[s] cannot be specified in StorageClass if allowedTopologies specified")
		}
		// scheduler will guarantee if node != null above, zoneFromNode is member of allowedZones.
		// so if zoneFromNode != "", we can safely assume it is part of allowedZones.
		zones, err := chooseZonesForVolumeIncludingZone(allowedZones, pvcName, zoneFromNode, numReplicas)
		if err != nil {
			return nil, fmt.Errorf("cannot process zones in allowedTopologies: %v", err)
		}
		return zones, nil
	}

	// pick zone from parameters if present
	if zoneParameterPresent {
		if numReplicas > 1 {
			return nil, fmt.Errorf("zone cannot be specified if desired number of replicas for pv is greather than 1. Please specify zones or allowedTopologies to specify desired zones")
		}
		return sets.NewString(zoneParameter), nil
	}

	if zonesParameterPresent {
		if uint32(zonesParameter.Len()) < numReplicas {
			return nil, fmt.Errorf("not enough zones found in zones parameter to provision a volume with %d replicas. Found %d zones, need %d zones", numReplicas, zonesParameter.Len(), numReplicas)
		}
		// directly choose from zones parameter; no zone from node need to be considered
		return ChooseZonesForVolume(zonesParameter, pvcName, numReplicas), nil
	}

	// pick zone from zones with nodes
	if zonesWithNodes.Len() > 0 {
		// If node != null (and thus zoneFromNode != ""), zoneFromNode will be member of zonesWithNodes
		zones, err := chooseZonesForVolumeIncludingZone(zonesWithNodes, pvcName, zoneFromNode, numReplicas)
		if err != nil {
			return nil, fmt.Errorf("cannot process zones where nodes exist in the cluster: %v", err)
		}
		return zones, nil
	}
	return nil, fmt.Errorf("cannot determine zones to provision volume in")
}

// ZonesFromAllowedTopologies returns a list of zones specified in allowedTopologies
func ZonesFromAllowedTopologies(allowedTopologies []v1.TopologySelectorTerm) (sets.String, error) {
	zones := make(sets.String)
	for _, term := range allowedTopologies {
		for _, exp := range term.MatchLabelExpressions {
			if exp.Key == kubeletapis.LabelZoneFailureDomain {
				for _, value := range exp.Values {
					zones.Insert(value)
				}
			} else {
				return nil, fmt.Errorf("unsupported key found in matchLabelExpressions: %s", exp.Key)
			}
		}
	}
	return zones, nil
}

// ZonesSetToLabelValue converts zones set to label value
func ZonesSetToLabelValue(strSet sets.String) string {
	return strings.Join(strSet.UnsortedList(), kubeletapis.LabelMultiZoneDelimiter)
}

// ZonesToSet converts a string containing a comma separated list of zones to set
func ZonesToSet(zonesString string) (sets.String, error) {
	zones, err := stringToSet(zonesString, ",")
	if err != nil {
		return nil, fmt.Errorf("error parsing zones %s, must be strings separated by commas: %v", zonesString, err)
	}
	return zones, nil
}

// LabelZonesToSet converts a PV label value from string containing a delimited list of zones to set
func LabelZonesToSet(labelZonesValue string) (sets.String, error) {
	return stringToSet(labelZonesValue, kubeletapis.LabelMultiZoneDelimiter)
}

// StringToSet converts a string containing list separated by specified delimiter to a set
func stringToSet(str, delimiter string) (sets.String, error) {
	zonesSlice := strings.Split(str, delimiter)
	zonesSet := make(sets.String)
	for _, zone := range zonesSlice {
		trimmedZone := strings.TrimSpace(zone)
		if trimmedZone == "" {
			return make(sets.String), fmt.Errorf(
				"%q separated list (%q) must not contain an empty string",
				delimiter,
				str)
		}
		zonesSet.Insert(trimmedZone)
	}
	return zonesSet, nil
}

// LabelZonesToList converts a PV label value from string containing a delimited list of zones to list
func LabelZonesToList(labelZonesValue string) ([]string, error) {
	return stringToList(labelZonesValue, kubeletapis.LabelMultiZoneDelimiter)
}

// StringToList converts a string containing list separated by specified delimiter to a list
func stringToList(str, delimiter string) ([]string, error) {
	zonesSlice := make([]string, 0)
	for _, zone := range strings.Split(str, delimiter) {
		trimmedZone := strings.TrimSpace(zone)
		if trimmedZone == "" {
			return nil, fmt.Errorf(
				"%q separated list (%q) must not contain an empty string",
				delimiter,
				str)
		}
		zonesSlice = append(zonesSlice, trimmedZone)
	}
	return zonesSlice, nil
}

// CalculateTimeoutForVolume calculates time for a Recycler pod to complete a
// recycle operation. The calculation and return value is either the
// minimumTimeout or the timeoutIncrement per Gi of storage size, whichever is
// greater.
func CalculateTimeoutForVolume(minimumTimeout, timeoutIncrement int, pv *v1.PersistentVolume) int64 {
	giQty := resource.MustParse("1Gi")
	pvQty := pv.Spec.Capacity[v1.ResourceStorage]
	giSize := giQty.Value()
	pvSize := pvQty.Value()
	timeout := (pvSize / giSize) * int64(timeoutIncrement)
	if timeout < int64(minimumTimeout) {
		return int64(minimumTimeout)
	}
	return timeout
}

// RoundUpSize calculates how many allocation units are needed to accommodate
// a volume of given size. E.g. when user wants 1500MiB volume, while AWS EBS
// allocates volumes in gibibyte-sized chunks,
// RoundUpSize(1500 * 1024*1024, 1024*1024*1024) returns '2'
// (2 GiB is the smallest allocatable volume that can hold 1500MiB)
func RoundUpSize(volumeSizeBytes int64, allocationUnitBytes int64) int64 {
	roundedUp := volumeSizeBytes / allocationUnitBytes
	if volumeSizeBytes%allocationUnitBytes > 0 {
		roundedUp++
	}
	return roundedUp
}

// RoundUpToGB rounds up given quantity to chunks of GB
func RoundUpToGB(size resource.Quantity) int64 {
	requestBytes := size.Value()
	return RoundUpSize(requestBytes, GB)
}

// RoundUpToGiB rounds up given quantity upto chunks of GiB
func RoundUpToGiB(size resource.Quantity) int64 {
	requestBytes := size.Value()
	return RoundUpSize(requestBytes, GIB)
}

// RoundUpSizeInt calculates how many allocation units are needed to accommodate
// a volume of given size. It returns an int instead of an int64 and an error if
// there's overflow
func RoundUpSizeInt(volumeSizeBytes int64, allocationUnitBytes int64) (int, error) {
	roundedUp := RoundUpSize(volumeSizeBytes, allocationUnitBytes)
	roundedUpInt := int(roundedUp)
	if int64(roundedUpInt) != roundedUp {
		return 0, fmt.Errorf("capacity %v is too great, casting results in integer overflow", roundedUp)
	}
	return roundedUpInt, nil
}

// RoundUpToGBInt rounds up given quantity to chunks of GB. It returns an
// int instead of an int64 and an error if there's overflow
func RoundUpToGBInt(size resource.Quantity) (int, error) {
	requestBytes := size.Value()
	return RoundUpSizeInt(requestBytes, GB)
}

// RoundUpToGiBInt rounds up given quantity upto chunks of GiB. It returns an
// int instead of an int64 and an error if there's overflow
func RoundUpToGiBInt(size resource.Quantity) (int, error) {
	requestBytes := size.Value()
	return RoundUpSizeInt(requestBytes, GIB)
}

// GenerateVolumeName returns a PV name with clusterName prefix. The function
// should be used to generate a name of GCE PD or Cinder volume. It basically
// adds "<clusterName>-dynamic-" before the PV name, making sure the resulting
// string fits given length and cuts "dynamic" if not.
func GenerateVolumeName(clusterName, pvName string, maxLength int) string {
	prefix := clusterName + "-dynamic"
	pvLen := len(pvName)

	// cut the "<clusterName>-dynamic" to fit full pvName into maxLength
	// +1 for the '-' dash
	if pvLen+1+len(prefix) > maxLength {
		prefix = prefix[:maxLength-pvLen-1]
	}
	return prefix + "-" + pvName
}

// GetPath checks if the path from the mounter is empty.
func GetPath(mounter volume.Mounter) (string, error) {
	path := mounter.GetPath()
	if path == "" {
		return "", fmt.Errorf("path is empty %s", reflect.TypeOf(mounter).String())
	}
	return path, nil
}

// ChooseZoneForVolume  implements our heuristics for choosing a zone for volume creation based on the volume name
// Volumes are generally round-robin-ed across all active zones, using the hash of the PVC Name.
// However, if the PVCName ends with `-<integer>`, we will hash the prefix, and then add the integer to the hash.
// This means that a StatefulSet's volumes (`claimname-statefulsetname-id`) will spread across available zones,
// assuming the id values are consecutive.
func ChooseZoneForVolume(zones sets.String, pvcName string) string {
	// No zones available, return empty string.
	if zones.Len() == 0 {
		return ""
	}

	// We create the volume in a zone determined by the name
	// Eventually the scheduler will coordinate placement into an available zone
	hash, index := getPVCNameHashAndIndexOffset(pvcName)

	// Zones.List returns zones in a consistent order (sorted)
	// We do have a potential failure case where volumes will not be properly spread,
	// if the set of zones changes during StatefulSet volume creation.  However, this is
	// probably relatively unlikely because we expect the set of zones to be essentially
	// static for clusters.
	// Hopefully we can address this problem if/when we do full scheduler integration of
	// PVC placement (which could also e.g. avoid putting volumes in overloaded or
	// unhealthy zones)
	zoneSlice := zones.List()
	zone := zoneSlice[(hash+index)%uint32(len(zoneSlice))]

	glog.V(2).Infof("Creating volume for PVC %q; chose zone=%q from zones=%q", pvcName, zone, zoneSlice)
	return zone
}

// chooseZonesForVolumeIncludingZone is a wrapper around ChooseZonesForVolume that ensures zoneToInclude is chosen
// zoneToInclude can either be empty in which case it is ignored. If non-empty, zoneToInclude is expected to be member of zones.
// numReplicas is expected to be > 0 and <= zones.Len()
func chooseZonesForVolumeIncludingZone(zones sets.String, pvcName, zoneToInclude string, numReplicas uint32) (sets.String, error) {
	if numReplicas == 0 {
		return nil, fmt.Errorf("invalid number of replicas passed")
	}
	if uint32(zones.Len()) < numReplicas {
		return nil, fmt.Errorf("not enough zones found to provision a volume with %d replicas. Need at least %d distinct zones for a volume with %d replicas", numReplicas, numReplicas, numReplicas)
	}
	if zoneToInclude != "" && !zones.Has(zoneToInclude) {
		return nil, fmt.Errorf("zone to be included: %s needs to be member of set: %v", zoneToInclude, zones)
	}
	if uint32(zones.Len()) == numReplicas {
		return zones, nil
	}
	if zoneToInclude != "" {
		zones.Delete(zoneToInclude)
		numReplicas = numReplicas - 1
	}
	zonesChosen := ChooseZonesForVolume(zones, pvcName, numReplicas)
	if zoneToInclude != "" {
		zonesChosen.Insert(zoneToInclude)
	}
	return zonesChosen, nil
}

// ChooseZonesForVolume is identical to ChooseZoneForVolume, but selects a multiple zones, for multi-zone disks.
func ChooseZonesForVolume(zones sets.String, pvcName string, numZones uint32) sets.String {
	// No zones available, return empty set.
	replicaZones := sets.NewString()
	if zones.Len() == 0 {
		return replicaZones
	}

	// We create the volume in a zone determined by the name
	// Eventually the scheduler will coordinate placement into an available zone
	hash, index := getPVCNameHashAndIndexOffset(pvcName)

	// Zones.List returns zones in a consistent order (sorted)
	// We do have a potential failure case where volumes will not be properly spread,
	// if the set of zones changes during StatefulSet volume creation.  However, this is
	// probably relatively unlikely because we expect the set of zones to be essentially
	// static for clusters.
	// Hopefully we can address this problem if/when we do full scheduler integration of
	// PVC placement (which could also e.g. avoid putting volumes in overloaded or
	// unhealthy zones)
	zoneSlice := zones.List()

	startingIndex := index * numZones
	for index = startingIndex; index < startingIndex+numZones; index++ {
		zone := zoneSlice[(hash+index)%uint32(len(zoneSlice))]
		replicaZones.Insert(zone)
	}

	glog.V(2).Infof("Creating volume for replicated PVC %q; chosen zones=%q from zones=%q",
		pvcName, replicaZones.UnsortedList(), zoneSlice)
	return replicaZones
}

func getPVCNameHashAndIndexOffset(pvcName string) (hash uint32, index uint32) {
	if pvcName == "" {
		// We should always be called with a name; this shouldn't happen
		glog.Warningf("No name defined during volume create; choosing random zone")

		hash = rand.Uint32()
	} else {
		hashString := pvcName

		// Heuristic to make sure that volumes in a StatefulSet are spread across zones
		// StatefulSet PVCs are (currently) named ClaimName-StatefulSetName-Id,
		// where Id is an integer index.
		// Note though that if a StatefulSet pod has multiple claims, we need them to be
		// in the same zone, because otherwise the pod will be unable to mount both volumes,
		// and will be unschedulable.  So we hash _only_ the "StatefulSetName" portion when
		// it looks like `ClaimName-StatefulSetName-Id`.
		// We continue to round-robin volume names that look like `Name-Id` also; this is a useful
		// feature for users that are creating statefulset-like functionality without using statefulsets.
		lastDash := strings.LastIndexByte(pvcName, '-')
		if lastDash != -1 {
			statefulsetIDString := pvcName[lastDash+1:]
			statefulsetID, err := strconv.ParseUint(statefulsetIDString, 10, 32)
			if err == nil {
				// Offset by the statefulsetID, so we round-robin across zones
				index = uint32(statefulsetID)
				// We still hash the volume name, but only the prefix
				hashString = pvcName[:lastDash]

				// In the special case where it looks like `ClaimName-StatefulSetName-Id`,
				// hash only the StatefulSetName, so that different claims on the same StatefulSet
				// member end up in the same zone.
				// Note that StatefulSetName (and ClaimName) might themselves both have dashes.
				// We actually just take the portion after the final - of ClaimName-StatefulSetName.
				// For our purposes it doesn't much matter (just suboptimal spreading).
				lastDash := strings.LastIndexByte(hashString, '-')
				if lastDash != -1 {
					hashString = hashString[lastDash+1:]
				}

				glog.V(2).Infof("Detected StatefulSet-style volume name %q; index=%d", pvcName, index)
			}
		}

		// We hash the (base) volume name, so we don't bias towards the first N zones
		h := fnv.New32()
		h.Write([]byte(hashString))
		hash = h.Sum32()
	}

	return hash, index
}

// UnmountViaEmptyDir delegates the tear down operation for secret, configmap, git_repo and downwardapi
// to empty_dir
func UnmountViaEmptyDir(dir string, host volume.VolumeHost, volName string, volSpec volume.Spec, podUID utypes.UID) error {
	glog.V(3).Infof("Tearing down volume %v for pod %v at %v", volName, podUID, dir)

	// Wrap EmptyDir, let it do the teardown.
	wrapped, err := host.NewWrapperUnmounter(volName, volSpec, podUID)
	if err != nil {
		return err
	}
	return wrapped.TearDownAt(dir)
}

// MountOptionFromSpec extracts and joins mount options from volume spec with supplied options
func MountOptionFromSpec(spec *volume.Spec, options ...string) []string {
	pv := spec.PersistentVolume

	if pv != nil {
		// Use beta annotation first
		if mo, ok := pv.Annotations[v1.MountOptionAnnotation]; ok {
			moList := strings.Split(mo, ",")
			return JoinMountOptions(moList, options)
		}

		if len(pv.Spec.MountOptions) > 0 {
			return JoinMountOptions(pv.Spec.MountOptions, options)
		}
	}

	return options
}

// JoinMountOptions joins mount options eliminating duplicates
func JoinMountOptions(userOptions []string, systemOptions []string) []string {
	allMountOptions := sets.NewString()

	for _, mountOption := range userOptions {
		if len(mountOption) > 0 {
			allMountOptions.Insert(mountOption)
		}
	}

	for _, mountOption := range systemOptions {
		allMountOptions.Insert(mountOption)
	}
	return allMountOptions.List()
}

// ValidateZone returns:
// - an error in case zone is an empty string or contains only any combination of spaces and tab characters
// - nil otherwise
func ValidateZone(zone string) error {
	if strings.TrimSpace(zone) == "" {
		return fmt.Errorf("the provided %q zone is not valid, it's an empty string or contains only spaces and tab characters", zone)
	}
	return nil
}

// AccessModesContains returns whether the requested mode is contained by modes
func AccessModesContains(modes []v1.PersistentVolumeAccessMode, mode v1.PersistentVolumeAccessMode) bool {
	for _, m := range modes {
		if m == mode {
			return true
		}
	}
	return false
}

// AccessModesContainedInAll returns whether all of the requested modes are contained by modes
func AccessModesContainedInAll(indexedModes []v1.PersistentVolumeAccessMode, requestedModes []v1.PersistentVolumeAccessMode) bool {
	for _, mode := range requestedModes {
		if !AccessModesContains(indexedModes, mode) {
			return false
		}
	}
	return true
}

// GetWindowsPath get a windows path
func GetWindowsPath(path string) string {
	windowsPath := strings.Replace(path, "/", "\\", -1)
	if strings.HasPrefix(windowsPath, "\\") {
		windowsPath = "c:" + windowsPath
	}
	return windowsPath
}

// GetUniquePodName returns a unique identifier to reference a pod by
func GetUniquePodName(pod *v1.Pod) types.UniquePodName {
	return types.UniquePodName(pod.UID)
}

// GetUniqueVolumeName returns a unique name representing the volume/plugin.
// Caller should ensure that volumeName is a name/ID uniquely identifying the
// actual backing device, directory, path, etc. for a particular volume.
// The returned name can be used to uniquely reference the volume, for example,
// to prevent operations (attach/detach or mount/unmount) from being triggered
// on the same volume.
func GetUniqueVolumeName(pluginName, volumeName string) v1.UniqueVolumeName {
	return v1.UniqueVolumeName(fmt.Sprintf("%s/%s", pluginName, volumeName))
}

// GetUniqueVolumeNameForNonAttachableVolume returns the unique volume name
// for a non-attachable volume.
func GetUniqueVolumeNameForNonAttachableVolume(
	podName types.UniquePodName, volumePlugin volume.VolumePlugin, volumeSpec *volume.Spec) v1.UniqueVolumeName {
	return v1.UniqueVolumeName(
		fmt.Sprintf("%s/%v-%s", volumePlugin.GetPluginName(), podName, volumeSpec.Name()))
}

// GetUniqueVolumeNameFromSpec uses the given VolumePlugin to generate a unique
// name representing the volume defined in the specified volume spec.
// This returned name can be used to uniquely reference the actual backing
// device, directory, path, etc. referenced by the given volumeSpec.
// If the given plugin does not support the volume spec, this returns an error.
func GetUniqueVolumeNameFromSpec(
	volumePlugin volume.VolumePlugin,
	volumeSpec *volume.Spec) (v1.UniqueVolumeName, error) {
	if volumePlugin == nil {
		return "", fmt.Errorf(
			"volumePlugin should not be nil. volumeSpec.Name=%q",
			volumeSpec.Name())
	}

	volumeName, err := volumePlugin.GetVolumeName(volumeSpec)
	if err != nil || volumeName == "" {
		return "", fmt.Errorf(
			"failed to GetVolumeName from volumePlugin for volumeSpec %q err=%v",
			volumeSpec.Name(),
			err)
	}

	return GetUniqueVolumeName(
			volumePlugin.GetPluginName(),
			volumeName),
		nil
}

// IsPodTerminated checks if pod is terminated
func IsPodTerminated(pod *v1.Pod, podStatus v1.PodStatus) bool {
	return podStatus.Phase == v1.PodFailed || podStatus.Phase == v1.PodSucceeded || (pod.DeletionTimestamp != nil && notRunning(podStatus.ContainerStatuses))
}

// notRunning returns true if every status is terminated or waiting, or the status list
// is empty.
func notRunning(statuses []v1.ContainerStatus) bool {
	for _, status := range statuses {
		if status.State.Terminated == nil && status.State.Waiting == nil {
			return false
		}
	}
	return true
}

// SplitUniqueName splits the unique name to plugin name and volume name strings. It expects the uniqueName to follow
// the format plugin_name/volume_name and the plugin name must be namespaced as described by the plugin interface,
// i.e. namespace/plugin containing exactly one '/'. This means the unique name will always be in the form of
// plugin_namespace/plugin/volume_name, see k8s.io/kubernetes/pkg/volume/plugins.go VolumePlugin interface
// description and pkg/volume/util/volumehelper/volumehelper.go GetUniqueVolumeNameFromSpec that constructs
// the unique volume names.
func SplitUniqueName(uniqueName v1.UniqueVolumeName) (string, string, error) {
	components := strings.SplitN(string(uniqueName), "/", 3)
	if len(components) != 3 {
		return "", "", fmt.Errorf("cannot split volume unique name %s to plugin/volume components", uniqueName)
	}
	pluginName := fmt.Sprintf("%s/%s", components[0], components[1])
	return pluginName, components[2], nil
}

// NewSafeFormatAndMountFromHost creates a new SafeFormatAndMount with Mounter
// and Exec taken from given VolumeHost.
func NewSafeFormatAndMountFromHost(pluginName string, host volume.VolumeHost) *mount.SafeFormatAndMount {
	mounter := host.GetMounter(pluginName)
	exec := host.GetExec(pluginName)
	return &mount.SafeFormatAndMount{Interface: mounter, Exec: exec}
}

// GetVolumeMode retrieves VolumeMode from pv.
// If the volume doesn't have PersistentVolume, it's an inline volume,
// should return volumeMode as filesystem to keep existing behavior.
func GetVolumeMode(volumeSpec *volume.Spec) (v1.PersistentVolumeMode, error) {
	if volumeSpec == nil || volumeSpec.PersistentVolume == nil {
		return v1.PersistentVolumeFilesystem, nil
	}
	if volumeSpec.PersistentVolume.Spec.VolumeMode != nil {
		return *volumeSpec.PersistentVolume.Spec.VolumeMode, nil
	}
	return "", fmt.Errorf("cannot get volumeMode for volume: %v", volumeSpec.Name())
}

// GetPersistentVolumeClaimVolumeMode retrieves VolumeMode from pvc.
func GetPersistentVolumeClaimVolumeMode(claim *v1.PersistentVolumeClaim) (v1.PersistentVolumeMode, error) {
	if claim.Spec.VolumeMode != nil {
		return *claim.Spec.VolumeMode, nil
	}
	return "", fmt.Errorf("cannot get volumeMode from pvc: %v", claim.Name)
}

// GetPersistentVolumeClaimQualifiedName returns a qualified name for pvc.
func GetPersistentVolumeClaimQualifiedName(claim *v1.PersistentVolumeClaim) string {
	return utilstrings.JoinQualifiedName(claim.GetNamespace(), claim.GetName())
}

// CheckVolumeModeFilesystem checks VolumeMode.
// If the mode is Filesystem, return true otherwise return false.
func CheckVolumeModeFilesystem(volumeSpec *volume.Spec) (bool, error) {
	if utilfeature.DefaultFeatureGate.Enabled(features.BlockVolume) {
		volumeMode, err := GetVolumeMode(volumeSpec)
		if err != nil {
			return true, err
		}
		if volumeMode == v1.PersistentVolumeBlock {
			return false, nil
		}
	}
	return true, nil
}

// CheckPersistentVolumeClaimModeBlock checks VolumeMode.
// If the mode is Block, return true otherwise return false.
func CheckPersistentVolumeClaimModeBlock(pvc *v1.PersistentVolumeClaim) bool {
	return utilfeature.DefaultFeatureGate.Enabled(features.BlockVolume) && pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode == v1.PersistentVolumeBlock
}

// MakeAbsolutePath convert path to absolute path according to GOOS
func MakeAbsolutePath(goos, path string) string {
	if goos != "windows" {
		return filepath.Clean("/" + path)
	}
	// These are all for windows
	// If there is a colon, give up.
	if strings.Contains(path, ":") {
		return path
	}
	// If there is a slash, but no drive, add 'c:'
	if strings.HasPrefix(path, "/") || strings.HasPrefix(path, "\\") {
		return "c:" + path
	}
	// Otherwise, add 'c:\'
	return "c:\\" + path
}

// MapBlockVolume is a utility function to provide a common way of mounting
// block device path for a specified volume and pod.  This function should be
// called by volume plugins that implements volume.BlockVolumeMapper.Map() method.
func MapBlockVolume(
	devicePath,
	globalMapPath,
	podVolumeMapPath,
	volumeMapName string,
	podUID utypes.UID,
) error {
	blkUtil := volumepathhandler.NewBlockVolumePathHandler()

	// map devicePath to global node path
	mapErr := blkUtil.MapDevice(devicePath, globalMapPath, string(podUID))
	if mapErr != nil {
		return mapErr
	}

	// map devicePath to pod volume path
	mapErr = blkUtil.MapDevice(devicePath, podVolumeMapPath, volumeMapName)
	if mapErr != nil {
		return mapErr
	}

	return nil
}
