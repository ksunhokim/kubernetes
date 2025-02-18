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

package resource

import (
	"fmt"
	"math"
	"strconv"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// addResourceList adds the resources in newList to list
func addResourceList(list, new v1.ResourceList) {
	for name, quantity := range new {
		if value, ok := list[name]; !ok {
			list[name] = *quantity.Copy()
		} else {
			value.Add(quantity)
			list[name] = value
		}
	}
}

// maxResourceList sets list to the greater of list/newList for every resource
// either list
func maxResourceList(list, new v1.ResourceList) {
	for name, quantity := range new {
		if value, ok := list[name]; !ok {
			list[name] = *quantity.Copy()
			continue
		} else {
			if quantity.Cmp(value) > 0 {
				list[name] = *quantity.Copy()
			}
		}
	}
}

// PodRequestsAndLimits returns a dictionary of all defined resources summed up for all
// containers of the pod.
func PodRequestsAndLimits(pod *v1.Pod) (reqs, limits v1.ResourceList) {
	reqs, limits = v1.ResourceList{}, v1.ResourceList{}
	for _, container := range pod.Spec.Containers {
		addResourceList(reqs, container.Resources.Requests)
		addResourceList(limits, container.Resources.Limits)
	}
	// init containers define the minimum of any resource
	for _, container := range pod.Spec.InitContainers {
		maxResourceList(reqs, container.Resources.Requests)
		maxResourceList(limits, container.Resources.Limits)
	}
	return
}

// GetResourceRequest finds and returns the request for a specific resource.
func GetResourceRequest(pod *v1.Pod, resource v1.ResourceName) int64 {
	if resource == v1.ResourcePods {
		return 1
	}
	totalResources := int64(0)
	for _, container := range pod.Spec.Containers {
		if rQuantity, ok := container.Resources.Requests[resource]; ok {
			if resource == v1.ResourceCPU {
				totalResources += rQuantity.MilliValue()
			} else {
				totalResources += rQuantity.Value()
			}
		}
	}
	// take max_resource(sum_pod, any_init_container)
	for _, container := range pod.Spec.InitContainers {
		if rQuantity, ok := container.Resources.Requests[resource]; ok {
			if resource == v1.ResourceCPU && rQuantity.MilliValue() > totalResources {
				totalResources = rQuantity.MilliValue()
			} else if rQuantity.Value() > totalResources {
				totalResources = rQuantity.Value()
			}
		}
	}
	return totalResources
}

// ExtractResourceValueByContainerName extracts the value of a resource
// by providing container name
func ExtractResourceValueByContainerName(fs *v1.ResourceFieldSelector, pod *v1.Pod, containerName string) (string, error) {
	container, err := findContainerInPod(pod, containerName)
	if err != nil {
		return "", err
	}
	return ExtractContainerResourceValue(fs, container)
}

// ExtractResourceValueByContainerNameAndNodeAllocatable extracts the value of a resource
// by providing container name and node allocatable
func ExtractResourceValueByContainerNameAndNodeAllocatable(fs *v1.ResourceFieldSelector, pod *v1.Pod, containerName string, nodeAllocatable v1.ResourceList) (string, error) {
	realContainer, err := findContainerInPod(pod, containerName)
	if err != nil {
		return "", err
	}

	container := realContainer.DeepCopy()

	MergeContainerResourceLimits(container, nodeAllocatable)

	return ExtractContainerResourceValue(fs, container)
}

// ExtractContainerResourceValue extracts the value of a resource
// in an already known container
func ExtractContainerResourceValue(fs *v1.ResourceFieldSelector, container *v1.Container) (string, error) {
	divisor := resource.Quantity{}
	if divisor.Cmp(fs.Divisor) == 0 {
		divisor = resource.MustParse("1")
	} else {
		divisor = fs.Divisor
	}

	switch fs.Resource {
	case "limits.cpu":
		return convertResourceCPUToString(container.Resources.Limits.Cpu(), divisor)
	case "limits.memory":
		return convertResourceMemoryToString(container.Resources.Limits.Memory(), divisor)
	case "limits.ephemeral-storage":
		return convertResourceEphemeralStorageToString(container.Resources.Limits.StorageEphemeral(), divisor)
	case "requests.cpu":
		return convertResourceCPUToString(container.Resources.Requests.Cpu(), divisor)
	case "requests.memory":
		return convertResourceMemoryToString(container.Resources.Requests.Memory(), divisor)
	case "requests.ephemeral-storage":
		return convertResourceEphemeralStorageToString(container.Resources.Requests.StorageEphemeral(), divisor)
	}

	return "", fmt.Errorf("unsupported container resource : %v", fs.Resource)
}

// convertResourceCPUToString converts cpu value to the format of divisor and returns
// ceiling of the value.
func convertResourceCPUToString(cpu *resource.Quantity, divisor resource.Quantity) (string, error) {
	c := int64(math.Ceil(float64(cpu.MilliValue()) / float64(divisor.MilliValue())))
	return strconv.FormatInt(c, 10), nil
}

// convertResourceMemoryToString converts memory value to the format of divisor and returns
// ceiling of the value.
func convertResourceMemoryToString(memory *resource.Quantity, divisor resource.Quantity) (string, error) {
	m := int64(math.Ceil(float64(memory.Value()) / float64(divisor.Value())))
	return strconv.FormatInt(m, 10), nil
}

// convertResourceEphemeralStorageToString converts ephemeral storage value to the format of divisor and returns
// ceiling of the value.
func convertResourceEphemeralStorageToString(ephemeralStorage *resource.Quantity, divisor resource.Quantity) (string, error) {
	m := int64(math.Ceil(float64(ephemeralStorage.Value()) / float64(divisor.Value())))
	return strconv.FormatInt(m, 10), nil
}

// findContainerInPod finds a container by its name in the provided pod
func findContainerInPod(pod *v1.Pod, containerName string) (*v1.Container, error) {
	for _, container := range pod.Spec.Containers {
		if container.Name == containerName {
			return &container, nil
		}
	}
	return nil, fmt.Errorf("container %s not found", containerName)
}

// MergeContainerResourceLimits checks if a limit is applied for
// the container, and if not, it sets the limit to the passed resource list.
func MergeContainerResourceLimits(container *v1.Container,
	allocatable v1.ResourceList) {
	if container.Resources.Limits == nil {
		container.Resources.Limits = make(v1.ResourceList)
	}
	for _, resource := range []v1.ResourceName{v1.ResourceCPU, v1.ResourceMemory, v1.ResourceEphemeralStorage} {
		if quantity, exists := container.Resources.Limits[resource]; !exists || quantity.IsZero() {
			if cap, exists := allocatable[resource]; exists {
				container.Resources.Limits[resource] = *cap.Copy()
			}
		}
	}
}
